package performance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func evidenceRoot(t *testing.T) *os.Root {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func evidenceStore(t *testing.T, root *os.Root) *EvidenceStore {
	t.Helper()
	store, err := NewEvidenceStore(root, testManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func healthyCapture(t *testing.T) CapturedProcess {
	t.Helper()
	return CapturedProcess{Process: successfulProcess(), Stdout: fixture(t, "fio-success.json"), Stderr: []byte("synthetic diagnostic\n")}
}

func TestEvidenceCaptureReceiptsAndRetention(t *testing.T) {
	root := evidenceRoot(t)
	store := evidenceStore(t, root)
	capture := healthyCapture(t)
	result, err := store.RecordCapture(capture)
	if err != nil || result.Evaluation.Status != Pass || len(result.Artifacts) != 4 || result.Index == nil {
		t.Fatalf("capture failed: %+v (%v)", result, err)
	}
	receipts := append(append([]ArtifactReceipt(nil), result.Artifacts...), *result.Index)
	for _, receipt := range receipts {
		raw, readErr := root.ReadFile(receipt.Name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		hash := sha256.Sum256(raw)
		if uint64(len(raw)) != receipt.Bytes || hex.EncodeToString(hash[:]) != receipt.SHA256 {
			t.Fatalf("invalid artifact receipt: %+v", receipt)
		}
		info, statErr := root.Stat(receipt.Name)
		if statErr != nil || info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("artifact not private: %v", statErr)
		}
	}
	stdout, err := root.ReadFile(store.filename("stdout"))
	if err != nil || !bytes.Equal(stdout, capture.Stdout) {
		t.Fatal("stdout not preserved exactly")
	}
	stderr, err := root.ReadFile(store.filename("stderr"))
	if err != nil || !bytes.Equal(stderr, capture.Stderr) {
		t.Fatal("stderr not preserved exactly")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := root.ReadFile(result.Index.Name); err != nil {
		t.Fatal("closing the store removed evidence or closed caller's root")
	}
}

func TestEvidenceRetainsFailedAndIncompleteCapture(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfirmed child", true: "malformed stdout"}[malformed], func(t *testing.T) {
			root := evidenceRoot(t)
			store := evidenceStore(t, root)
			capture := healthyCapture(t)
			if malformed {
				capture.Stdout = []byte("fio: synthetic error before JSON\n{}")
			} else {
				capture.Process.ChildrenExited = false
			}
			result, err := store.RecordCapture(capture)
			if err != nil || result.Evaluation.Status != Fail || result.Index == nil {
				t.Fatalf("failed evaluation did not retain complete evidence: %+v (%v)", result, err)
			}
			raw, err := root.ReadFile(store.filename("stdout"))
			if err != nil || !bytes.Equal(raw, capture.Stdout) {
				t.Fatal("failure output changed")
			}
		})
	}
}

func TestEvidenceRefusesReuseAndOrphans(t *testing.T) {
	root := evidenceRoot(t)
	manifest := testManifest(t)
	store := evidenceStore(t, root)
	if _, err := NewEvidenceStore(root, manifest); !errors.Is(err, ErrEvidenceExists) {
		t.Fatalf("existing run accepted: %v", err)
	}
	if _, err := store.RecordCapture(healthyCapture(t)); err != nil {
		t.Fatal(err)
	}
	result, err := store.RecordCapture(CapturedProcess{Stdout: []byte("replacement")})
	if !errors.Is(err, ErrCaptureRecorded) || result.Evaluation.Status != Fail {
		t.Fatal("second capture did not fail")
	}
	raw, err := root.ReadFile(store.filename("stdout"))
	if err != nil || !bytes.Equal(raw, fixture(t, "fio-success.json")) {
		t.Fatal("second capture overwrote original evidence")
	}
	orphanRoot := evidenceRoot(t)
	orphan := manifest.RunID + ".stderr.raw"
	if err := orphanRoot.WriteFile(orphan, []byte("prior orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEvidenceStore(orphanRoot, manifest); !errors.Is(err, ErrEvidenceExists) {
		t.Fatalf("orphaned artifact accepted: %v", err)
	}
}

func TestEvidenceCollisionDoesNotFollowSymlinkOrHideFailure(t *testing.T) {
	root := evidenceRoot(t)
	store := evidenceStore(t, root)
	if err := root.WriteFile("protected", []byte("do not overwrite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink("protected", store.filename("stdout")); err != nil {
		t.Fatal(err)
	}
	result, err := store.RecordCapture(healthyCapture(t))
	if !errors.Is(err, ErrEvidenceIncomplete) || result.Evaluation.Status != Fail || !hasCode(result.Evaluation, "EVIDENCE_INCOMPLETE") {
		t.Fatalf("persistence failure hidden: %+v (%v)", result, err)
	}
	raw, err := root.ReadFile("protected")
	if err != nil || string(raw) != "do not overwrite" {
		t.Fatal("artifact symlink followed")
	}
	if _, err := root.ReadFile(store.filename("stderr")); err != nil || result.Index == nil {
		t.Fatal("one artifact failure discarded other streams or the failing index")
	}
	if _, err := store.RecordCapture(healthyCapture(t)); !errors.Is(err, ErrCaptureRecorded) {
		t.Fatal("failed capture was retried")
	}
}

func TestEvidenceFinalIndexCollisionFailsClosed(t *testing.T) {
	root := evidenceRoot(t)
	store := evidenceStore(t, root)
	if err := root.WriteFile(store.filename("result"), []byte("prior result"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := store.RecordCapture(healthyCapture(t))
	if err == nil || result.Index != nil || result.Evaluation.Status != Fail {
		t.Fatalf("finalization failure accepted: %+v (%v)", result, err)
	}
	raw, err := root.ReadFile(store.filename("result"))
	if err != nil || string(raw) != "prior result" {
		t.Fatal("existing final index replaced")
	}
}

func TestEvidenceRejectsUnsafeRootAndClosedStore(t *testing.T) {
	manifest := testManifest(t)
	if _, err := NewEvidenceStore(nil, manifest); !errors.Is(err, ErrEvidenceRoot) {
		t.Fatal("nil root accepted")
	}
	root := evidenceRoot(t)
	if err := root.Chmod(".", 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEvidenceStore(root, manifest); !errors.Is(err, ErrEvidenceRoot) {
		t.Fatalf("non-private root accepted: %v", err)
	}
	if err := root.Chmod(".", 0o700); err != nil {
		t.Fatal(err)
	}
	store := evidenceStore(t, root)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := store.RecordCapture(healthyCapture(t))
	if !errors.Is(err, ErrEvidenceClosed) || result.Evaluation.Status != Fail {
		t.Fatal("closed store accepted capture")
	}
}

func TestEvidenceConcurrentReservationAndRecording(t *testing.T) {
	root := evidenceRoot(t)
	manifest := testManifest(t)
	var wg sync.WaitGroup
	stores := make(chan *EvidenceStore, 8)
	for range 8 {
		wg.Go(func() {
			store, err := NewEvidenceStore(root, manifest)
			if err == nil {
				stores <- store
			} else if !errors.Is(err, ErrEvidenceExists) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	close(stores)
	if len(stores) != 1 {
		t.Fatalf("expected one reservation, got %d", len(stores))
	}
	store := <-stores
	defer store.Close()
	capture := healthyCapture(t)
	results := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := store.RecordCapture(capture)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrCaptureRecorded) {
			t.Error(err)
		}
	}
	if successes != 1 {
		t.Fatalf("expected one capture, got %d", successes)
	}
}

func TestEvidencePinsDirectoryAndManifest(t *testing.T) {
	parent := t.TempDir()
	name := filepath.Join(parent, "evidence")
	if err := os.Mkdir(name, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(name)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	manifest := testManifest(t)
	store, err := NewEvidenceStore(root, manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manifest.Jobs[0].Name = "mutated"
	if renameErr := os.Rename(name, name+"-moved"); renameErr != nil {
		t.Fatal(renameErr)
	}
	if mkdirErr := os.Mkdir(name, 0o700); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	result, err := store.RecordCapture(healthyCapture(t))
	if err != nil || result.Evaluation.Status != Pass {
		t.Fatalf("directory or manifest did not stay pinned: %v", err)
	}
	entries, err := os.ReadDir(name)
	if err != nil || len(entries) != 0 {
		t.Fatal("renamed path redirected evidence writes")
	}
}

func TestEvidenceSanitizesProcessSignal(t *testing.T) {
	root := evidenceRoot(t)
	store := evidenceStore(t, root)
	capture := healthyCapture(t)
	const sentinel = "private-token-never-echo"
	capture.Process.Signal = sentinel
	result, err := store.RecordCapture(capture)
	if !errors.Is(err, ErrEvidenceIncomplete) || result.Evaluation.Status != Fail {
		t.Fatal("invalid receipt signal accepted")
	}
	for _, kind := range []string{"process", "result"} {
		raw, err := root.ReadFile(store.filename(kind))
		if err != nil || strings.Contains(string(raw), sentinel) || !json.Valid(raw) {
			t.Fatal("untrusted process string leaked or record invalid")
		}
	}
}

type faultFile struct {
	writeErr error
	syncErr  error
	closeErr error
	events   []string
	short    bool
}

func (f *faultFile) Write(data []byte) (int, error) {
	f.events = append(f.events, "write")
	if f.short {
		return len(data) - 1, f.writeErr
	}
	return len(data), f.writeErr
}

func (f *faultFile) Sync() error {
	f.events = append(f.events, "sync")
	return f.syncErr
}

func (f *faultFile) Close() error {
	f.events = append(f.events, "close")
	return f.closeErr
}

func TestEvidenceShortWriteSyncAndCloseFailures(t *testing.T) {
	privateErr := errors.New("private-storage-details")
	for name, file := range map[string]*faultFile{
		"short": {short: true}, "write": {writeErr: privateErr},
		"sync": {syncErr: privateErr}, "close": {closeErr: privateErr},
	} {
		t.Run(name, func(t *testing.T) {
			err := writeSynced(file, []byte("data"))
			if !errors.Is(err, ErrEvidenceIncomplete) || strings.Contains(err.Error(), privateErr.Error()) {
				t.Fatal("write failure accepted or echoed")
			}
			if file.events[len(file.events)-1] != "close" {
				t.Fatal("failed write leaked descriptor")
			}
			if (name == "short" || name == "write") && strings.Contains(strings.Join(file.events, ","), "sync") {
				t.Fatal("incomplete write was synced as if complete")
			}
		})
	}
}
