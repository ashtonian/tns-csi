//go:build linux || darwin

package performance

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func savedChecksumPlan(t *testing.T) (*os.Root, ArtifactReceipt, Manifest) {
	t.Helper()
	root := evidenceRoot(t)
	manifest, expected, _ := checksumFixture(t)
	receipt, err := SaveChecksumPlan(root, manifest, expected)
	if err != nil {
		t.Fatal(err)
	}
	return root, receipt, manifest
}

func requireRejectedChecksumPlan(t *testing.T, root *os.Root, receipt ArtifactReceipt, manifest Manifest, want error) {
	t.Helper()
	expected, err := LoadChecksumPlan(root, receipt, manifest)
	if !errors.Is(err, want) || expected != nil {
		t.Fatalf("rejected checksum plan exposed expectations: %v (%v)", expected, err)
	}
	if strings.Contains(err.Error(), privateSentinel) {
		t.Fatal("checksum-plan error exposed input")
	}
}

func TestChecksumPlanSavedBeforeDataAndReopened(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "healthy", true: "corrupt"}[corrupt], func(t *testing.T) {
			root := evidenceRoot(t)
			manifest, expected, data := checksumFixture(t)
			receipt, err := SaveChecksumPlan(root, manifest, expected)
			if err != nil {
				t.Fatal(err)
			}
			raw := evidenceBytes(t, root, receipt.Name)
			// Expectations are retained before the scratch data files are created.
			sources := reopenedChecksumFiles(t, data, func(path string) {
				if corrupt && filepath.Base(path) == manifest.Jobs[1].Filename {
					changed := bytes.Clone(data[manifest.Jobs[1].Filename])
					changed[manifest.Jobs[1].OffsetBytes] ^= 1
					if writeErr := os.WriteFile(path, changed, 0o600); writeErr != nil {
						t.Fatal(writeErr)
					}
				}
			})
			directory := root.Name()
			if closeErr := root.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			reopened, openErr := os.OpenRoot(directory)
			if openErr != nil {
				t.Fatal(openErr)
			}
			defer reopened.Close()
			loaded, loadErr := LoadChecksumPlan(reopened, receipt, manifest)
			if loadErr != nil || !reflect.DeepEqual(loaded, expected) {
				t.Fatalf("saved plan did not round trip: %v", loadErr)
			}
			result := VerifyChecksums(context.Background(), manifest, loaded, sources)
			if corrupt {
				if result.Status != Fail || result.VerifiedBytes != nil || !integrityHasCode(result, "CHECKSUM_MISMATCH") {
					t.Fatal("retained expectations did not detect data corruption")
				}
			} else if result.Status != Pass || result.VerifiedBytes == nil || *result.VerifiedBytes != 12288 {
				t.Fatal("retained expectations failed healthy data")
			}
			if !bytes.Equal(raw, evidenceBytes(t, reopened, receipt.Name)) {
				t.Fatal("loading or verification modified checksum evidence")
			}
		})
	}
}

func TestChecksumPlanRejectsInvalidInputsBeforeCreation(t *testing.T) {
	for name, change := range map[string]func(*Manifest, *[]ExtentChecksum){
		"write phase":      func(m *Manifest, _ *[]ExtentChecksum) { m.Phase = "write" },
		"invalid manifest": func(m *Manifest, _ *[]ExtentChecksum) { m.RunID = privateSentinel + "/" },
		"missing digest":   func(_ *Manifest, e *[]ExtentChecksum) { *e = (*e)[:1] },
		"duplicate job":    func(_ *Manifest, e *[]ExtentChecksum) { (*e)[1] = (*e)[0] },
		"changed extent":   func(_ *Manifest, e *[]ExtentChecksum) { (*e)[0].Plan.OffsetBytes += 4096 },
		"invalid digest":   func(_ *Manifest, e *[]ExtentChecksum) { (*e)[0].SHA256 = privateSentinel },
		"oversize plan": func(m *Manifest, _ *[]ExtentChecksum) {
			m.RunnerImage = strings.Repeat("a", maxChecksumPlanBytes) + "@sha256:" + strings.Repeat("0", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := evidenceRoot(t)
			manifest, expected, _ := checksumFixture(t)
			change(&manifest, &expected)
			receipt, err := SaveChecksumPlan(root, manifest, expected)
			if !errors.Is(err, ErrInvalidChecksumPlan) || receipt != (ArtifactReceipt{}) {
				t.Fatal("invalid plan produced a trusted receipt")
			}
			if len(evidenceSnapshot(t, root)) != 0 {
				t.Fatal("invalid plan reserved an artifact")
			}
		})
	}
}

func TestChecksumPlanExclusiveWriters(t *testing.T) {
	root := evidenceRoot(t)
	manifest, expected, _ := checksumFixture(t)
	var group sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for range 2 {
		group.Go(func() {
			_, err := SaveChecksumPlan(root, manifest, expected)
			errorsSeen <- err
		})
	}
	group.Wait()
	close(errorsSeen)
	var saved, refused int
	for err := range errorsSeen {
		switch {
		case err == nil:
			saved++
		case errors.Is(err, ErrEvidenceExists):
			refused++
		default:
			t.Fatal(err)
		}
	}
	if saved != 1 || refused != 1 {
		t.Fatal("concurrent plan writers did not preserve exclusive creation")
	}
}

func TestChecksumPlanStrictRecordsAndBindings(t *testing.T) {
	for name, change := range map[string]func(map[string]any){
		"unknown":       func(d map[string]any) { d[privateSentinel] = true },
		"missing":       func(d map[string]any) { delete(d, "schema_version") },
		"null version":  func(d map[string]any) { d["schema_version"] = nil },
		"null expected": func(d map[string]any) { d["expected"] = nil },
		"null job":      func(d map[string]any) { array(t, d["expected"])[0] = nil },
		"cross run":     func(d map[string]any) { object(t, d["manifest"])["run_id"] = "other-run" },
		"different identity": func(d map[string]any) {
			object(t, object(t, d["manifest"])["identity"])["volume_handle"] = "other-volume"
		},
		"write phase": func(d map[string]any) { object(t, d["manifest"])["phase"] = "write" },
		"changed budget": func(d map[string]any) {
			object(t, object(t, d["manifest"])["budget"])["capacity_bytes"] = uint64(65536)
		},
		"missing job": func(d map[string]any) { d["expected"] = array(t, d["expected"])[:1] },
		"duplicate job": func(d map[string]any) {
			jobs := array(t, d["expected"])
			jobs[1] = jobs[0]
		},
		"invalid digest": func(d map[string]any) { object(t, array(t, d["expected"])[0])["sha256"] = privateSentinel },
		"changed extent": func(d map[string]any) {
			object(t, object(t, array(t, d["expected"])[0])["plan"])["offset_bytes"] = uint64(4096)
		},
	} {
		t.Run(name, func(t *testing.T) {
			root, receipt, manifest := savedChecksumPlan(t)
			raw := editOutput(t, evidenceBytes(t, root, receipt.Name), change)
			receipt = replaceEvidence(t, root, receipt.Name, raw)
			requireRejectedChecksumPlan(t, root, receipt, manifest, ErrEvidenceCorrupt)
		})
	}
	for name, transform := range map[string]func([]byte) []byte{
		"duplicate": func(raw []byte) []byte {
			return bytes.Replace(raw, []byte(`"schema_version":1`), []byte(`"schema_version":1,"schema_version":1`), 1)
		},
		"Unicode alias":     func(raw []byte) []byte { return bytes.Replace(raw, []byte(`"sha256":`), []byte(`"ſha256":`), 1) },
		"trailing document": func(raw []byte) []byte { return append(raw, []byte(`{}`)...) },
	} {
		t.Run(name, func(t *testing.T) {
			root, receipt, manifest := savedChecksumPlan(t)
			receipt = replaceEvidence(t, root, receipt.Name, transform(evidenceBytes(t, root, receipt.Name)))
			requireRejectedChecksumPlan(t, root, receipt, manifest, ErrEvidenceCorrupt)
		})
	}
	root, receipt, manifest := savedChecksumPlan(t)
	raw := editOutput(t, evidenceBytes(t, root, receipt.Name), func(d map[string]any) { d["schema_version"] = uint64(2) })
	receipt = replaceEvidence(t, root, receipt.Name, raw)
	requireRejectedChecksumPlan(t, root, receipt, manifest, ErrEvidenceVersion)
}

func TestChecksumPlanRejectsChangedReceiptOrFile(t *testing.T) {
	for name, change := range map[string]func(*ArtifactReceipt){
		"path":           func(r *ArtifactReceipt) { r.Name = "../" + r.Name },
		"different run":  func(r *ArtifactReceipt) { r.Name = "other-run.checksums.json" },
		"zero size":      func(r *ArtifactReceipt) { r.Bytes = 0 },
		"wrong size":     func(r *ArtifactReceipt) { r.Bytes++ },
		"oversize":       func(r *ArtifactReceipt) { r.Bytes = maxChecksumPlanBytes + 1 },
		"bad hash":       func(r *ArtifactReceipt) { r.SHA256 = privateSentinel },
		"uppercase hash": func(r *ArtifactReceipt) { r.SHA256 = strings.ToUpper(r.SHA256) },
	} {
		t.Run(name, func(t *testing.T) {
			root, receipt, manifest := savedChecksumPlan(t)
			change(&receipt)
			requireRejectedChecksumPlan(t, root, receipt, manifest, ErrEvidenceCorrupt)
		})
	}
	for _, name := range []string{"changed bytes", "missing", "public file", "symlink", "directory"} {
		t.Run(name, func(t *testing.T) {
			root, receipt, manifest := savedChecksumPlan(t)
			raw := evidenceBytes(t, root, receipt.Name)
			switch name {
			case "changed bytes":
				raw[0] = ' '
				replaceEvidence(t, root, receipt.Name, raw)
			case "missing":
				if err := root.Remove(receipt.Name); err != nil {
					t.Fatal(err)
				}
			case "public file":
				if err := root.Chmod(receipt.Name, 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := root.Rename(receipt.Name, "original"); err != nil {
					t.Fatal(err)
				}
				if err := root.Symlink("original", receipt.Name); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := root.Remove(receipt.Name); err != nil {
					t.Fatal(err)
				}
				if err := root.Mkdir(receipt.Name, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			requireRejectedChecksumPlan(t, root, receipt, manifest, ErrEvidenceCorrupt)
		})
	}
}

func TestChecksumPlanRootsAndExistingArtifacts(t *testing.T) {
	manifest, expected, _ := checksumFixture(t)
	if receipt, err := SaveChecksumPlan(nil, manifest, expected); !errors.Is(err, ErrEvidenceRoot) || receipt != (ArtifactReceipt{}) {
		t.Fatal("nil root allowed save")
	}
	root, receipt, manifest := savedChecksumPlan(t)
	requireRejectedChecksumPlan(t, nil, receipt, manifest, ErrEvidenceRoot)
	for _, name := range []string{"existing", "symlink"} {
		t.Run(name, func(t *testing.T) {
			private := evidenceRoot(t)
			filename := manifest.RunID + ".checksums.json"
			if name == "existing" {
				if err := private.WriteFile(filename, []byte(privateSentinel), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := private.Symlink("missing", filename); err != nil {
					t.Fatal(err)
				}
			}
			if saved, err := SaveChecksumPlan(private, manifest, expected); !errors.Is(err, ErrEvidenceExists) || saved != (ArtifactReceipt{}) {
				t.Fatal("existing artifact was replaced")
			}
			if name == "existing" && string(evidenceBytes(t, private, filename)) != privateSentinel {
				t.Fatal("existing artifact changed")
			}
		})
	}
	if err := os.Chmod(root.Name(), 0o755); err != nil {
		t.Fatal(err)
	}
	requireRejectedChecksumPlan(t, root, receipt, manifest, ErrEvidenceRoot)
	if _, err := SaveChecksumPlan(root, manifest, expected); !errors.Is(err, ErrEvidenceRoot) {
		t.Fatal("public root allowed save")
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	requireRejectedChecksumPlan(t, root, receipt, manifest, ErrEvidenceRoot)
	if _, err := SaveChecksumPlan(root, manifest, expected); !errors.Is(err, ErrEvidenceRoot) {
		t.Fatal("closed root allowed save")
	}
}

func TestChecksumPlanPinnedDirectoryAndCallerManifest(t *testing.T) {
	root, receipt, manifest := savedChecksumPlan(t)
	original := root.Name()
	if err := os.Rename(original, original+"-moved"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(original+"-moved", original) })
	if _, err := LoadChecksumPlan(root, receipt, manifest); err != nil {
		t.Fatal("renamed pinned root lost evidence", err)
	}
	manifest.Identity.VolumeHandle = "different-volume"
	requireRejectedChecksumPlan(t, root, receipt, manifest, ErrEvidenceCorrupt)
	manifest.Phase = "write"
	requireRejectedChecksumPlan(t, root, receipt, manifest, ErrInvalidChecksumPlan)
}
