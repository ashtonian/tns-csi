//go:build linux || darwin

package performance

import (
	"bytes"
	"errors"
	"math"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func witnessStore(t *testing.T, root *os.Root, stream WitnessStream) *WitnessStore {
	t.Helper()
	store, err := NewWitnessStore(root, stream)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return store
}

func commitWitness(t *testing.T, store *WitnessStore, records []WitnessRecord) []ArtifactReceipt {
	t.Helper()
	receipts := make([]ArtifactReceipt, 0, len(records))
	for _, record := range records {
		receipt, err := store.Append(record)
		if err != nil {
			t.Fatal(err)
		}
		receipts = append(receipts, receipt)
	}
	return receipts
}

func requireWitnessError(t *testing.T, receipt ArtifactReceipt, err, want error) {
	t.Helper()
	if receipt != (ArtifactReceipt{}) || !errors.Is(err, want) {
		t.Fatalf("failed witness operation returned a receipt: %+v (%v), want %v", receipt, err, want)
	}
	if strings.Contains(err.Error(), privateSentinel) {
		t.Fatal("witness error exposed input")
	}
}

func TestWitnessStoreLostReplyFinalizeAndReopen(t *testing.T) {
	root := evidenceRoot(t)
	ledger := witnessFixture()
	store := witnessStore(t, root, ledger.Stream)
	first := commitWitness(t, store, ledger.Records[:1])[0]
	if raw, loadErr := LoadWitnessLedger(root, ledger.Stream, first); raw != nil || !errors.Is(loadErr, ErrEvidenceCorrupt) {
		t.Fatal("a commit receipt was accepted as finalization evidence")
	}
	before := evidenceSnapshot(t, root)
	retry, err := store.Append(ledger.Records[0])
	if err != nil || retry != first || !reflect.DeepEqual(before, evidenceSnapshot(t, root)) {
		t.Fatal("identical retry wrote another entry or changed its receipt", err)
	}
	// The second commit reply is deliberately omitted from writer observations.
	commitWitness(t, store, ledger.Records[1:])
	final, err := store.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := LoadWitnessLedger(root, ledger.Stream, final)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeRecord[WitnessLedger](raw, maxWitnessBytes)
	if err != nil || !reflect.DeepEqual(decoded, ledger) {
		t.Fatal("finalization lost accepted records", err)
	}
	before = evidenceSnapshot(t, root)
	again, err := store.Finalize()
	if err != nil || again != final || !reflect.DeepEqual(before, evidenceSnapshot(t, root)) {
		t.Fatal("finalization retry rewrote evidence or changed its receipt", err)
	}
	late := ledger.Records[1]
	late.Sequence, late.OffsetBytes = 3, 8192
	receipt, err := store.Append(late)
	requireWitnessError(t, receipt, err, ErrWitnessSealed)
	if closeErr := store.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if _, statErr := root.Stat(final.Name); statErr != nil {
		t.Fatal("store closed caller root", statErr)
	}
	reopened, err := os.OpenRoot(root.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	raw, err = LoadWitnessLedger(reopened, ledger.Stream, final)
	if err != nil {
		t.Fatal(err)
	}
	result := ReconcileWitness(ledger.Stream, final, raw, ledger.Records, ledger.Records[:1])
	if result.Status != Pass || result.VerifiedRecords != 2 || result.RepliedRecords != 1 {
		t.Fatal("durable record whose reply was lost disappeared from required recovery")
	}
	result = ReconcileWitness(ledger.Stream, final, raw, ledger.Records[:1], ledger.Records[:1])
	if result.Status != Fail || !witnessHasCode(result, "COVERAGE_INCOMPLETE", 2) {
		t.Fatal("finalized store passed a missing record whose reply was lost")
	}
}

func TestWitnessStoreInvalidCommitsFailPermanently(t *testing.T) {
	for name, change := range map[string]func(*WitnessRecord){
		"gap":           func(r *WitnessRecord) { r.Sequence = 3 },
		"zero sequence": func(r *WitnessRecord) { r.Sequence = 0 },
		"overlap":       func(r *WitnessRecord) { r.OffsetBytes = 0 },
		"overflow":      func(r *WitnessRecord) { r.OffsetBytes = math.MaxUint64 },
		"empty":         func(r *WitnessRecord) { r.PayloadBytes = 0 },
		"bad digest":    func(r *WitnessRecord) { r.SHA256 = privateSentinel },
		"generation":    func(r *WitnessRecord) { r.Stream.Generation++ },
		"stream":        func(r *WitnessRecord) { r.Stream.StreamID = "other-stream" },
		"conflict":      func(r *WitnessRecord) { r.Sequence = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			root := evidenceRoot(t)
			ledger := witnessFixture()
			store := witnessStore(t, root, ledger.Stream)
			commitWitness(t, store, ledger.Records[:1])
			before := evidenceSnapshot(t, root)
			record := ledger.Records[1]
			change(&record)
			receipt, err := store.Append(record)
			want := ErrInvalidWitness
			if name == "conflict" {
				want = ErrWitnessConflict
			}
			requireWitnessError(t, receipt, err, want)
			if !reflect.DeepEqual(before, evidenceSnapshot(t, root)) {
				t.Fatal("rejected commit changed evidence")
			}
			receipt, err = store.Append(ledger.Records[1])
			requireWitnessError(t, receipt, err, ErrWitnessFailed)
			receipt, err = store.Finalize()
			requireWitnessError(t, receipt, err, ErrWitnessFailed)
		})
	}
}

func TestWitnessStorePreservesPersistenceFailure(t *testing.T) {
	root := evidenceRoot(t)
	ledger := witnessFixture()
	store := witnessStore(t, root, ledger.Stream)
	name := witnessEntryName(ledger.Stream.RunID, 1)
	if err := root.WriteFile(name, []byte(privateSentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	receipt, err := store.Append(ledger.Records[0])
	requireWitnessError(t, receipt, err, ErrEvidenceExists)
	if string(evidenceBytes(t, root, name)) != privateSentinel {
		t.Fatal("existing entry overwritten")
	}
	if removeErr := root.Remove(name); removeErr != nil {
		t.Fatal(removeErr)
	}
	receipt, err = store.Append(ledger.Records[0])
	requireWitnessError(t, receipt, err, ErrWitnessFailed)
	receipt, err = store.Finalize()
	requireWitnessError(t, receipt, err, ErrWitnessFailed)
}

func TestWitnessStoreRefusesAlteredCommittedArtifacts(t *testing.T) {
	for _, name := range []string{"start", "entry", "missing", "symlink", "public file", "extra", "existing final", "public root"} {
		t.Run(name, func(t *testing.T) {
			root := evidenceRoot(t)
			ledger := witnessFixture()
			store := witnessStore(t, root, ledger.Stream)
			receipts := commitWitness(t, store, ledger.Records)
			entry := receipts[0].Name
			original := evidenceBytes(t, root, entry)
			switch name {
			case "start":
				if err := root.WriteFile(ledger.Stream.RunID+".witness.start.json", []byte(privateSentinel), 0o600); err != nil {
					t.Fatal(err)
				}
			case "entry":
				if err := root.WriteFile(entry, original[:len(original)-1], 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := root.Remove(entry); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := root.Rename(entry, "preserved-record"); err != nil {
					t.Fatal(err)
				}
				if err := root.Symlink("preserved-record", entry); err != nil {
					t.Fatal(err)
				}
			case "public file":
				if err := root.Chmod(entry, 0o644); err != nil {
					t.Fatal(err)
				}
			case "extra":
				if err := root.WriteFile(witnessEntryName(ledger.Stream.RunID, 3), []byte(privateSentinel), 0o600); err != nil {
					t.Fatal(err)
				}
			case "existing final":
				if err := root.WriteFile(ledger.Stream.RunID+".witness.json", []byte(privateSentinel), 0o600); err != nil {
					t.Fatal(err)
				}
			case "public root":
				if err := os.Chmod(root.Name(), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			receipt, err := store.Finalize()
			requireWitnessError(t, receipt, err, ErrEvidenceIncomplete)
			if name == "entry" {
				if restoreErr := root.WriteFile(entry, original, 0o600); restoreErr != nil {
					t.Fatal(restoreErr)
				}
			}
			receipt, err = store.Finalize()
			requireWitnessError(t, receipt, err, ErrWitnessFailed)
			if name == "existing final" && string(evidenceBytes(t, root, ledger.Stream.RunID+".witness.json")) != privateSentinel {
				t.Fatal("existing final ledger overwritten")
			}
		})
	}
}

func TestWitnessStoreRetriesVerifyOriginalFiles(t *testing.T) {
	for _, finalized := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "finalization"}[finalized], func(t *testing.T) {
			root := evidenceRoot(t)
			ledger := witnessFixture()
			store := witnessStore(t, root, ledger.Stream)
			receipt := commitWitness(t, store, ledger.Records[:1])[0]
			if finalized {
				var err error
				receipt, err = store.Finalize()
				if err != nil {
					t.Fatal(err)
				}
			}
			original := evidenceBytes(t, root, receipt.Name)
			if err := root.WriteFile(receipt.Name, []byte(privateSentinel), 0o600); err != nil {
				t.Fatal(err)
			}
			var got ArtifactReceipt
			var err error
			if finalized {
				got, err = store.Finalize()
			} else {
				got, err = store.Append(ledger.Records[0])
			}
			requireWitnessError(t, got, err, ErrEvidenceIncomplete)
			if restoreErr := root.WriteFile(receipt.Name, original, 0o600); restoreErr != nil {
				t.Fatal(restoreErr)
			}
			got, err = store.Finalize()
			requireWitnessError(t, got, err, ErrWitnessFailed)
		})
	}
}

func TestWitnessStoreAbandonmentAndRunReuse(t *testing.T) {
	for _, existing := range []string{"abandoned", "start", "orphan entry", "final", "symlink"} {
		t.Run(existing, func(t *testing.T) {
			root := evidenceRoot(t)
			ledger := witnessFixture()
			name := ledger.Stream.RunID + ".witness.start.json"
			switch existing {
			case "abandoned":
				store := witnessStore(t, root, ledger.Stream)
				commitWitness(t, store, ledger.Records[:1])
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				receipt, err := store.Finalize()
				requireWitnessError(t, receipt, err, ErrEvidenceClosed)
				if _, err := root.Stat(ledger.Stream.RunID + ".witness.json"); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("Close finalized an abandoned store")
				}
			case "orphan entry":
				name = witnessEntryName(ledger.Stream.RunID, 2)
			case "final":
				name = ledger.Stream.RunID + ".witness.json"
			case "symlink":
				if err := root.Symlink("absent", name); err != nil {
					t.Fatal(err)
				}
			}
			if existing != "abandoned" && existing != "symlink" {
				if err := root.WriteFile(name, []byte(privateSentinel), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if store, err := NewWitnessStore(root, ledger.Stream); store != nil || !errors.Is(err, ErrEvidenceExists) {
				t.Fatal("existing run was resumed or overwritten", err)
			}
		})
	}
	root := evidenceRoot(t)
	store := witnessStore(t, root, witnessFixture().Stream)
	receipt, err := store.Finalize()
	requireWitnessError(t, receipt, err, ErrInvalidWitness)
	receipt, err = store.Append(witnessFixture().Records[0])
	requireWitnessError(t, receipt, err, ErrWitnessFailed)
}

func TestWitnessStoreConcurrentReservationAndRetries(t *testing.T) {
	root := evidenceRoot(t)
	ledger := witnessFixture()
	stores := make(chan *WitnessStore, 2)
	failures := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Go(func() {
			store, err := NewWitnessStore(root, ledger.Stream)
			if err != nil {
				failures <- err
			} else {
				stores <- store
			}
		})
	}
	group.Wait()
	close(stores)
	close(failures)
	if len(stores) != 1 || len(failures) != 1 || !errors.Is(<-failures, ErrEvidenceExists) {
		t.Fatal("concurrent owners admitted")
	}
	store := <-stores
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	receipts := make(chan ArtifactReceipt, 2)
	for range 2 {
		group.Go(func() {
			receipt, err := store.Append(ledger.Records[0])
			if err != nil {
				t.Error(err)
			}
			receipts <- receipt
		})
	}
	group.Wait()
	first, second := <-receipts, <-receipts
	if first != second {
		t.Fatal("identical concurrent requests produced different receipts")
	}
	if len(evidenceSnapshot(t, root)) != 2 {
		t.Fatal("concurrent retry duplicated a record")
	}
}

func TestWitnessStoreFinalizeAccountsForConcurrentAppend(t *testing.T) {
	for trial := range 16 {
		root := evidenceRoot(t)
		ledger := witnessFixture()
		store := witnessStore(t, root, ledger.Stream)
		commitWitness(t, store, ledger.Records[:1])
		appendResult := make(chan error, 1)
		finalReceipt := make(chan ArtifactReceipt, 1)
		finalResult := make(chan error, 1)
		start := make(chan struct{})
		var group sync.WaitGroup
		appendRecord := func() { <-start; _, err := store.Append(ledger.Records[1]); appendResult <- err }
		finalize := func() { <-start; receipt, err := store.Finalize(); finalReceipt <- receipt; finalResult <- err }
		if trial%2 == 0 {
			group.Go(appendRecord)
			group.Go(finalize)
		} else {
			group.Go(finalize)
			group.Go(appendRecord)
		}
		close(start)
		group.Wait()
		final, err := <-finalReceipt, <-finalResult
		if err != nil {
			t.Fatal(err)
		}
		raw, err := LoadWitnessLedger(root, ledger.Stream, final)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeRecord[WitnessLedger](raw, maxWitnessBytes)
		if err != nil {
			t.Fatal(err)
		}
		switch appendErr := <-appendResult; {
		case appendErr == nil:
			if decoded.FinalSequence != 2 {
				t.Fatal("accepted concurrent commit omitted by finalization")
			}
		case errors.Is(appendErr, ErrWitnessSealed):
			if decoded.FinalSequence != 1 {
				t.Fatal("unaccepted concurrent commit entered ledger")
			}
		default:
			t.Fatal(appendErr)
		}
	}
}

func TestWitnessStoreChecksReservationBeforeNewCommit(t *testing.T) {
	for _, name := range []string{"changed reservation", "missing reservation", "public root"} {
		t.Run(name, func(t *testing.T) {
			root := evidenceRoot(t)
			ledger := witnessFixture()
			store := witnessStore(t, root, ledger.Stream)
			start := ledger.Stream.RunID + ".witness.start.json"
			switch name {
			case "changed reservation":
				if err := root.WriteFile(start, []byte(privateSentinel), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing reservation":
				if err := root.Remove(start); err != nil {
					t.Fatal(err)
				}
			case "public root":
				if err := os.Chmod(root.Name(), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			receipt, err := store.Append(ledger.Records[0])
			requireWitnessError(t, receipt, err, ErrEvidenceIncomplete)
			if _, statErr := root.Lstat(witnessEntryName(ledger.Stream.RunID, 1)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("commit was persisted after reservation became invalid")
			}
		})
	}
}

func TestWitnessStoreRootOwnershipAndLoaderRejection(t *testing.T) {
	ledger := witnessFixture()
	if store, err := NewWitnessStore(nil, ledger.Stream); store != nil || !errors.Is(err, ErrEvidenceRoot) {
		t.Fatal("nil root allowed")
	}
	bad := ledger.Stream
	bad.Generation = 0
	if store, err := NewWitnessStore(evidenceRoot(t), bad); store != nil || !errors.Is(err, ErrInvalidWitness) {
		t.Fatal("invalid stream allowed")
	}
	root := evidenceRoot(t)
	if err := os.Chmod(root.Name(), 0o755); err != nil {
		t.Fatal(err)
	}
	if store, err := NewWitnessStore(root, ledger.Stream); store != nil || !errors.Is(err, ErrEvidenceRoot) {
		t.Fatal("public root allowed")
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if store, err := NewWitnessStore(root, ledger.Stream); store != nil || !errors.Is(err, ErrEvidenceRoot) {
		t.Fatal("closed root allowed")
	}
	root = evidenceRoot(t)
	store := witnessStore(t, root, ledger.Stream)
	original := root.Name()
	moved := original + "-moved"
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Rename(moved, original); err != nil {
			t.Error(err)
		}
	})
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	commitWitness(t, store, ledger.Records)
	final, err := store.Finalize()
	if err != nil {
		t.Fatal("caller root close or rename redirected owned handle", err)
	}
	reopened, err := os.OpenRoot(moved)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, name := range []string{"healthy", "wrong receipt", "wrong stream", "symlink"} {
		t.Run(name, func(t *testing.T) {
			expected, receipt := ledger.Stream, final
			switch name {
			case "wrong receipt":
				receipt.SHA256 = strings.Repeat("0", 64)
			case "wrong stream":
				expected.Generation++
			case "symlink":
				if err := reopened.Rename(final.Name, "preserved-final"); err != nil {
					t.Fatal(err)
				}
				if err := reopened.Symlink("preserved-final", final.Name); err != nil {
					t.Fatal(err)
				}
			}
			raw, loadErr := LoadWitnessLedger(reopened, expected, receipt)
			if name == "healthy" {
				if loadErr != nil || !bytes.Equal(raw, witnessBytes(t, ledger)) {
					t.Fatal("valid final ledger rejected", loadErr)
				}
			} else if !errors.Is(loadErr, ErrEvidenceCorrupt) || raw != nil {
				t.Fatal("invalid final ledger exposed bytes", loadErr)
			}
		})
	}
}
