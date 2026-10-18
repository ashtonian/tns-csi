//go:build linux || darwin

package performance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const privateSentinel = "private-evidence-never-echo"

func retainedEvidence(t *testing.T, capture CapturedProcess) (*os.Root, CaptureResult) {
	t.Helper()
	root := evidenceRoot(t)
	result, err := evidenceStore(t, root).RecordCapture(capture)
	if err != nil || result.Index == nil {
		t.Fatalf("capture not retained: %v", err)
	}
	return root, result
}

func evidenceBytes(t *testing.T, root *os.Root, name string) []byte {
	t.Helper()
	raw, err := root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func replaceEvidence(t *testing.T, root *os.Root, name string, raw []byte) ArtifactReceipt {
	t.Helper()
	if err := root.WriteFile(name, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	return ArtifactReceipt{Name: name, Bytes: uint64(len(raw)), SHA256: hex.EncodeToString(hash[:])}
}

func editIndex(t *testing.T, root *os.Root, index ArtifactReceipt, edit func(map[string]any)) ArtifactReceipt {
	t.Helper()
	raw := editOutput(t, evidenceBytes(t, root, index.Name), edit)
	return replaceEvidence(t, root, index.Name, raw)
}

func replaceCaptureArtifact(t *testing.T, root *os.Root, result CaptureResult, name string, raw []byte) ArtifactReceipt {
	t.Helper()
	receipt := replaceEvidence(t, root, name, raw)
	return editIndex(t, root, *result.Index, func(document map[string]any) {
		for _, value := range array(t, document["artifacts"]) {
			artifact := object(t, value)
			if artifact["name"] == name {
				artifact["bytes"], artifact["sha256"] = receipt.Bytes, receipt.SHA256
			}
		}
	})
}

func evidenceSnapshot(t *testing.T, root *os.Root) map[string]string {
	t.Helper()
	directory, err := root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("snapshot directory: %v, %v", readErr, closeErr)
	}
	snapshot := make(map[string]string, len(entries))
	for _, entry := range entries {
		snapshot[entry.Name()] = string(evidenceBytes(t, root, entry.Name()))
	}
	return snapshot
}

func requireRejectedEvidence(t *testing.T, root *os.Root, index ArtifactReceipt, want error) {
	t.Helper()
	result, err := VerifyEvidence(root, index)
	if !errors.Is(err, want) || result.Evaluation.Status != Fail || result.Index != nil ||
		result.Evaluation.VerifiedBytes != nil || len(result.Evaluation.Jobs) != 0 || len(result.Artifacts) != 0 {

		t.Fatalf("unverified evidence accepted: %+v, %v", result, err)
	}
	raw, marshalErr := json.Marshal(result)
	if marshalErr != nil || strings.Contains(string(raw), privateSentinel) || strings.Contains(err.Error(), privateSentinel) {
		t.Fatal("rejection exposed untrusted evidence")
	}
}

func TestVerifyEvidenceRoundTrips(t *testing.T) {
	for _, name := range []string{"healthy", "reordered", "missing jobs", "malformed stdout", "unknown exit", "undrained", "job error", "empty stderr"} {
		t.Run(name, func(t *testing.T) {
			capture := healthyCapture(t)
			switch name {
			case "reordered":
				capture.Stdout = editOutput(t, capture.Stdout, func(document map[string]any) {
					jobs := array(t, document["jobs"])
					jobs[0], jobs[1] = jobs[1], jobs[0]
				})
			case "missing jobs":
				capture.Stdout = editOutput(t, capture.Stdout, func(document map[string]any) { document["jobs"] = []any{} })
			case "malformed stdout":
				capture.Stdout = []byte(privateSentinel)
			case "unknown exit":
				capture.Process.ExitCode = nil
			case "undrained":
				capture.Process.ChildrenExited = false
			case "job error":
				capture.Stdout = editJob(t, capture.Stdout, 1, func(job map[string]any) { job["error"] = 5 })
			case "empty stderr":
				capture.Stderr = nil
			}
			root, saved := retainedEvidence(t, capture)
			before := evidenceSnapshot(t, root)
			for range 8 {
				got, err := VerifyEvidence(root, *saved.Index)
				if err != nil || !reflect.DeepEqual(got, saved) || got.Evaluation.VerifiedBytes != nil {
					t.Fatalf("capture changed on read-back: %+v, %v", got, err)
				}
			}
			if !reflect.DeepEqual(before, evidenceSnapshot(t, root)) {
				t.Fatal("verification modified evidence")
			}
		})
	}
}

func TestVerifyEvidenceRejectsChangedOrMissingFiles(t *testing.T) {
	for _, suffix := range []string{"manifest.json", "stdout.raw", "stderr.raw", "process.json", "result.json"} {
		for _, change := range []string{"content", "short", "append", "missing", "public"} {
			t.Run(suffix+"/"+change, func(t *testing.T) {
				root, saved := retainedEvidence(t, healthyCapture(t))
				name := testManifest(t).RunID + "." + suffix
				raw := evidenceBytes(t, root, name)
				switch change {
				case "content":
					raw[0] ^= 1
					replaceEvidence(t, root, name, raw)
				case "short":
					replaceEvidence(t, root, name, raw[:len(raw)-1])
				case "append":
					replaceEvidence(t, root, name, append(raw, '\n'))
				case "missing":
					if err := root.Remove(name); err != nil {
						t.Fatal(err)
					}
				case "public":
					if err := root.Chmod(name, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				before := evidenceSnapshot(t, root)
				requireRejectedEvidence(t, root, *saved.Index, ErrEvidenceCorrupt)
				if !reflect.DeepEqual(before, evidenceSnapshot(t, root)) {
					t.Fatal("rejection changed retained files")
				}
			})
		}
	}
}

func TestVerifyEvidenceRefusesInvalidIndexReceipts(t *testing.T) {
	root, saved := retainedEvidence(t, healthyCapture(t))
	for name, change := range map[string]func(*ArtifactReceipt){
		"traversal": func(index *ArtifactReceipt) { index.Name = "../" + index.Name },
		"absolute":  func(index *ArtifactReceipt) { index.Name = "/" + index.Name },
		"suffix":    func(index *ArtifactReceipt) { index.Name += ".extra" },
		"run":       func(index *ArtifactReceipt) { index.Name = privateSentinel + "/run.result.json" },
		"hash":      func(index *ArtifactReceipt) { index.SHA256 = privateSentinel },
		"uppercase": func(index *ArtifactReceipt) { index.SHA256 = strings.Repeat("A", 64) },
		"zero":      func(index *ArtifactReceipt) { index.Bytes = 0 },
		"oversize":  func(index *ArtifactReceipt) { index.Bytes = maxIndexBytes + 1 },
		"overflow":  func(index *ArtifactReceipt) { index.Bytes = math.MaxUint64 },
	} {
		t.Run(name, func(t *testing.T) {
			index := *saved.Index
			change(&index)
			requireRejectedEvidence(t, root, index, ErrEvidenceCorrupt)
		})
	}
}

func TestVerifyEvidenceRejectsInvalidIndexRecords(t *testing.T) {
	for name, change := range map[string]func(*testing.T, map[string]any){
		"missing version": func(_ *testing.T, document map[string]any) { delete(document, "schema_version") },
		"unknown field":   func(_ *testing.T, document map[string]any) { document[privateSentinel] = privateSentinel },
		"missing index":   func(_ *testing.T, document map[string]any) { delete(document, "index") },
		"self receipt": func(t *testing.T, document map[string]any) {
			t.Helper()
			document["index"] = array(t, document["artifacts"])[0]
		},
		"missing artifact": func(t *testing.T, document map[string]any) {
			t.Helper()
			document["artifacts"] = array(t, document["artifacts"])[:3]
		},
		"extra artifact": func(t *testing.T, document map[string]any) {
			t.Helper()
			artifacts := array(t, document["artifacts"])
			document["artifacts"] = append(artifacts, artifacts[0])
		},
		"duplicate artifact": func(t *testing.T, document map[string]any) {
			t.Helper()
			artifacts := array(t, document["artifacts"])
			artifacts[3] = artifacts[0]
		},
		"cross run": func(t *testing.T, document map[string]any) {
			t.Helper()
			object(t, array(t, document["artifacts"])[0])["name"] = "another.manifest.json"
		},
		"artifact overflow": func(t *testing.T, document map[string]any) {
			t.Helper()
			object(t, array(t, document["artifacts"])[0])["bytes"] = uint64(math.MaxUint64)
		},
		"artifact hash": func(t *testing.T, document map[string]any) {
			t.Helper()
			object(t, array(t, document["artifacts"])[0])["sha256"] = privateSentinel
		},
		"invented checksum": func(t *testing.T, document map[string]any) {
			t.Helper()
			object(t, document["evaluation"])["verified_bytes"] = 4096
		},
		"changed observation": func(t *testing.T, document map[string]any) {
			t.Helper()
			object(t, array(t, object(t, document["evaluation"])["jobs"])[0])["name"] = privateSentinel
		},
		"invented failure": func(t *testing.T, document map[string]any) {
			t.Helper()
			object(t, document["evaluation"])["status"] = "FAIL"
		},
	} {
		t.Run(name, func(t *testing.T) {
			root, saved := retainedEvidence(t, healthyCapture(t))
			index := editIndex(t, root, *saved.Index, func(document map[string]any) { change(t, document) })
			requireRejectedEvidence(t, root, index, ErrEvidenceCorrupt)
		})
	}
}

func TestVerifyEvidenceRejectsForgedPassAndUnsupportedVersion(t *testing.T) {
	capture := healthyCapture(t)
	capture.Process.ChildrenExited = false
	root, saved := retainedEvidence(t, capture)
	index := editIndex(t, root, *saved.Index, func(document map[string]any) {
		object(t, document["evaluation"])["status"] = "PASS"
	})
	requireRejectedEvidence(t, root, index, ErrEvidenceCorrupt)
	index = editIndex(t, root, index, func(document map[string]any) { document["schema_version"] = EvidenceVersion + 1 })
	requireRejectedEvidence(t, root, index, ErrEvidenceVersion)
}

func TestVerifyEvidenceStrictProcessAndManifest(t *testing.T) {
	for name, change := range map[string]func(map[string]any){
		"missing cancel":  func(document map[string]any) { delete(document, "canceled") },
		"null cancel":     func(document map[string]any) { document["canceled"] = nil },
		"missing exit":    func(document map[string]any) { delete(document, "exit_code") },
		"wrong type":      func(document map[string]any) { document["canceled"] = 0 },
		"unknown signal":  func(document map[string]any) { document["signal"] = privateSentinel },
		"unknown process": func(document map[string]any) { document[privateSentinel] = privateSentinel },
		"wrong run":       func(document map[string]any) { document["run_id"] = "another-run" },
	} {
		t.Run(name, func(t *testing.T) {
			root, saved := retainedEvidence(t, healthyCapture(t))
			suffix := ".process.json"
			if name == "wrong run" {
				suffix = ".manifest.json"
			}
			filename := testManifest(t).RunID + suffix
			raw := editOutput(t, evidenceBytes(t, root, filename), change)
			index := replaceCaptureArtifact(t, root, saved, filename, raw)
			requireRejectedEvidence(t, root, index, ErrEvidenceCorrupt)
		})
	}
}

func TestVerifyEvidenceRefusesSymlinksAndNonregularFiles(t *testing.T) {
	for _, suffix := range []string{"manifest.json", "stdout.raw", "stderr.raw", "process.json", "result.json"} {
		for _, kind := range []string{"symlink", "fifo", "directory"} {
			t.Run(suffix+"/"+kind, func(t *testing.T) {
				root, saved := retainedEvidence(t, healthyCapture(t))
				name := testManifest(t).RunID + "." + suffix
				raw := evidenceBytes(t, root, name)
				if err := root.Remove(name); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "symlink":
					replaceEvidence(t, root, "matching-target", raw)
					if err := root.Symlink("matching-target", name); err != nil {
						t.Fatal(err)
					}
				case "fifo":
					if err := unix.Mkfifo(filepath.Join(root.Name(), name), 0o600); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := root.Mkdir(name, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				requireRejectedEvidence(t, root, *saved.Index, ErrEvidenceCorrupt)
				if kind == "symlink" && !bytes.Equal(raw, evidenceBytes(t, root, "matching-target")) {
					t.Fatal("symlink rejection modified target")
				}
			})
		}
	}
}

func TestVerifyEvidencePinsDirectoryAndPreservesCallerRoot(t *testing.T) {
	root, saved := retainedEvidence(t, healthyCapture(t))
	before := evidenceBytes(t, root, saved.Index.Name)
	name := root.Name()
	if err := os.Rename(name, name+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(name, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(name); err != nil {
			t.Error(err)
		}
		if err := os.Rename(name+"-moved", name); err != nil {
			t.Error(err)
		}
	})
	if _, err := VerifyEvidence(root, *saved.Index); err != nil {
		t.Fatalf("rename redirected evidence: %v", err)
	}
	if !bytes.Equal(before, evidenceBytes(t, root, saved.Index.Name)) {
		t.Fatal("caller root no longer usable")
	}
	entries, err := os.ReadDir(name)
	if err != nil || len(entries) != 0 {
		t.Fatal("verification used or modified the replacement directory")
	}
}

func TestVerifyEvidenceRejectsUnavailableRoots(t *testing.T) {
	root, saved := retainedEvidence(t, healthyCapture(t))
	requireRejectedEvidence(t, nil, *saved.Index, ErrEvidenceRoot)
	if err := root.Chmod(".", 0o755); err != nil {
		t.Fatal(err)
	}
	requireRejectedEvidence(t, root, *saved.Index, ErrEvidenceRoot)
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	requireRejectedEvidence(t, root, *saved.Index, ErrEvidenceRoot)
}

func TestVerifyEvidenceRejectsOversizedStreamsWithoutTruncation(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			root := evidenceRoot(t)
			capture := healthyCapture(t)
			oversized := bytes.Repeat([]byte("x"), maxFioBytes+1)
			if stream == "stdout" {
				capture.Stdout = oversized
			} else {
				capture.Stderr = oversized
			}
			saved, err := evidenceStore(t, root).RecordCapture(capture)
			if !errors.Is(err, ErrEvidenceIncomplete) || saved.Index == nil || saved.Evaluation.Status != Fail {
				t.Fatal("oversized evidence was finalized as complete")
			}
			filename := testManifest(t).RunID + "." + stream + ".raw"
			if !bytes.Equal(oversized, evidenceBytes(t, root, filename)) {
				t.Fatal("oversized raw capture was truncated")
			}
			requireRejectedEvidence(t, root, *saved.Index, ErrEvidenceCorrupt)
		})
	}
}
