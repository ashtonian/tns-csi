package performance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
)

const (
	// EvidenceVersion identifies the capture format and evaluation rules.
	EvidenceVersion = 2

	artifactManifest = "manifest"
	artifactStdout   = "stdout"
	artifactStderr   = "stderr"
	artifactProcess  = "process"
	artifactResult   = "result"
)

var (
	// ErrEvidenceExists prevents reuse or replacement of an existing run's files.
	ErrEvidenceExists = errors.New("performance evidence already exists")
	// ErrEvidenceIncomplete reports persistence failures without exposing file contents.
	ErrEvidenceIncomplete = errors.New("performance evidence incomplete")
	// ErrEvidenceClosed rejects writes after a store has been closed.
	ErrEvidenceClosed = errors.New("performance evidence closed")
	// ErrCaptureRecorded rejects a second capture, including after a failed attempt.
	ErrCaptureRecorded = errors.New("performance capture already recorded")
	// ErrEvidenceRoot requires a caller-selected private evidence directory handle.
	ErrEvidenceRoot = errors.New("performance evidence requires a private directory")
)

// ArtifactReceipt is returned only after a complete file write and directory sync.
type ArtifactReceipt struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  uint64 `json:"bytes"`
}

// CapturedProcess contains already captured fio streams; no command is executed.
type CapturedProcess struct {
	Process ProcessReceipt
	Stdout  []byte
	Stderr  []byte
}

// CaptureResult includes the final index receipt separately to avoid a self hash.
type CaptureResult struct {
	Index         *ArtifactReceipt  `json:"index"`
	Artifacts     []ArtifactReceipt `json:"artifacts"`
	Evaluation    Evaluation        `json:"evaluation"`
	SchemaVersion uint64            `json:"schema_version"`
}

// EvidenceStore records one capture in a caller-owned private directory.
// The directory must be outside the measured storage and selected by the caller.
// It never removes files, retries a capture or supervises a process.
type EvidenceStore struct {
	root     *os.Root
	started  ArtifactReceipt
	manifest Manifest
	mu       sync.Mutex
	recorded bool
	closed   bool
}

// NewEvidenceStore reserves a run with an exclusive manifest file. It duplicates
// the supplied directory handle; closing the store does not close the caller's root.
func NewEvidenceStore(root *os.Root, manifest Manifest) (*EvidenceStore, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, ErrEvidenceRoot
	}
	private, err := root.OpenRoot(".")
	if err != nil {
		return nil, ErrEvidenceRoot
	}
	manifest.Jobs = append([]JobPlan(nil), manifest.Jobs...)
	store := &EvidenceStore{root: private, manifest: manifest}
	if err := store.reserve(); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	return store, nil
}

func (s *EvidenceStore) reserve() error {
	info, err := s.root.Stat(".")
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return ErrEvidenceRoot
	}
	for _, kind := range []string{artifactManifest, artifactStdout, artifactStderr, artifactProcess, artifactResult} {
		_, statErr := s.root.Lstat(s.filename(kind))
		if statErr == nil {
			return ErrEvidenceExists
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return ErrEvidenceIncomplete
		}
	}
	raw, err := json.Marshal(s.manifest)
	if err != nil {
		return ErrEvidenceIncomplete
	}
	if len(raw) > maxManifestBytes {
		return ErrInvalidManifest
	}
	s.started, err = s.persist(artifactManifest, raw)
	return err
}

// RecordCapture persists both raw streams and the process receipt even when fio
// evaluation fails. Persistence failure forces FAIL and never permits a retry.
// Raw streams must already be credential-free; API responses are not accepted here.
func (s *EvidenceStore) RecordCapture(capture CapturedProcess) (CaptureResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return failedCapture(), ErrEvidenceClosed
	}
	if s.recorded {
		return failedCapture(), ErrCaptureRecorded
	}
	s.recorded = true
	result := CaptureResult{SchemaVersion: EvidenceVersion, Evaluation: EvaluateFio(s.manifest, capture.Process, capture.Stdout)}
	result.Artifacts = append(result.Artifacts, s.started)
	failed := len(capture.Stdout) > maxFioBytes || len(capture.Stderr) > maxFioBytes
	for _, stream := range []struct {
		kind string
		data []byte
	}{{kind: artifactStdout, data: capture.Stdout}, {kind: artifactStderr, data: capture.Stderr}} {
		receipt, err := s.persist(stream.kind, stream.data)
		if err != nil {
			failed = true
			continue
		}
		result.Artifacts = append(result.Artifacts, receipt)
	}
	// Signal names are the receipt's only free-form process string.
	if !validSignal(capture.Process.Signal) {
		capture.Process.Signal = "unknown"
		failed = true
	}
	process, err := json.Marshal(capture.Process)
	if err != nil {
		failed = true
	} else if receipt, persistErr := s.persist(artifactProcess, process); persistErr != nil {
		failed = true
	} else {
		result.Artifacts = append(result.Artifacts, receipt)
	}
	if failed {
		result.Evaluation.reject("EVIDENCE_INCOMPLETE", nil)
	}
	index, err := json.Marshal(result)
	if err != nil {
		return failedResult(result), ErrEvidenceIncomplete
	}
	if len(index) > maxIndexBytes {
		return failedResult(result), ErrEvidenceIncomplete
	}
	receipt, err := s.persist(artifactResult, index)
	if err != nil {
		return failedResult(result), err
	}
	result.Index = &receipt
	if failed {
		return result, ErrEvidenceIncomplete
	}
	return result, nil
}

// Close releases only the store's descriptor. Evidence files remain intact.
func (s *EvidenceStore) Close() error {
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

func (s *EvidenceStore) filename(kind string) string {
	extension := ".json"
	if kind == artifactStdout || kind == artifactStderr {
		extension = ".raw"
	}
	return s.manifest.RunID + "." + kind + extension
}

func (s *EvidenceStore) persist(kind string, data []byte) (ArtifactReceipt, error) {
	name := s.filename(kind)
	// A single component plus O_EXCL refuses existing files and final symlinks.
	file, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return ArtifactReceipt{}, ErrEvidenceExists
		}
		return ArtifactReceipt{}, ErrEvidenceIncomplete
	}
	if err := writeSynced(file, data); err != nil {
		return ArtifactReceipt{}, err
	}
	if err := s.syncDirectory(); err != nil {
		return ArtifactReceipt{}, err
	}
	hash := sha256.Sum256(data)
	return ArtifactReceipt{Name: name, SHA256: hex.EncodeToString(hash[:]), Bytes: uint64(len(data))}, nil
}

func (s *EvidenceStore) syncDirectory() error {
	dir, err := s.root.Open(".")
	if err != nil {
		return ErrEvidenceIncomplete
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil || closeErr != nil {
		return ErrEvidenceIncomplete
	}
	return nil
}

type durableFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

func writeSynced(file durableFile, data []byte) error {
	n, writeErr := file.Write(data)
	var syncErr error
	if writeErr == nil && n == len(data) {
		syncErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || n != len(data) || syncErr != nil || closeErr != nil {
		return ErrEvidenceIncomplete
	}
	return nil
}

func validSignal(signal string) bool {
	switch signal {
	case "", "SIGHUP", "SIGINT", "SIGQUIT", "SIGILL", "SIGABRT", "SIGFPE", "SIGKILL", "SIGSEGV", "SIGPIPE", "SIGALRM", "SIGTERM", "SIGBUS", "SIGXCPU", "SIGXFSZ":
		return true
	default:
		return false
	}
}

func failedCapture() CaptureResult {
	return failedResult(CaptureResult{})
}

func failedResult(result CaptureResult) CaptureResult {
	result.SchemaVersion = EvidenceVersion
	result.Evaluation.reject("EVIDENCE_INCOMPLETE", nil)
	return result
}
