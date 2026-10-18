//go:build linux || darwin

package performance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func witnessFilePlan() WitnessFilePlan {
	return WitnessFilePlan{Stream: WitnessStream{RunID: "synthetic-file", StreamID: "stream-a", Generation: 3},
		CapacityBytes: 512 << 10, MaxPayloadBytes: 128 << 10, MaxRecords: 8}
}

func witnessTargetFiles(t *testing.T) (target, parent *os.File) {
	t.Helper()
	directory := t.TempDir()
	file, err := os.OpenFile(filepath.Join(directory, "target.bin"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	parent, err = os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parent.Close() })
	return file, parent
}

func witnessReadOnly(t *testing.T, target *os.File) *os.File {
	t.Helper()
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(target.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func writtenWitnessFile(t *testing.T) (WitnessFilePlan, *os.File, ArtifactReceipt, []byte, []WitnessRecord) {
	t.Helper()
	plan := witnessFilePlan()
	target, parent := witnessTargetFiles(t)
	root := evidenceRoot(t)
	store := witnessStore(t, root, plan.Stream)
	writer, err := NewWitnessFileWriter(plan, target, parent, store)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]WitnessRecord, 0, 2)
	for _, payload := range [][]byte{[]byte("record-a-payload"), bytes.Repeat([]byte("record-b"), 9000)} {
		result, appendErr := writer.Append(context.Background(), payload)
		if appendErr != nil || result.Receipt == nil {
			t.Fatal("target record not witnessed", appendErr)
		}
		records = append(records, result.Record)
	}
	writer.Close()
	final, err := store.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := LoadWitnessLedger(root, plan.Stream, final)
	if err != nil {
		t.Fatal(err)
	}
	return plan, target, final, raw, records
}

type witnessSourceProbe struct {
	WitnessFileSource
	read         func([]byte, int64) (int, error)
	stat         func() (os.FileInfo, error)
	reads, stats int
}

func (s *witnessSourceProbe) ReadAt(raw []byte, offset int64) (int, error) {
	s.reads++
	if s.read != nil {
		return s.read(raw, offset)
	}
	return s.WitnessFileSource.ReadAt(raw, offset)
}

func (s *witnessSourceProbe) Stat() (os.FileInfo, error) {
	s.stats++
	if s.stat != nil {
		return s.stat()
	}
	return s.WitnessFileSource.Stat()
}

func TestWitnessFileReadOnlyRecoveryAndUnwitnessedTail(t *testing.T) {
	for _, tail := range [][]byte{nil, []byte("torn unwitnessed tail")} {
		plan, target, receipt, raw, records := writtenWitnessFile(t)
		last := records[len(records)-1]
		end := last.OffsetBytes + last.PayloadBytes
		if len(tail) > 0 {
			if _, err := target.WriteAt(tail, int64(end)); err != nil {
				t.Fatal(err)
			}
		}
		readOnly := witnessReadOnly(t, target)
		before, err := os.ReadFile(readOnly.Name())
		if err != nil {
			t.Fatal(err)
		}
		source := &witnessSourceProbe{WitnessFileSource: readOnly}
		source.read = func(buffer []byte, offset int64) (int, error) {
			if uint64(offset)+uint64(len(buffer)) > end {
				t.Fatal("recovery read unwitnessed tail")
			}
			return readOnly.ReadAt(buffer, offset)
		}
		result := VerifyWitnessFile(context.Background(), plan, source, receipt, raw, records[:1])
		want := records[0].PayloadBytes + records[1].PayloadBytes
		if result.Status != Pass || result.VerifiedBytes == nil || *result.VerifiedBytes != want || result.VerifiedRecords != 2 || result.RepliedRecords != 1 ||
			result.UnwitnessedTailBytes == nil || *result.UnwitnessedTailBytes != uint64(len(tail)) || result.ReadBytes != end {

			t.Fatalf("read-only prefix recovery failed: %+v", result)
		}
		after, readErr := os.ReadFile(readOnly.Name())
		if readErr != nil || !bytes.Equal(before, after) {
			t.Fatal("recovery changed target data")
		}
	}
}

func TestWitnessFileDetectsCorruptHeadersPayloadAndShortFile(t *testing.T) {
	for _, name := range []string{"payload", "header hash", "padding", "metadata", "short file"} {
		t.Run(name, func(t *testing.T) {
			plan, target, receipt, raw, records := writtenWitnessFile(t)
			last := records[1]
			switch name {
			case "payload":
				if _, err := target.WriteAt([]byte{0}, int64(last.OffsetBytes+1)); err != nil {
					t.Fatal(err)
				}
			case "header hash":
				if _, err := target.WriteAt([]byte{0xff}, int64(last.OffsetBytes-WitnessHeaderBytes+24)); err != nil {
					t.Fatal(err)
				}
			case "padding":
				if _, err := target.WriteAt([]byte{1}, int64(last.OffsetBytes-1)); err != nil {
					t.Fatal(err)
				}
			case "metadata":
				changed := last
				changed.Stream.Generation++
				header, err := encodeWitnessHeader(changed)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := target.WriteAt(header, int64(last.OffsetBytes-WitnessHeaderBytes)); err != nil {
					t.Fatal(err)
				}
			case "short file":
				if err := target.Truncate(int64(last.OffsetBytes + last.PayloadBytes - 1)); err != nil {
					t.Fatal(err)
				}
			}
			readOnly := witnessReadOnly(t, target)
			before, err := os.ReadFile(readOnly.Name())
			if err != nil {
				t.Fatal(err)
			}
			result := VerifyWitnessFile(context.Background(), plan, readOnly, receipt, raw, records)
			if result.Status != Fail || result.VerifiedBytes != nil || result.VerifiedRecords != 1 || len(result.Findings) == 0 {
				t.Fatalf("changed target passed or lost healthy prefix: %+v", result)
			}
			after, readErr := os.ReadFile(readOnly.Name())
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatal("recovery repaired/truncated failed data")
			}
		})
	}
}

func TestWitnessFileRejectsInvalidInputsBeforeSourceCalls(t *testing.T) {
	for _, name := range []string{"plan", "receipt", "ledger", "reply", "layout", "budget", "canceled"} {
		t.Run(name, func(t *testing.T) {
			plan, target, receipt, raw, records := writtenWitnessFile(t)
			source := &witnessSourceProbe{WitnessFileSource: witnessReadOnly(t, target)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			replies := append([]WitnessRecord(nil), records...)
			switch name {
			case "plan":
				plan.MaxPayloadBytes = 0
			case "receipt":
				receipt.SHA256 = strings.Repeat("0", 64)
			case "ledger":
				raw[0] = ' '
			case "reply":
				replies[0].SHA256 = strings.Repeat("a", 64)
			case "layout":
				ledger := WitnessLedger{Records: records, Stream: plan.Stream, FinalSequence: 2, SchemaVersion: WitnessVersion}
				ledger.Records[0].OffsetBytes++
				raw = witnessBytes(t, ledger)
				receipt = witnessReceipt(plan.Stream, raw)
			case "budget":
				plan.MaxRecords = 1
			case "canceled":
				cancel()
			}
			result := VerifyWitnessFile(ctx, plan, source, receipt, raw, replies)
			if result.Status != Fail || result.VerifiedBytes != nil || source.stats != 0 || source.reads != 0 {
				t.Fatal("invalid request reached source I/O or claimed coverage")
			}
		})
	}
}

func TestWitnessFileRejectsMalformedReadersAndLateCancellation(t *testing.T) {
	for _, name := range []string{"negative", "oversized", "short nil", "private error", "late cancel", "post-stat cancel", "full EOF"} {
		t.Run(name, func(t *testing.T) {
			plan, target, receipt, raw, records := writtenWitnessFile(t)
			readOnly := witnessReadOnly(t, target)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source := &witnessSourceProbe{WitnessFileSource: readOnly}
			source.read = func(buffer []byte, offset int64) (int, error) {
				switch name {
				case "negative":
					return -1, nil
				case "oversized":
					return len(buffer) + 1, nil
				case "short nil":
					return 0, nil
				case "private error":
					return 0, errors.New(privateSentinel)
				}
				n, err := readOnly.ReadAt(buffer, offset)
				if name == "late cancel" && uint64(offset)+uint64(n) == records[1].OffsetBytes+records[1].PayloadBytes {
					cancel()
				}
				if name == "full EOF" && n == len(buffer) {
					err = io.EOF
				}
				return n, err
			}
			source.stat = func() (os.FileInfo, error) {
				if name == "post-stat cancel" && source.stats == 2 {
					cancel()
				}
				return readOnly.Stat()
			}
			result := VerifyWitnessFile(ctx, plan, source, receipt, raw, records)
			if name == "full EOF" {
				if result.Status != Pass {
					t.Fatal("legal full ReaderAt EOF rejected")
				}
			} else if result.Status != Fail || result.VerifiedBytes != nil {
				t.Fatal("malformed read or late cancellation passed")
			}
		})
	}
}

func TestWitnessFilePinsInputsAndRejectsChangingFile(t *testing.T) {
	for _, changed := range []bool{false, true} {
		plan, target, receipt, raw, records := writtenWitnessFile(t)
		readOnly := witnessReadOnly(t, target)
		replies := append([]WitnessRecord(nil), records...)
		source := &witnessSourceProbe{WitnessFileSource: readOnly}
		source.read = func(buffer []byte, offset int64) (int, error) {
			if source.reads == 1 {
				raw[0] = ' '
				replies[0].SHA256 = strings.Repeat("0", 64)
				if changed {
					mutator, err := os.OpenFile(readOnly.Name(), os.O_WRONLY|os.O_APPEND, 0)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := mutator.WriteString("extra"); err != nil {
						t.Fatal(err)
					}
					if err := mutator.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
			return readOnly.ReadAt(buffer, offset)
		}
		result := VerifyWitnessFile(context.Background(), plan, source, receipt, raw, replies)
		if changed {
			if result.Status != Fail || result.VerifiedBytes != nil || result.UnwitnessedTailBytes != nil {
				t.Fatal("changing source passed")
			}
		} else if result.Status != Pass || result.RepliedRecords != 2 {
			t.Fatal("callback changed pinned expectations/replies")
		}
	}
}

func TestWitnessHeaderRejectsMalformedEnvelope(t *testing.T) {
	record := witnessFixture().Records[0]
	record.OffsetBytes = WitnessHeaderBytes
	header, err := encodeWitnessHeader(record)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func([]byte) []byte{
		"short":    func(h []byte) []byte { return h[:len(h)-1] },
		"magic":    func(h []byte) []byte { h[0] ^= 1; return h },
		"version":  func(h []byte) []byte { binary.BigEndian.PutUint64(h[8:16], 2); return h },
		"length":   func(h []byte) []byte { binary.BigEndian.PutUint64(h[16:24], 1<<63); return h },
		"reserved": func(h []byte) []byte { h[56] = 1; return h },
		"padding":  func(h []byte) []byte { h[len(h)-1] = 1; return h },
		"unknown field": func(h []byte) []byte {
			return rewriteWitnessHeader(h, func(body []byte) []byte { return append(body[:len(body)-1], []byte(`,"unknown":true}`)...) })
		},
		"duplicate": func(h []byte) []byte {
			return rewriteWitnessHeader(h, func(body []byte) []byte {
				return bytes.Replace(body, []byte(`"sequence":1`), []byte(`"sequence":1,"sequence":1`), 1)
			})
		},
		"null sequence": func(h []byte) []byte {
			return rewriteWitnessHeader(h, func(body []byte) []byte {
				return bytes.Replace(body, []byte(`"sequence":1`), []byte(`"sequence":null`), 1)
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			if record, decodeErr := decodeWitnessHeader(mutate(bytes.Clone(header))); !errors.Is(decodeErr, ErrEvidenceCorrupt) || !reflect.DeepEqual(record, WitnessRecord{}) {
				t.Fatal("invalid header exposed metadata")
			}
		})
	}
}

func rewriteWitnessHeader(header []byte, change func([]byte) []byte) []byte {
	size := binary.BigEndian.Uint64(header[16:24])
	body := change(bytes.Clone(header[witnessHeaderPrefix : witnessHeaderPrefix+size]))
	clear(header[witnessHeaderPrefix:])
	binary.BigEndian.PutUint64(header[16:24], uint64(len(body)))
	hash := sha256.Sum256(body)
	copy(header[24:56], hash[:])
	copy(header[witnessHeaderPrefix:], body)
	return header
}
