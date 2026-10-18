//go:build linux || darwin

package performance

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

type witnessCommitFunc func(WitnessRecord) (ArtifactReceipt, error)

func (f witnessCommitFunc) Append(record WitnessRecord) (ArtifactReceipt, error) { return f(record) }

type witnessTargetProbe struct {
	*os.File
	events               *[]string
	write                func([]byte, int64) (int, error)
	sync                 func() error
	writes, syncs, stats int
}

func (f *witnessTargetProbe) Stat() (os.FileInfo, error) { f.stats++; return f.File.Stat() }

func (f *witnessTargetProbe) WriteAt(raw []byte, offset int64) (int, error) {
	f.writes++
	if f.events != nil {
		*f.events = append(*f.events, "write")
	}
	if f.write != nil {
		return f.write(raw, offset)
	}
	return f.File.WriteAt(raw, offset)
}

func (f *witnessTargetProbe) Sync() error {
	f.syncs++
	if f.events != nil {
		*f.events = append(*f.events, "target-sync")
	}
	if f.sync != nil {
		return f.sync()
	}
	return f.File.Sync()
}

type witnessDirectoryProbe struct {
	*os.File
	events *[]string
	sync   func() error
	syncs  int
}

func (d *witnessDirectoryProbe) Sync() error {
	d.syncs++
	if d.events != nil {
		*d.events = append(*d.events, "parent-sync")
	}
	if d.sync != nil {
		return d.sync()
	}
	return d.File.Sync()
}

func TestWitnessWriterSyncOrderAndPayloadPinning(t *testing.T) {
	target, parent := witnessTargetFiles(t)
	var events []string
	file := &witnessTargetProbe{File: target, events: &events}
	directory := &witnessDirectoryProbe{File: parent, events: &events}
	payload := []byte("independent pre-write data")
	original := bytes.Clone(payload)
	file.write = func(raw []byte, offset int64) (int, error) {
		if file.writes == 1 {
			payload[0] ^= 1
		}
		return target.WriteAt(raw, offset)
	}
	committer := witnessCommitFunc(func(record WitnessRecord) (ArtifactReceipt, error) {
		events = append(events, "witness")
		return witnessRecordReceipt(record)
	})
	writer, err := NewWitnessFileWriter(witnessFilePlan(), file, directory, committer)
	if err != nil {
		t.Fatal(err)
	}
	first, err := writer.Append(context.Background(), payload)
	if err != nil || first.Receipt == nil {
		t.Fatal(err)
	}
	second, err := writer.Append(context.Background(), []byte("next"))
	if err != nil || second.Receipt == nil {
		t.Fatal(err)
	}
	want := []string{"write", "write", "target-sync", "parent-sync", "witness", "write", "write", "target-sync", "witness"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("unsafe write/sync/witness order: %v", events)
	}
	got := make([]byte, len(original))
	if n, readErr := target.ReadAt(got, int64(first.Record.OffsetBytes)); readErr != nil || n != len(got) || !bytes.Equal(got, original) {
		t.Fatal("target callback changed pinned payload")
	}
	writer.Close()
	if _, statErr := target.Stat(); statErr != nil {
		t.Fatal("writer closed caller target")
	}
}

func TestWitnessWriterTargetErrorsCannotReachWitness(t *testing.T) {
	for _, name := range []string{"header short", "payload short", "negative count", "oversized count", "private error", "target sync", "parent sync", "canceled after header", "canceled after target sync"} {
		t.Run(name, func(t *testing.T) {
			target, parent := witnessTargetFiles(t)
			file := &witnessTargetProbe{File: target}
			directory := &witnessDirectoryProbe{File: parent}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			file.write = func(raw []byte, offset int64) (int, error) {
				switch {
				case name == "header short" && file.writes == 1, name == "payload short" && file.writes == 2:
					return target.WriteAt(raw[:len(raw)-1], offset)
				case name == "negative count":
					return -1, nil
				case name == "oversized count":
					return len(raw) + 1, nil
				case name == "private error":
					return 0, errors.New(privateSentinel)
				}
				n, err := target.WriteAt(raw, offset)
				if name == "canceled after header" && file.writes == 1 {
					cancel()
				}
				return n, err
			}
			file.sync = func() error {
				if name == "target sync" {
					return errors.New(privateSentinel)
				}
				if name == "canceled after target sync" {
					cancel()
				}
				return target.Sync()
			}
			if name == "parent sync" {
				directory.sync = func() error { return errors.New(privateSentinel) }
			}
			calls := 0
			committer := witnessCommitFunc(func(record WitnessRecord) (ArtifactReceipt, error) { calls++; return witnessRecordReceipt(record) })
			writer, err := NewWitnessFileWriter(witnessFilePlan(), file, directory, committer)
			if err != nil {
				t.Fatal(err)
			}
			result, err := writer.Append(ctx, []byte("checked payload"))
			if err == nil || result != (WitnessWriteResult{}) || calls != 0 || strings.Contains(err.Error(), privateSentinel) {
				t.Fatal("failed/unsynced target was witnessed or exposed input", err)
			}
			before, readErr := os.ReadFile(target.Name())
			if readErr != nil {
				t.Fatal(readErr)
			}
			_, retryErr := writer.Append(context.Background(), []byte("replacement"))
			if !errors.Is(retryErr, ErrWitnessFailed) {
				t.Fatal("writer continued after target failure")
			}
			after, readErr := os.ReadFile(target.Name())
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatal("failed target bytes were rewritten")
			}
		})
	}
}

func TestWitnessWriterUncertainReplyRetriesOnlyWitness(t *testing.T) {
	for _, name := range []string{"lost reply", "bad receipt", "not committed", "canceled reply"} {
		t.Run(name, func(t *testing.T) {
			plan := witnessFilePlan()
			target, parent := witnessTargetFiles(t)
			file := &witnessTargetProbe{File: target}
			directory := &witnessDirectoryProbe{File: parent}
			root := evidenceRoot(t)
			store := witnessStore(t, root, plan.Stream)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			committer := witnessCommitFunc(func(record WitnessRecord) (ArtifactReceipt, error) {
				calls++
				if calls == 1 && name == "not committed" {
					return ArtifactReceipt{}, errors.New(privateSentinel)
				}
				receipt, err := store.Append(record)
				if calls == 1 {
					switch name {
					case "lost reply":
						return ArtifactReceipt{}, errors.New(privateSentinel)
					case "bad receipt":
						receipt.SHA256 = strings.Repeat("0", 64)
					case "canceled reply":
						cancel()
					}
				}
				return receipt, err
			})
			writer, err := NewWitnessFileWriter(plan, file, directory, committer)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := writer.Append(ctx, []byte("durable before witness request"))
			if !errors.Is(err, ErrWitnessPending) || pending.Receipt != nil || pending.Record.Sequence != 1 {
				t.Fatal("uncertain commit was acknowledged or forgotten")
			}
			if record, ok := writer.Pending(); !ok || record != pending.Record {
				t.Fatal("pending request missing")
			}
			before, readErr := os.ReadFile(target.Name())
			if readErr != nil {
				t.Fatal(readErr)
			}
			beforeStats := file.stats
			blocked, blockedErr := writer.Append(context.Background(), []byte("must not be written"))
			if !errors.Is(blockedErr, ErrWitnessPending) || blocked.Record != pending.Record || calls != 1 {
				t.Fatal("pending request allowed another write or submission")
			}
			canceled, stop := context.WithCancel(context.Background())
			stop()
			if _, cancelErr := writer.RetryWitness(canceled); !errors.Is(cancelErr, context.Canceled) || calls != 1 {
				t.Fatal("canceled resolution submitted a witness request")
			}
			resolved, retryErr := writer.RetryWitness(context.Background())
			if retryErr != nil || resolved.Receipt == nil || resolved.Record != pending.Record || calls != 2 {
				t.Fatal("witness-only resolution failed", retryErr)
			}
			if file.writes != 2 || file.syncs != 1 || file.stats != beforeStats || directory.syncs != 1 {
				t.Fatal("witness retry repeated target I/O")
			}
			after, readErr := os.ReadFile(target.Name())
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatal("witness retry rewrote target bytes")
			}
			if _, ok := writer.Pending(); ok {
				t.Fatal("acknowledged request still pending")
			}
			writer.Close()
			final, finalErr := store.Finalize()
			if finalErr != nil {
				t.Fatal(finalErr)
			}
			raw, loadErr := LoadWitnessLedger(root, plan.Stream, final)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			result := VerifyWitnessFile(context.Background(), plan, witnessReadOnly(t, target), final, raw, []WitnessRecord{resolved.Record})
			if result.Status != Pass || result.WitnessedRecords != 1 {
				t.Fatal("resolved record failed actual file verification")
			}
		})
	}
}

func TestWitnessWriterClosePreservesLostReplyForReconciliation(t *testing.T) {
	plan := witnessFilePlan()
	target, parent := witnessTargetFiles(t)
	root := evidenceRoot(t)
	store := witnessStore(t, root, plan.Stream)
	committer := witnessCommitFunc(func(record WitnessRecord) (ArtifactReceipt, error) {
		if _, err := store.Append(record); err != nil {
			t.Fatal(err)
		}
		return ArtifactReceipt{}, errors.New(privateSentinel)
	})
	writer, err := NewWitnessFileWriter(plan, target, parent, committer)
	if err != nil {
		t.Fatal(err)
	}
	_, err = writer.Append(context.Background(), []byte("lost reply still requires recovery"))
	if !errors.Is(err, ErrWitnessPending) {
		t.Fatal(err)
	}
	writer.Close()
	if _, ok := writer.Pending(); !ok {
		t.Fatal("Close forgot unresolved metadata")
	}
	if _, retryErr := writer.RetryWitness(context.Background()); !errors.Is(retryErr, ErrEvidenceClosed) {
		t.Fatal("closed writer submitted a request")
	}
	final, err := store.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := LoadWitnessLedger(root, plan.Stream, final)
	if err != nil {
		t.Fatal(err)
	}
	result := VerifyWitnessFile(context.Background(), plan, witnessReadOnly(t, target), final, raw, nil)
	if result.Status != Pass || result.VerifiedRecords != 1 || result.RepliedRecords != 0 {
		t.Fatal("lost reply hid an actual committed target record")
	}
}

func TestWitnessWriterRejectsExistingDataAndFiniteLimits(t *testing.T) {
	for _, name := range []string{"nonempty", "payload", "records", "capacity", "canceled"} {
		t.Run(name, func(t *testing.T) {
			plan := witnessFilePlan()
			target, parent := witnessTargetFiles(t)
			calls := 0
			committer := witnessCommitFunc(func(record WitnessRecord) (ArtifactReceipt, error) { calls++; return witnessRecordReceipt(record) })
			if name == "nonempty" {
				if _, err := target.WriteString("existing"); err != nil {
					t.Fatal(err)
				}
				if writer, err := NewWitnessFileWriter(plan, target, parent, committer); writer != nil || !errors.Is(err, ErrInvalidWitness) {
					t.Fatal("existing file admitted")
				}
				return
			}
			if name == "records" {
				plan.MaxRecords = 1
			}
			if name == "capacity" {
				plan.CapacityBytes, plan.MaxPayloadBytes = WitnessHeaderBytes+2, 2
			}
			writer, err := NewWitnessFileWriter(plan, target, parent, committer)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			payload := []byte("ok")
			switch name {
			case "payload":
				payload = make([]byte, plan.MaxPayloadBytes+1)
			case "records", "capacity":
				if _, appendErr := writer.Append(ctx, payload); appendErr != nil {
					t.Fatal(appendErr)
				}
			case "canceled":
				cancel()
			}
			before, readErr := os.ReadFile(target.Name())
			if readErr != nil {
				t.Fatal(readErr)
			}
			beforeCalls := calls
			if result, appendErr := writer.Append(ctx, payload); appendErr == nil || result != (WitnessWriteResult{}) {
				t.Fatal("invalid/over-budget append accepted")
			}
			after, readErr := os.ReadFile(target.Name())
			if readErr != nil || !bytes.Equal(before, after) || calls != beforeCalls {
				t.Fatal("invalid append reached target or witness")
			}
		})
	}
}
