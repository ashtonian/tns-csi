//go:build linux || darwin

package performance

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func TestWitnessTargetPinnedDirectoryAndReadOnlyRecovery(t *testing.T) {
	plan := witnessFilePlan()
	base := t.TempDir()
	original, moved := filepath.Join(base, "original"), filepath.Join(base, "retained")
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(original)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	before, err := root.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	if renameErr := os.Rename(original, moved); renameErr != nil {
		t.Fatal(renameErr)
	}
	if mkdirErr := os.Mkdir(original, 0o700); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	replacement := filepath.Join(original, witnessTargetName(plan))
	sentinel := []byte("replacement directory must stay untouched")
	if writeErr := os.WriteFile(replacement, sentinel, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	target, parent, err := CreateWitnessTarget(root, plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeWitnessFiles(target, parent) })
	after, err := parent.Stat()
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("target parent was rediscovered by path", err)
	}
	if closeErr := root.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	storeRoot := evidenceRoot(t)
	store := witnessStore(t, storeRoot, plan.Stream)
	writer, err := NewWitnessFileWriter(plan, target, parent, store)
	if err != nil {
		t.Fatal(err)
	}
	written, err := writer.Append(context.Background(), []byte("caller root can close before the write"))
	if err != nil || written.Receipt == nil {
		t.Fatal(err)
	}
	writer.Close()
	if closeErr := closeWitnessFiles(target, parent); closeErr != nil {
		t.Fatal(closeErr)
	}
	receipt, err := store.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := LoadWitnessLedger(storeRoot, plan.Stream, receipt)
	if err != nil {
		t.Fatal(err)
	}
	recoveryRoot, err := os.OpenRoot(moved)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recoveryRoot.Close() })
	if renameErr := os.Rename(moved, filepath.Join(base, "recovered")); renameErr != nil {
		t.Fatal(renameErr)
	}
	if mkdirErr := os.Mkdir(moved, 0o700); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	recoveryReplacement := filepath.Join(moved, witnessTargetName(plan))
	if writeErr := os.WriteFile(recoveryReplacement, sentinel, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	source, err := OpenWitnessSource(recoveryRoot, plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	if closeErr := recoveryRoot.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if _, writeErr := source.WriteAt([]byte{0}, 0); writeErr == nil {
		t.Fatal("recovery handle permits writes")
	}
	result := VerifyWitnessFile(context.Background(), plan, source, receipt, raw, []WitnessRecord{written.Record})
	if result.Status != Pass || result.VerifiedBytes == nil || *result.VerifiedBytes != written.Record.PayloadBytes {
		t.Fatalf("anchored target failed recovery: %+v", result)
	}
	info, err := source.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatal("target is not a private regular file", err)
	}
	for _, name := range []string{replacement, recoveryReplacement} {
		unchanged, readErr := os.ReadFile(name)
		if readErr != nil || !bytes.Equal(unchanged, sentinel) {
			t.Fatal("renamed root redirected target I/O")
		}
	}
}

func TestWitnessTargetRefusesExistingNamesAndRetainsBytes(t *testing.T) {
	for _, kind := range []string{"file", "internal symlink", "external symlink", "dangling symlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			root := evidenceRoot(t)
			plan := witnessFilePlan()
			sentinel := witnessTargetFixture(t, root, plan, kind)
			target, parent, err := CreateWitnessTarget(root, plan)
			if target != nil || parent != nil || !errors.Is(err, ErrEvidenceExists) {
				t.Fatal("existing target name was opened or replaced", err)
			}
			if sentinel != "" {
				got, readErr := os.ReadFile(sentinel)
				if readErr != nil || string(got) != privateSentinel {
					t.Fatal("existing file/link destination changed")
				}
			}
		})
	}
}

func TestWitnessSourceRefusesSymlinksNonregularAndUnboundedFiles(t *testing.T) {
	for _, kind := range []string{"internal symlink", "external symlink", "dangling symlink", "directory", "fifo", "missing", "public", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			root := evidenceRoot(t)
			plan := witnessFilePlan()
			_ = witnessTargetFixture(t, root, plan, kind)
			source, err := OpenWitnessSource(root, plan)
			if source != nil || !errors.Is(err, ErrEvidenceCorrupt) || strings.Contains(err.Error(), privateSentinel) {
				t.Fatal("unsafe source was opened or leaked private input", err)
			}
		})
	}
}

func witnessTargetFixture(t *testing.T, root *os.Root, plan WitnessFilePlan, kind string) string {
	t.Helper()
	name := filepath.Join(root.Name(), witnessTargetName(plan))
	switch kind {
	case "file", "public", "oversized":
		if err := os.WriteFile(name, []byte(privateSentinel), 0o600); err != nil {
			t.Fatal(err)
		}
		if kind == "public" {
			if err := os.Chmod(name, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if kind == "oversized" {
			if err := os.Truncate(name, int64(plan.CapacityBytes+1)); err != nil {
				t.Fatal(err)
			}
		}
		return name
	case "internal symlink", "external symlink", "dangling symlink":
		destination := filepath.Join(root.Name(), "other.bin")
		if kind == "external symlink" {
			destination = filepath.Join(t.TempDir(), "outside.bin")
		}
		if kind != "dangling symlink" {
			if err := os.WriteFile(destination, []byte(privateSentinel), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		link := destination
		if kind == "internal symlink" {
			link = "other.bin"
		}
		if err := os.Symlink(link, name); err != nil {
			t.Fatal(err)
		}
		if kind != "dangling symlink" {
			return destination
		}
	case "directory":
		if err := os.Mkdir(name, 0o700); err != nil {
			t.Fatal(err)
		}
	case "fifo":
		if err := unix.Mkfifo(name, 0o600); err != nil {
			t.Fatal(err)
		}
	case "missing":
	default:
		t.Fatal("unknown fixture kind")
	}
	return ""
}

func TestWitnessTargetRejectsInvalidPlanAndRootBeforeCreation(t *testing.T) {
	for _, kind := range []string{"run path", "capacity", "nil root", "closed root", "public root"} {
		t.Run(kind, func(t *testing.T) {
			root := evidenceRoot(t)
			plan := witnessFilePlan()
			expected := ErrEvidenceRoot
			switch kind {
			case "run path":
				plan.Stream.RunID = "../outside"
				expected = ErrInvalidWitness
			case "capacity":
				plan.CapacityBytes = 0
				expected = ErrInvalidWitness
			case "nil root":
				root = nil
			case "closed root":
				if err := root.Close(); err != nil {
					t.Fatal(err)
				}
			case "public root":
				if err := os.Chmod(root.Name(), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			target, parent, err := CreateWitnessTarget(root, plan)
			if target != nil || parent != nil || !errors.Is(err, expected) {
				t.Fatal("invalid creation request admitted", err)
			}
			if source, sourceErr := OpenWitnessSource(root, plan); source != nil || !errors.Is(sourceErr, expected) {
				t.Fatal("invalid source request admitted", sourceErr)
			}
			if root != nil {
				entries, readErr := os.ReadDir(root.Name())
				if readErr != nil || len(entries) != 0 {
					t.Fatal("invalid request created target files", readErr)
				}
			}
		})
	}
}

func TestWitnessTargetExclusiveConcurrentCreationAndNoResume(t *testing.T) {
	root := evidenceRoot(t)
	plan := witnessFilePlan()
	var workers sync.WaitGroup
	outcomes := make(chan error, 2)
	for range 2 {
		workers.Go(func() {
			target, parent, err := CreateWitnessTarget(root, plan)
			if err == nil {
				err = closeWitnessFiles(target, parent)
			}
			outcomes <- err
		})
	}
	workers.Wait()
	close(outcomes)
	winners, conflicts := 0, 0
	for err := range outcomes {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrEvidenceExists):
			conflicts++
		default:
			t.Fatal("unexpected concurrent create error", err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatal("concurrent requests did not reserve exactly one file")
	}
	plan.Stream.Generation++
	if target, parent, err := CreateWitnessTarget(root, plan); target != nil || parent != nil || !errors.Is(err, ErrEvidenceExists) {
		t.Fatal("new generation resumed or replaced an existing target", err)
	}
	source, err := OpenWitnessSource(root, plan)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil || info.Size() != 0 {
		t.Fatal("abandoned empty target was not retained", err)
	}
}
