package performance

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
)

const (
	// WitnessVersion identifies the finalized witness comparison format.
	WitnessVersion = 1

	maxWitnessBytes       = 2 << 20
	maxWitnessRecords     = 4096
	reasonWitnessConflict = "WITNESS_CONFLICT"
)

// WitnessStream identifies one run's single append stream and writer generation.
type WitnessStream struct {
	RunID      string `json:"run_id"`
	StreamID   string `json:"stream_id"`
	Generation uint64 `json:"generation"`
}

// WitnessRecord binds a sequence to the complete payload's location and SHA-256.
// Recovered records must come from independently read and hashed target payloads.
type WitnessRecord struct {
	SHA256       string        `json:"sha256"`
	Stream       WitnessStream `json:"stream"`
	Sequence     uint64        `json:"sequence"`
	OffsetBytes  uint64        `json:"offset_bytes"`
	PayloadBytes uint64        `json:"payload_bytes"`
}

// WitnessLedger represents a finalized, nonempty durable prefix. Its trusted
// receipt must be issued only after commits are reconciled and the generation is
// closed to appends. This type and its comparison do not perform finalization.
type WitnessLedger struct {
	Records       []WitnessRecord `json:"records"`
	Stream        WitnessStream   `json:"stream"`
	FinalSequence uint64          `json:"final_sequence"`
	SchemaVersion uint64          `json:"schema_version"`
}

// WitnessFinding identifies a failed comparison without echoing record contents.
type WitnessFinding struct {
	Sequence *uint64 `json:"sequence,omitempty"`
	Code     string  `json:"code"`
}

// WitnessResult counts the entire durable prefix, independently of reply delivery.
// Unwitnessed tail sequences never contribute verified bytes or durability claims.
type WitnessResult struct {
	VerifiedBytes    *uint64          `json:"verified_bytes"`
	Status           Status           `json:"status"`
	Findings         []WitnessFinding `json:"findings"`
	TailSequences    []uint64         `json:"tail_sequences"`
	WitnessedRecords uint64           `json:"witnessed_records"`
	VerifiedRecords  uint64           `json:"verified_records"`
	RepliedRecords   uint64           `json:"replied_records"`
}

// ReconcileWitness authenticates a finalized ledger against an independently
// trusted receipt, then compares recovered records and writer-observed replies.
// Identical reply retries count once; every durable entry is required even if its
// reply was lost. The caller owns collector trust, actual read/hash completeness,
// durable finalization and source identity. No I/O, recovery or admission occurs.
func ReconcileWitness(stream WitnessStream, receipt ArtifactReceipt, raw []byte, recovered, replies []WitnessRecord) WitnessResult {
	result := WitnessResult{Status: Pass}
	if !validWitnessStream(stream) {
		result.reject("INVALID_REQUEST", nil)
		return result
	}
	ledger, code := authenticatedWitness(stream, receipt, raw)
	if code != "" {
		result.reject(code, nil)
		return result
	}
	observed, finding := witnessRecords(stream, recovered, false)
	if finding != nil {
		result.reject(finding.Code, finding.Sequence)
		return result
	}
	acknowledged, finding := witnessRecords(stream, replies, true)
	if finding != nil {
		result.reject(finding.Code, finding.Sequence)
		return result
	}
	result.WitnessedRecords = ledger.FinalSequence
	var total uint64
	for _, record := range ledger.Records {
		actual, found := observed[record.Sequence]
		switch {
		case !found:
			result.reject("COVERAGE_INCOMPLETE", &record.Sequence)
		case actual != record:
			actual.SHA256 = record.SHA256
			findingCode := reasonWitnessConflict
			if actual == record {
				findingCode = "CHECKSUM_MISMATCH"
			}
			result.reject(findingCode, &record.Sequence)
		default:
			result.VerifiedRecords++
			// Validated prefix extents do not overlap or overflow their byte range.
			total += record.PayloadBytes
		}
		if reply, ok := acknowledged[record.Sequence]; ok {
			if reply != record {
				result.reject(reasonWitnessConflict, &record.Sequence)
			} else {
				result.RepliedRecords++
			}
		}
	}
	for _, sequence := range sortedWitnessSequences(observed) {
		if sequence > ledger.FinalSequence {
			result.TailSequences = append(result.TailSequences, sequence)
		}
	}
	for _, sequence := range sortedWitnessSequences(acknowledged) {
		if sequence > ledger.FinalSequence {
			result.reject(reasonWitnessConflict, &sequence)
		}
	}
	if !validRecoveredOrder(ledger, observed) {
		result.reject(reasonWitnessConflict, nil)
	}
	if result.Status == Pass {
		result.VerifiedBytes = &total
	}
	return result
}

func (r *WitnessResult) reject(code string, sequence *uint64) {
	r.Status = Fail
	r.Findings = append(r.Findings, WitnessFinding{Code: code, Sequence: sequence})
}

func authenticatedWitness(stream WitnessStream, receipt ArtifactReceipt, raw []byte) (ledger WitnessLedger, code string) {
	if len(raw) == 0 || !validReceipt(receipt, stream.RunID+".witness.json", maxWitnessBytes) || receipt.Bytes != uint64(len(raw)) {
		return WitnessLedger{}, reasonEvidenceIncomplete
	}
	hash := sha256.Sum256(raw)
	if hex.EncodeToString(hash[:]) != receipt.SHA256 {
		return WitnessLedger{}, reasonEvidenceIncomplete
	}
	ledger, err := decodeRecord[WitnessLedger](raw, maxWitnessBytes)
	if err != nil || ledger.SchemaVersion != WitnessVersion || ledger.Stream != stream ||
		len(ledger.Records) == 0 || len(ledger.Records) > maxWitnessRecords || ledger.FinalSequence != uint64(len(ledger.Records)) {

		return WitnessLedger{}, reasonEvidenceIncomplete
	}
	var end uint64
	for index, record := range ledger.Records {
		if !validWitnessRecord(stream, record) || record.Sequence != uint64(index)+1 || record.OffsetBytes < end {
			return WitnessLedger{}, reasonEvidenceIncomplete
		}
		end = record.OffsetBytes + record.PayloadBytes
	}
	return ledger, ""
}

func validWitnessStream(stream WitnessStream) bool {
	return identifier.MatchString(stream.RunID) && identifier.MatchString(stream.StreamID) && stream.Generation > 0
}

func validWitnessRecord(stream WitnessStream, record WitnessRecord) bool {
	_, validEnd := add(record.OffsetBytes, record.PayloadBytes)
	if record.Stream != stream || record.Sequence == 0 || record.PayloadBytes == 0 || !validEnd {
		return false
	}
	_, valid := decodeSHA256(record.SHA256)
	return valid
}

func witnessRecords(stream WitnessStream, records []WitnessRecord, allowRetries bool) (map[uint64]WitnessRecord, *WitnessFinding) {
	if len(records) > maxWitnessRecords {
		return nil, &WitnessFinding{Code: reasonWitnessConflict}
	}
	indexed := make(map[uint64]WitnessRecord, len(records))
	for _, record := range records {
		if !validWitnessRecord(stream, record) {
			return nil, &WitnessFinding{Code: reasonWitnessConflict}
		}
		if prior, exists := indexed[record.Sequence]; exists && (!allowRetries || prior != record) {
			return nil, &WitnessFinding{Code: reasonWitnessConflict, Sequence: &record.Sequence}
		}
		indexed[record.Sequence] = record
	}
	return indexed, nil
}

func sortedWitnessSequences(records map[uint64]WitnessRecord) []uint64 {
	sequences := make([]uint64, 0, len(records))
	for sequence := range records {
		sequences = append(sequences, sequence)
	}
	slices.Sort(sequences)
	return sequences
}

func validRecoveredOrder(ledger WitnessLedger, observed map[uint64]WitnessRecord) bool {
	last := ledger.Records[len(ledger.Records)-1]
	end := last.OffsetBytes + last.PayloadBytes
	for _, sequence := range sortedWitnessSequences(observed) {
		if sequence <= ledger.FinalSequence {
			continue
		}
		record := observed[sequence]
		if record.OffsetBytes < end {
			return false
		}
		end = record.OffsetBytes + record.PayloadBytes
	}
	return true
}
