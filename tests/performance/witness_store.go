package performance

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
)

const (
	maxWitnessEntryBytes       = 2 << 10
	maxWitnessDirectoryEntries = 16384
)

var (
	// ErrInvalidWitness rejects an invalid stream, record, sequence or format budget.
	ErrInvalidWitness = errors.New("invalid performance witness request")
	// ErrWitnessConflict rejects a repeated sequence with different contents.
	ErrWitnessConflict = errors.New("performance witness record conflict")
	// ErrWitnessFailed prevents continuation after a rejected or incomplete commit.
	ErrWitnessFailed = errors.New("performance witness store failed")
	// ErrWitnessSealed rejects appends after finalization begins.
	ErrWitnessSealed = errors.New("performance witness generation sealed")
)

type witnessStart struct {
	Stream        WitnessStream `json:"stream"`
	SchemaVersion uint64        `json:"schema_version"`
}

type witnessEntry struct {
	Record        WitnessRecord `json:"record"`
	SchemaVersion uint64        `json:"schema_version"`
}

type witnessCommit struct {
	receipt ArtifactReceipt
	record  WitnessRecord
}

// WitnessStore durably records one generation in a caller-selected private
// directory outside the target and fault domains. Only this instance may append;
// it never resumes an existing run, rewrites evidence or admits a storage fault.
// The caller must establish target fsync success before submitting each record.
type WitnessStore struct {
	root         *os.Root
	final        *ArtifactReceipt
	commits      []witnessCommit
	started      ArtifactReceipt
	stream       WitnessStream
	mu           sync.Mutex
	encodedBytes int
	failed       bool
	sealed       bool
	closed       bool
}

// NewWitnessStore reserves a fresh run before accepting commits. It owns a
// duplicate of root; closing either descriptor does not close the other.
// Linux and Darwin provide the required descriptor-relative no-follow reads.
func NewWitnessStore(root *os.Root, stream WitnessStream) (*WitnessStore, error) {
	if !validWitnessStream(stream) {
		return nil, ErrInvalidWitness
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return nil, ErrEvidencePlatform
	}
	if root == nil {
		return nil, ErrEvidenceRoot
	}
	private, err := root.OpenRoot(".")
	if err != nil {
		return nil, ErrEvidenceRoot
	}
	store := &WitnessStore{root: private, stream: stream}
	if err := store.reserve(); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	return store, nil
}

func (s *WitnessStore) reserve() error {
	names, err := witnessFiles(s.root, s.stream.RunID)
	if err != nil {
		return err
	}
	if len(names) != 0 {
		return ErrEvidenceExists
	}
	base, err := json.Marshal(WitnessLedger{Records: []WitnessRecord{}, Stream: s.stream, FinalSequence: maxWitnessRecords, SchemaVersion: WitnessVersion})
	if err != nil {
		return ErrInvalidWitness
	}
	// Reserve the largest final-sequence encoding and one delimiter per record.
	s.encodedBytes = len(base)
	raw, err := json.Marshal(witnessStart{Stream: s.stream, SchemaVersion: WitnessVersion})
	if err != nil || len(raw) > maxWitnessEntryBytes {
		return ErrInvalidWitness
	}
	s.started, err = persistArtifact(s.root, s.stream.RunID+".witness.start.json", raw)
	return err
}

// Append returns a receipt only after exclusive write, file sync/close and
// directory sync. An identical retry verifies the original artifact and returns
// its receipt without another write. Conflicts, invalid records and persistence
// failures permanently prevent further commits or finalization by this instance.
func (s *WitnessStore) Append(record WitnessRecord) (ArtifactReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendable(); err != nil {
		return ArtifactReceipt{}, err
	}
	if !validWitnessRecord(s.stream, record) {
		return s.fail(ErrInvalidWitness)
	}
	if record.Sequence <= uint64(len(s.commits)) {
		prior := s.commits[record.Sequence-1]
		if prior.record != record {
			return s.fail(ErrWitnessConflict)
		}
		if _, err := readWitnessArtifact(s.root, prior.receipt, maxWitnessEntryBytes); err != nil {
			return s.fail(ErrEvidenceIncomplete)
		}
		return prior.receipt, nil
	}
	if record.Sequence != uint64(len(s.commits))+1 || len(s.commits) >= maxWitnessRecords {
		return s.fail(ErrInvalidWitness)
	}
	if len(s.commits) > 0 {
		prior := s.commits[len(s.commits)-1].record
		if record.OffsetBytes < prior.OffsetBytes+prior.PayloadBytes {
			return s.fail(ErrInvalidWitness)
		}
	}
	recordJSON, err := json.Marshal(record)
	if err != nil || len(recordJSON)+1 > maxWitnessBytes-s.encodedBytes {
		return s.fail(ErrInvalidWitness)
	}
	raw, err := json.Marshal(witnessEntry{Record: record, SchemaVersion: WitnessVersion})
	if err != nil || len(raw) > maxWitnessEntryBytes {
		return s.fail(ErrInvalidWitness)
	}
	if _, readErr := readWitnessArtifact(s.root, s.started, maxWitnessEntryBytes); readErr != nil {
		return s.fail(ErrEvidenceIncomplete)
	}
	receipt, err := persistArtifact(s.root, witnessEntryName(s.stream.RunID, record.Sequence), raw)
	if err != nil {
		return s.fail(err)
	}
	s.commits = append(s.commits, witnessCommit{record: record, receipt: receipt})
	s.encodedBytes += len(recordJSON) + 1
	return receipt, nil
}

func (s *WitnessStore) appendable() error {
	switch {
	case s.closed:
		return ErrEvidenceClosed
	case s.failed:
		return ErrWitnessFailed
	case s.sealed:
		return ErrWitnessSealed
	default:
		return nil
	}
}

func (s *WitnessStore) fail(err error) (ArtifactReceipt, error) {
	s.failed = true
	return ArtifactReceipt{}, err
}

// Finalize seals the generation before checking all committed artifacts and
// exclusively persisting its nonempty ledger. Accepted Append calls are included
// even if the caller lost their replies. A successful repeated call verifies and
// returns the original receipt; any failed attempt forbids automatic retries.
// The caller must drain the target writer and account for unsubmitted requests.
func (s *WitnessStore) Finalize() (ArtifactReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ArtifactReceipt{}, ErrEvidenceClosed
	}
	if s.failed {
		return ArtifactReceipt{}, ErrWitnessFailed
	}
	s.sealed = true
	if s.final != nil {
		if _, err := LoadWitnessLedger(s.root, s.stream, *s.final); err != nil {
			return s.fail(ErrEvidenceIncomplete)
		}
		return *s.final, nil
	}
	if len(s.commits) == 0 {
		return s.fail(ErrInvalidWitness)
	}
	if err := s.verifyCommits(); err != nil {
		return s.fail(err)
	}
	ledger := WitnessLedger{Stream: s.stream, SchemaVersion: WitnessVersion, FinalSequence: uint64(len(s.commits))}
	for _, commit := range s.commits {
		ledger.Records = append(ledger.Records, commit.record)
	}
	raw, err := json.Marshal(ledger)
	if err != nil || len(raw) > maxWitnessBytes {
		return s.fail(ErrEvidenceIncomplete)
	}
	receipt, err := persistArtifact(s.root, s.stream.RunID+".witness.json", raw)
	if err != nil {
		return s.fail(err)
	}
	s.final = &receipt
	return receipt, nil
}

func (s *WitnessStore) verifyCommits() error {
	names, err := witnessFiles(s.root, s.stream.RunID)
	if err != nil || len(names) != len(s.commits)+1 || !names[s.started.Name] {
		return ErrEvidenceIncomplete
	}
	if _, err := readWitnessArtifact(s.root, s.started, maxWitnessEntryBytes); err != nil {
		return ErrEvidenceIncomplete
	}
	for _, commit := range s.commits {
		if !names[commit.receipt.Name] {
			return ErrEvidenceIncomplete
		}
		if _, err := readWitnessArtifact(s.root, commit.receipt, maxWitnessEntryBytes); err != nil {
			return ErrEvidenceIncomplete
		}
	}
	return nil
}

// Close releases the owned descriptor without finalizing, deleting or resuming.
func (s *WitnessStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.root.Close(); err != nil {
		return ErrEvidenceIncomplete
	}
	return nil
}

func witnessEntryName(runID string, sequence uint64) string {
	return fmt.Sprintf("%s.witness.%020d.json", runID, sequence)
}

// LoadWitnessLedger reads and authenticates a finalized ledger without modifying
// evidence. The receipt and expected stream must arrive through a trusted path.
// It does not resume an abandoned store or establish target/collector identity.
func LoadWitnessLedger(root *os.Root, stream WitnessStream, receipt ArtifactReceipt) ([]byte, error) {
	if !validWitnessStream(stream) {
		return nil, ErrInvalidWitness
	}
	if receipt.Bytes == 0 || !validReceipt(receipt, stream.RunID+".witness.json", maxWitnessBytes) {
		return nil, ErrEvidenceCorrupt
	}
	raw, err := readWitnessArtifact(root, receipt, maxWitnessBytes)
	if err != nil {
		return nil, err
	}
	if _, code := authenticatedWitness(stream, receipt, raw); code != "" {
		return nil, ErrEvidenceCorrupt
	}
	return raw, nil
}

func readWitnessArtifact(root *os.Root, receipt ArtifactReceipt, limit int) ([]byte, error) {
	directory, err := witnessDirectory(root)
	if err != nil {
		return nil, err
	}
	raw, readErr := readArtifact(directory, receipt, limit)
	if closeErr := directory.Close(); closeErr != nil {
		return nil, ErrEvidenceCorrupt
	}
	return raw, readErr
}

func witnessDirectory(root *os.Root) (*os.File, error) {
	if root == nil {
		return nil, ErrEvidenceRoot
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, ErrEvidenceRoot
	}
	info, statErr := directory.Stat()
	if statErr != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		if closeErr := directory.Close(); closeErr != nil {
			return nil, ErrEvidenceIncomplete
		}
		return nil, ErrEvidenceRoot
	}
	return directory, nil
}

func witnessFiles(root *os.Root, runID string) (names map[string]bool, err error) {
	directory, err := witnessDirectory(root)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := directory.Close(); closeErr != nil {
			names, err = nil, ErrEvidenceIncomplete
		}
	}()
	names = make(map[string]bool)
	for seen := 0; ; {
		entries, readErr := directory.ReadDir(128)
		seen += len(entries)
		if seen > maxWitnessDirectoryEntries || readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, ErrEvidenceIncomplete
		}
		for _, entry := range entries {
			name := entry.Name()
			if name == runID+".witness.json" || strings.HasPrefix(name, runID+".witness.") {
				names[name] = true
			}
		}
		if errors.Is(readErr, io.EOF) {
			return names, nil
		}
	}
}
