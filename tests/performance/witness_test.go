package performance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func witnessFixture() WitnessLedger {
	stream := WitnessStream{RunID: "synthetic-witness", StreamID: "stream-a", Generation: 2}
	ledger := WitnessLedger{Stream: stream, SchemaVersion: WitnessVersion, FinalSequence: 2}
	for index, payload := range []string{"first committed payload", "second committed payload"} {
		hash := sha256.Sum256([]byte(payload))
		ledger.Records = append(ledger.Records, WitnessRecord{
			Stream: stream, Sequence: uint64(index) + 1, OffsetBytes: uint64(index) * 4096,
			PayloadBytes: uint64(len(payload)), SHA256: hex.EncodeToString(hash[:]),
		})
	}
	return ledger
}

func witnessReceipt(stream WitnessStream, raw []byte) ArtifactReceipt {
	hash := sha256.Sum256(raw)
	return ArtifactReceipt{Name: stream.RunID + ".witness.json", Bytes: uint64(len(raw)), SHA256: hex.EncodeToString(hash[:])}
}

func witnessBytes(t *testing.T, ledger WitnessLedger) []byte {
	t.Helper()
	raw, err := json.Marshal(ledger)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func reconcileFixture(t *testing.T, ledger WitnessLedger, recovered, replies []WitnessRecord) WitnessResult {
	t.Helper()
	raw := witnessBytes(t, ledger)
	return ReconcileWitness(ledger.Stream, witnessReceipt(ledger.Stream, raw), raw, recovered, replies)
}

func witnessHasCode(result WitnessResult, code string, sequence uint64) bool {
	for _, finding := range result.Findings {
		if finding.Code == code && (sequence == 0 || finding.Sequence != nil && *finding.Sequence == sequence) {
			return true
		}
	}
	return false
}

func TestWitnessRequiresDurableEntryWhoseReplyWasLost(t *testing.T) {
	ledger := witnessFixture()
	replies := ledger.Records[:1]
	result := reconcileFixture(t, ledger, ledger.Records[:1], replies)
	if result.Status != Fail || result.VerifiedBytes != nil || result.WitnessedRecords != 2 ||
		result.VerifiedRecords != 1 || result.RepliedRecords != 1 || !witnessHasCode(result, "COVERAGE_INCOMPLETE", 2) {

		t.Fatalf("lost reply hid a missing durable record: %+v", result)
	}
	result = reconcileFixture(t, ledger, ledger.Records, replies)
	if result.Status != Pass || result.VerifiedBytes == nil || result.VerifiedRecords != 2 || result.RepliedRecords != 1 {
		t.Fatalf("recovered durable record with lost reply did not pass: %+v", result)
	}
	want := ledger.Records[0].PayloadBytes + ledger.Records[1].PayloadBytes
	if *result.VerifiedBytes != want {
		t.Fatal("verified bytes omitted an entry whose reply was lost")
	}
	result = reconcileFixture(t, ledger, ledger.Records, nil)
	if result.Status != Pass || result.VerifiedRecords != 2 || result.RepliedRecords != 0 {
		t.Fatal("all lost replies shrank the required durable prefix")
	}
}

func TestWitnessTailAndIdenticalReplyRetries(t *testing.T) {
	ledger := witnessFixture()
	recovered := slices.Clone(ledger.Records)
	for _, sequence := range []uint64{4, 3} {
		tail := ledger.Records[0]
		tail.Sequence, tail.OffsetBytes = sequence, sequence*4096
		recovered = append(recovered, tail)
	}
	replies := append(slices.Clone(ledger.Records), ledger.Records[0])
	result := reconcileFixture(t, ledger, recovered, replies)
	if result.Status != Pass || result.RepliedRecords != 2 || result.VerifiedRecords != 2 || result.VerifiedBytes == nil ||
		*result.VerifiedBytes != ledger.Records[0].PayloadBytes+ledger.Records[1].PayloadBytes ||
		!slices.Equal(result.TailSequences, []uint64{3, 4}) {

		t.Fatalf("unwitnessed tail/retry accounting changed durable coverage: %+v", result)
	}
	slices.Reverse(recovered)
	slices.Reverse(replies)
	if reordered := reconcileFixture(t, ledger, recovered, replies); !reflect.DeepEqual(reordered, result) {
		t.Fatal("observation order changed reconciliation")
	}
}

func TestWitnessRejectsCorruptOrConflictingTargetRecords(t *testing.T) {
	for name, change := range map[string]func([]WitnessRecord){
		"payload hash":  func(records []WitnessRecord) { records[1].SHA256 = strings.Repeat("a", 64) },
		"offset":        func(records []WitnessRecord) { records[1].OffsetBytes++ },
		"length":        func(records []WitnessRecord) { records[1].PayloadBytes++ },
		"generation":    func(records []WitnessRecord) { records[1].Stream.Generation++ },
		"stream":        func(records []WitnessRecord) { records[1].Stream.StreamID = "other-stream" },
		"run":           func(records []WitnessRecord) { records[1].Stream.RunID = "other-run" },
		"duplicate":     func(records []WitnessRecord) { records[1] = records[0] },
		"zero sequence": func(records []WitnessRecord) { records[1].Sequence = 0 },
		"zero payload":  func(records []WitnessRecord) { records[1].PayloadBytes = 0 },
		"overflow":      func(records []WitnessRecord) { records[1].OffsetBytes = math.MaxUint64 },
		"invalid hash":  func(records []WitnessRecord) { records[1].SHA256 = "private-witness-sentinel" },
	} {
		t.Run(name, func(t *testing.T) {
			ledger := witnessFixture()
			recovered := slices.Clone(ledger.Records)
			change(recovered)
			result := reconcileFixture(t, ledger, recovered, ledger.Records)
			code := reasonWitnessConflict
			if name == "payload hash" {
				code = "CHECKSUM_MISMATCH"
			}
			if result.Status != Fail || result.VerifiedBytes != nil || !witnessHasCode(result, code, 0) {
				t.Fatalf("changed target accepted: %+v", result)
			}
		})
	}
	ledger := witnessFixture()
	if result := reconcileFixture(t, ledger, nil, nil); result.Status != Fail || result.VerifiedRecords != 0 || result.VerifiedBytes != nil || len(result.Findings) != 2 {
		t.Fatal("empty recovery passed a nonempty witness")
	}
}

func TestWitnessRejectsReplyConflictAndOverlappingTail(t *testing.T) {
	for _, name := range []string{"changed reply", "conflicting retry", "reply outside ledger", "tail overlaps durable prefix", "tail overlaps tail"} {
		t.Run(name, func(t *testing.T) {
			ledger := witnessFixture()
			recovered := slices.Clone(ledger.Records)
			replies := slices.Clone(ledger.Records)
			record := ledger.Records[1]
			switch name {
			case "changed reply":
				replies[1].SHA256 = strings.Repeat("b", 64)
			case "conflicting retry":
				record.SHA256 = strings.Repeat("b", 64)
				replies = append(replies, record)
			case "reply outside ledger":
				record.Sequence, record.OffsetBytes = 3, 8192
				recovered = append(recovered, record)
				replies = append(replies, record)
			case "tail overlaps durable prefix":
				record.Sequence = 3
				recovered = append(recovered, record)
			case "tail overlaps tail":
				record.Sequence, record.OffsetBytes = 3, 8192
				recovered = append(recovered, record)
				record.Sequence = 4
				recovered = append(recovered, record)
			}
			result := reconcileFixture(t, ledger, recovered, replies)
			if result.Status != Fail || result.VerifiedBytes != nil || !witnessHasCode(result, reasonWitnessConflict, 0) {
				t.Fatal("reply/append conflict passed")
			}
			if name == "conflicting retry" && !witnessHasCode(result, reasonWitnessConflict, 2) {
				t.Fatal("conflicting duplicate lost its sequence")
			}
		})
	}
}

func TestWitnessRejectsInvalidFinalizedLedgers(t *testing.T) {
	for name, change := range map[string]func(*WitnessLedger){
		"empty":                func(l *WitnessLedger) { l.Records = nil; l.FinalSequence = 0 },
		"unsupported version":  func(l *WitnessLedger) { l.SchemaVersion++ },
		"missing finalization": func(l *WitnessLedger) { l.FinalSequence = 0 },
		"incomplete prefix":    func(l *WitnessLedger) { l.FinalSequence++ },
		"sequence gap":         func(l *WitnessLedger) { l.Records[1].Sequence++ },
		"duplicate":            func(l *WitnessLedger) { l.Records[1] = l.Records[0] },
		"overlap":              func(l *WitnessLedger) { l.Records[1].OffsetBytes = 1 },
		"overflow":             func(l *WitnessLedger) { l.Records[1].OffsetBytes = math.MaxUint64 },
		"wrong generation":     func(l *WitnessLedger) { l.Records[1].Stream.Generation++ },
		"bad hash":             func(l *WitnessLedger) { l.Records[0].SHA256 = strings.Repeat("A", 64) },
		"too many records": func(l *WitnessLedger) {
			l.Records = make([]WitnessRecord, maxWitnessRecords+1)
			l.FinalSequence = uint64(len(l.Records))
		},
	} {
		t.Run(name, func(t *testing.T) {
			ledger := witnessFixture()
			change(&ledger)
			result := reconcileFixture(t, ledger, witnessFixture().Records, nil)
			if result.Status != Fail || result.VerifiedBytes != nil || result.WitnessedRecords != 0 || !witnessHasCode(result, reasonEvidenceIncomplete, 0) {
				t.Fatal("invalid finalization exposed trusted coverage")
			}
		})
	}
}

func TestWitnessStrictDocumentsAndTrustedReceipts(t *testing.T) {
	ledger := witnessFixture()
	for name, change := range map[string]func([]byte) []byte{
		"missing final sequence": func(raw []byte) []byte { return bytes.Replace(raw, []byte(`"final_sequence":2,`), nil, 1) },
		"null records":           func(raw []byte) []byte { return editOutput(t, raw, func(d map[string]any) { d["records"] = nil }) },
		"unknown": func(raw []byte) []byte {
			return editOutput(t, raw, func(d map[string]any) { d["private-witness-sentinel"] = true })
		},
		"duplicate": func(raw []byte) []byte {
			return bytes.Replace(raw, []byte(`"schema_version":1`), []byte(`"schema_version":1,"schema_version":1`), 1)
		},
		"Unicode alias": func(raw []byte) []byte { return bytes.Replace(raw, []byte(`"stream_id":`), []byte(`"ſtream_id":`), 1) },
		"trailing":      func(raw []byte) []byte { return append(raw, []byte(`{}`)...) },
		"torn":          func(raw []byte) []byte { return raw[:len(raw)-1] },
		"oversize":      func(_ []byte) []byte { return bytes.Repeat([]byte(" "), maxWitnessBytes+1) },
	} {
		t.Run(name, func(t *testing.T) {
			raw := change(witnessBytes(t, ledger))
			result := ReconcileWitness(ledger.Stream, witnessReceipt(ledger.Stream, raw), raw, ledger.Records, ledger.Records)
			if result.Status != Fail || result.VerifiedBytes != nil || result.WitnessedRecords != 0 {
				t.Fatal("malformed finalized document accepted")
			}
		})
	}
	for name, change := range map[string]func(*ArtifactReceipt){
		"path":      func(r *ArtifactReceipt) { r.Name = "../" + r.Name },
		"cross run": func(r *ArtifactReceipt) { r.Name = "other-run.witness.json" },
		"hash":      func(r *ArtifactReceipt) { r.SHA256 = strings.Repeat("0", 64) },
		"length":    func(r *ArtifactReceipt) { r.Bytes++ },
	} {
		t.Run(name, func(t *testing.T) {
			raw := witnessBytes(t, ledger)
			receipt := witnessReceipt(ledger.Stream, raw)
			change(&receipt)
			if result := ReconcileWitness(ledger.Stream, receipt, raw, ledger.Records, nil); result.Status != Fail || result.VerifiedBytes != nil {
				t.Fatal("substituted trusted receipt accepted")
			}
		})
	}
}

func TestWitnessInputLimitsAndImmutability(t *testing.T) {
	ledger := witnessFixture()
	raw := witnessBytes(t, ledger)
	recovered, replies := slices.Clone(ledger.Records), slices.Clone(ledger.Records)
	before := bytes.Clone(raw)
	result := ReconcileWitness(ledger.Stream, witnessReceipt(ledger.Stream, raw), raw, recovered, replies)
	if result.Status != Pass || !bytes.Equal(before, raw) || !reflect.DeepEqual(recovered, ledger.Records) || !reflect.DeepEqual(replies, ledger.Records) {
		t.Fatal("reconciliation mutated caller inputs")
	}
	bad := ledger.Stream
	bad.Generation = 0
	if result = ReconcileWitness(bad, witnessReceipt(ledger.Stream, raw), raw, recovered, replies); result.Status != Fail || !witnessHasCode(result, "INVALID_REQUEST", 0) {
		t.Fatal("invalid expected stream accepted")
	}
	for _, oversizedReplies := range []bool{false, true} {
		tooMany := make([]WitnessRecord, maxWitnessRecords+1)
		readRecords, replyRecords := recovered, replies
		if oversizedReplies {
			replyRecords = tooMany
		} else {
			readRecords = tooMany
		}
		if result = ReconcileWitness(ledger.Stream, witnessReceipt(ledger.Stream, raw), raw, readRecords, replyRecords); result.Status != Fail || result.VerifiedBytes != nil {
			t.Fatal("unbounded observations accepted")
		}
	}
}

func TestWitnessRejectsOtherFinalizedGeneration(t *testing.T) {
	ledger := witnessFixture()
	expected := ledger.Stream
	ledger.Stream.Generation++
	for index := range ledger.Records {
		ledger.Records[index].Stream = ledger.Stream
	}
	raw := witnessBytes(t, ledger)
	result := ReconcileWitness(expected, witnessReceipt(expected, raw), raw, ledger.Records, ledger.Records)
	if result.Status != Fail || result.VerifiedBytes != nil || result.WitnessedRecords != 0 || !witnessHasCode(result, reasonEvidenceIncomplete, 0) {
		t.Fatal("receipt for another finalized generation was adopted")
	}
}
