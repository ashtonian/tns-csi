package performance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
)

const (
	// WitnessFileVersion identifies the append-file header format.
	WitnessFileVersion = 1
	// WitnessHeaderBytes precede each payload and are excluded from verified bytes.
	WitnessHeaderBytes = 1024

	witnessHeaderPrefix    = 64
	maxWitnessPayloadBytes = 32 << 20
	witnessFileMagic       = "TNSWIT01"
)

// WitnessFilePlan bounds one append file; it does not establish source identity
// or admit writes. The caller owns exclusive access and the target/fault scope.
// Writer memory includes a private payload copy of at most MaxPayloadBytes.
type WitnessFilePlan struct {
	Stream          WitnessStream `json:"stream"`
	CapacityBytes   uint64        `json:"capacity_bytes"`
	MaxPayloadBytes uint64        `json:"max_payload_bytes"`
	MaxRecords      uint64        `json:"max_records"`
}

// WitnessFileSource borrows a caller-identified read-only file for recovery.
type WitnessFileSource interface {
	io.ReaderAt
	Stat() (os.FileInfo, error)
}

// WitnessFileResult adds actual read counts and the unverified suffix size to
// prefix reconciliation. Tail bytes include headers and incomplete records.
type WitnessFileResult struct {
	UnwitnessedTailBytes *uint64 `json:"unwitnessed_tail_bytes"`
	WitnessResult
	ReadBytes uint64 `json:"read_bytes"`
}

func validWitnessFilePlan(plan WitnessFilePlan) bool {
	return validWitnessStream(plan.Stream) && plan.CapacityBytes > WitnessHeaderBytes && plan.CapacityBytes <= math.MaxInt64 &&
		plan.MaxPayloadBytes > 0 && plan.MaxPayloadBytes <= maxWitnessPayloadBytes &&
		plan.MaxPayloadBytes <= plan.CapacityBytes-WitnessHeaderBytes && plan.MaxRecords > 0 && plan.MaxRecords <= maxWitnessRecords
}

func encodeWitnessHeader(record WitnessRecord) ([]byte, error) {
	if !validWitnessStream(record.Stream) || !validWitnessRecord(record.Stream, record) || record.OffsetBytes < WitnessHeaderBytes {
		return nil, ErrInvalidWitness
	}
	body, err := json.Marshal(record)
	if err != nil || len(body) > WitnessHeaderBytes-witnessHeaderPrefix {
		return nil, ErrInvalidWitness
	}
	header := make([]byte, WitnessHeaderBytes)
	copy(header, witnessFileMagic)
	binary.BigEndian.PutUint64(header[8:16], WitnessFileVersion)
	binary.BigEndian.PutUint64(header[16:24], uint64(len(body)))
	hash := sha256.Sum256(body)
	copy(header[24:56], hash[:])
	copy(header[witnessHeaderPrefix:], body)
	return header, nil
}

func decodeWitnessHeader(header []byte) (WitnessRecord, error) {
	if len(header) != WitnessHeaderBytes || string(header[:8]) != witnessFileMagic ||
		binary.BigEndian.Uint64(header[8:16]) != WitnessFileVersion || !zeroWitnessPadding(header[56:witnessHeaderPrefix]) {

		return WitnessRecord{}, ErrEvidenceCorrupt
	}
	size := binary.BigEndian.Uint64(header[16:24])
	if size == 0 || size > WitnessHeaderBytes-witnessHeaderPrefix {
		return WitnessRecord{}, ErrEvidenceCorrupt
	}
	body := header[witnessHeaderPrefix : witnessHeaderPrefix+size]
	hash := sha256.Sum256(body)
	if !bytes.Equal(hash[:], header[24:56]) || !zeroWitnessPadding(header[witnessHeaderPrefix+size:]) {
		return WitnessRecord{}, ErrEvidenceCorrupt
	}
	record, err := decodeRecord[WitnessRecord](body, WitnessHeaderBytes-witnessHeaderPrefix)
	if err != nil || !validWitnessStream(record.Stream) || !validWitnessRecord(record.Stream, record) {
		return WitnessRecord{}, ErrEvidenceCorrupt
	}
	return record, nil
}

func zeroWitnessPadding(raw []byte) bool {
	for _, value := range raw {
		if value != 0 {
			return false
		}
	}
	return true
}

// VerifyWitnessFile authenticates the ledger before reading required headers and
// complete payloads. Observations come from those reads, never copied expectations.
// It opens/closes no files, performs no writes and does not inspect suffix data.
// The caller owns read-only opening, source identity, exclusion and receipt trust.
// Cancellation is checked between calls and cannot interrupt a blocked ReaderAt.
func VerifyWitnessFile(ctx context.Context, plan WitnessFilePlan, source WitnessFileSource, receipt ArtifactReceipt, raw []byte, replies []WitnessRecord) WitnessFileResult {
	result := WitnessFileResult{WitnessResult: WitnessResult{Status: Fail}}
	if ctx == nil || !validWitnessFilePlan(plan) || source == nil {
		result.reject("INVALID_REQUEST", nil)
		return result
	}
	ledger, code := authenticatedWitness(plan.Stream, receipt, raw)
	if code != "" {
		result.reject(code, nil)
		return result
	}
	prefixEnd, valid := witnessFilePrefix(plan, ledger)
	if !valid {
		result.reject("INVALID_REQUEST", nil)
		return result
	}
	if !validWitnessFileReplies(plan.Stream, ledger, replies) {
		result.reject(reasonWitnessConflict, nil)
		return result
	}
	// Pin caller-owned inputs before invoking any source callbacks.
	raw = bytes.Clone(raw)
	replies = append([]WitnessRecord(nil), replies...)
	if ctx.Err() != nil {
		result.reject(reasonProcessError, nil)
		return result
	}
	before, err := source.Stat()
	if err != nil || before == nil || !before.Mode().IsRegular() {
		result.reject(reasonEvidenceIncomplete, nil)
		return result
	}
	fileSize, modified := before.Size(), before.ModTime()
	if fileSize < 0 || uint64(fileSize) > plan.CapacityBytes {
		result.reject(reasonEvidenceIncomplete, nil)
		return result
	}
	observed := make([]WitnessRecord, 0, len(ledger.Records))
	var findings []WitnessFinding
	buffer := make([]byte, min(plan.MaxPayloadBytes, 64<<10))
	for _, expected := range ledger.Records {
		if ctx.Err() != nil {
			break
		}
		actual, readBytes, readCode := readWitnessFileRecord(ctx, source, expected, buffer)
		result.ReadBytes += readBytes
		if readCode != "" {
			findings = append(findings, WitnessFinding{Code: readCode, Sequence: &expected.Sequence})
			continue
		}
		observed = append(observed, actual)
	}
	result.WitnessResult = ReconcileWitness(plan.Stream, receipt, raw, observed, replies)
	for _, finding := range findings {
		result.reject(finding.Code, finding.Sequence)
	}
	if ctx.Err() != nil {
		result.reject(reasonProcessError, nil)
		result.VerifiedBytes = nil
		return result
	}
	after, statErr := source.Stat()
	if statErr != nil || after == nil || !after.Mode().IsRegular() || after.Size() != fileSize || !after.ModTime().Equal(modified) {
		result.reject(reasonEvidenceIncomplete, nil)
	} else if uint64(fileSize) >= prefixEnd {
		tail := uint64(fileSize) - prefixEnd
		result.UnwitnessedTailBytes = &tail
	}
	if ctx.Err() != nil {
		result.reject(reasonProcessError, nil)
	}
	if result.Status != Pass {
		result.VerifiedBytes = nil
	}
	return result
}

func witnessFilePrefix(plan WitnessFilePlan, ledger WitnessLedger) (uint64, bool) {
	if uint64(len(ledger.Records)) > plan.MaxRecords {
		return 0, false
	}
	var end uint64
	for _, record := range ledger.Records {
		headerEnd, valid := add(end, WitnessHeaderBytes)
		if !valid || record.OffsetBytes != headerEnd || record.PayloadBytes > plan.MaxPayloadBytes {
			return 0, false
		}
		end, valid = add(record.OffsetBytes, record.PayloadBytes)
		if !valid || end > plan.CapacityBytes {
			return 0, false
		}
	}
	return end, true
}

func validWitnessFileReplies(stream WitnessStream, ledger WitnessLedger, replies []WitnessRecord) bool {
	indexed, finding := witnessRecords(stream, replies, true)
	if finding != nil {
		return false
	}
	for sequence, record := range indexed {
		if sequence > ledger.FinalSequence || record != ledger.Records[sequence-1] {
			return false
		}
	}
	return true
}

func readWitnessFileRecord(ctx context.Context, source io.ReaderAt, expected WitnessRecord, buffer []byte) (record WitnessRecord, readBytes uint64, code string) {
	header := make([]byte, WitnessHeaderBytes)
	n, err := readWitnessBytes(source, header, expected.OffsetBytes-WitnessHeaderBytes)
	readBytes += n
	if err != nil {
		return WitnessRecord{}, readBytes, reasonEvidenceIncomplete
	}
	record, err = decodeWitnessHeader(header)
	if err != nil {
		return WitnessRecord{}, readBytes, reasonEvidenceIncomplete
	}
	if record != expected {
		return WitnessRecord{}, readBytes, reasonWitnessConflict
	}
	digest := sha256.New()
	var payloadRead uint64
	for payloadRead < record.PayloadBytes {
		if ctx.Err() != nil {
			return WitnessRecord{}, readBytes, reasonProcessError
		}
		chunk := buffer[:min(uint64(len(buffer)), record.PayloadBytes-payloadRead)]
		n, err = readWitnessBytes(source, chunk, record.OffsetBytes+payloadRead)
		readBytes += n
		if err != nil {
			return WitnessRecord{}, readBytes, reasonEvidenceIncomplete
		}
		if _, hashErr := digest.Write(chunk); hashErr != nil {
			return WitnessRecord{}, readBytes, reasonEvidenceIncomplete
		}
		payloadRead += n
	}
	if ctx.Err() != nil {
		return WitnessRecord{}, readBytes, reasonProcessError
	}
	if hex.EncodeToString(digest.Sum(nil)) != record.SHA256 {
		return WitnessRecord{}, readBytes, reasonChecksumMismatch
	}
	return record, readBytes, ""
}

func readWitnessBytes(source io.ReaderAt, buffer []byte, offset uint64) (uint64, error) {
	if offset > math.MaxInt64 {
		return 0, ErrEvidenceIncomplete
	}
	n, err := source.ReadAt(buffer, int64(offset))
	if n < 0 || n > len(buffer) {
		return 0, ErrEvidenceIncomplete
	}
	if n != len(buffer) || err != nil && !errors.Is(err, io.EOF) {
		return uint64(n), ErrEvidenceIncomplete
	}
	return uint64(n), nil
}
