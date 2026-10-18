package performance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func checksumFixture(t *testing.T) (Manifest, []ExtentChecksum, map[string][]byte) {
	t.Helper()
	manifest := testManifest(t)
	manifest.Phase = "read"
	expected := make([]ExtentChecksum, 0, len(manifest.Jobs))
	data := make(map[string][]byte, len(manifest.Jobs))
	for index, plan := range manifest.Jobs {
		payload := make([]byte, plan.SizeBytes)
		for offset := range payload {
			payload[offset] = byte((offset+index)%251 + 1)
		}
		digest := sha256.Sum256(payload)
		expected = append(expected, ExtentChecksum{Plan: plan, SHA256: hex.EncodeToString(digest[:])})
		data[plan.Filename] = append(make([]byte, plan.OffsetBytes), payload...)
	}
	return manifest, expected, data
}

func memorySources(data map[string][]byte) map[string]io.ReaderAt {
	sources := make(map[string]io.ReaderAt, len(data))
	for name, raw := range data {
		sources[name] = bytes.NewReader(raw)
	}
	return sources
}

func integrityHasCode(result IntegrityResult, code string) bool {
	for _, finding := range result.Findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}

func TestVerifyChecksumsHealthyAndReorderedExpected(t *testing.T) {
	manifest, expected, data := checksumFixture(t)
	expected[0], expected[1] = expected[1], expected[0]
	result := VerifyChecksums(context.Background(), manifest, expected, memorySources(data))
	if result.Status != Pass || result.VerifiedBytes == nil || *result.VerifiedBytes != 12288 || len(result.Jobs) != 2 {
		t.Fatalf("finite checksum control failed: %+v", result)
	}
	for index, job := range result.Jobs {
		plan := manifest.Jobs[index]
		if job.Name != plan.Name || job.ReadBytes != plan.SizeBytes || job.VerifiedBytes == nil || *job.VerifiedBytes != plan.SizeBytes {
			t.Fatal("verification lost per-job extent coverage")
		}
	}
}

func reopenedChecksumFiles(t *testing.T, data map[string][]byte, change func(string)) map[string]io.ReaderAt {
	t.Helper()
	directory := t.TempDir()
	for name, raw := range data {
		path := filepath.Join(directory, name)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if writeErr := writeSynced(file, raw); writeErr != nil {
			t.Fatal(writeErr)
		}
		if change != nil {
			change(path)
		}
	}
	sources := make(map[string]io.ReaderAt, len(data))
	for name := range data {
		file, err := os.Open(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if closeErr := file.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
		sources[name] = file
	}
	return sources
}

func TestVerifyChecksumsScratchFilesDetectCorruptionAndShortRead(t *testing.T) {
	for _, name := range []string{"healthy", "one byte corrupt", "short file", "outside extent"} {
		t.Run(name, func(t *testing.T) {
			manifest, expected, data := checksumFixture(t)
			last := manifest.Jobs[1]
			original := bytes.Clone(data[last.Filename])
			changed := bytes.Clone(original)
			switch name {
			case "one byte corrupt":
				changed[last.OffsetBytes+1] ^= 1
			case "short file":
				changed = changed[:len(changed)-1]
			case "outside extent":
				changed[0] ^= 1
			}
			sources := reopenedChecksumFiles(t, data, func(path string) {
				if filepath.Base(path) == last.Filename {
					if err := os.WriteFile(path, changed, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			})
			result := VerifyChecksums(context.Background(), manifest, expected, sources)
			if name == "healthy" || name == "outside extent" {
				if result.Status != Pass || result.VerifiedBytes == nil || *result.VerifiedBytes != 12288 {
					t.Fatal("complete extent verification failed")
				}
			} else {
				code := "CHECKSUM_MISMATCH"
				if name == "short file" {
					code = "COVERAGE_INCOMPLETE"
				}
				if result.Status != Fail || result.VerifiedBytes != nil || !integrityHasCode(result, code) || result.Jobs[0].VerifiedBytes == nil {
					t.Fatalf("bad final extent hid failure or lost healthy coverage: %+v", result)
				}
			}
			retained := make([]byte, len(changed))
			if n, err := sources[last.Filename].ReadAt(retained, 0); err != nil || n != len(retained) || !bytes.Equal(retained, changed) {
				t.Fatal("verification rewrote failing data")
			}
		})
	}
}

type checksumReadFunc func([]byte, int64) (int, error)

func (read checksumReadFunc) ReadAt(data []byte, offset int64) (int, error) {
	return read(data, offset)
}

type checksumBoundaryContext struct {
	context.Context
	afterCheck func()
}

func (ctx checksumBoundaryContext) Err() error {
	err := ctx.Context.Err()
	ctx.afterCheck()
	return err
}

func TestVerifyChecksumsCancellationBetweenExtents(t *testing.T) {
	manifest, expected, data := checksumFixture(t)
	sources := memorySources(data)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finishedRead := false
	boundary := checksumBoundaryContext{Context: ctx, afterCheck: func() {
		if finishedRead {
			cancel()
		}
	}}
	first := sources[manifest.Jobs[0].Filename]
	sources[manifest.Jobs[0].Filename] = checksumReadFunc(func(buffer []byte, offset int64) (int, error) {
		n, err := first.ReadAt(buffer, offset)
		finishedRead = true
		return n, err
	})
	result := VerifyChecksums(boundary, manifest, expected, sources)
	if result.Status != Fail || result.VerifiedBytes != nil || len(result.Jobs) != 1 || !integrityHasCode(result, "PROCESS_ERROR") {
		t.Fatal("cancellation between extents manufactured a partial PASS")
	}
}

func TestVerifyChecksumsRejectsInvalidInputsBeforeReads(t *testing.T) {
	for _, name := range []string{"write phase", "bad manifest", "missing checksum", "duplicate checksum", "changed plan", "bad digest", "uppercase digest", "missing source", "nil source", "signed offset overflow"} {
		t.Run(name, func(t *testing.T) {
			manifest, expected, data := checksumFixture(t)
			reads := 0
			sources := memorySources(data)
			for filename := range sources {
				sources[filename] = checksumReadFunc(func(_ []byte, _ int64) (int, error) { reads++; return 0, io.EOF })
			}
			switch name {
			case "write phase":
				manifest.Phase = "write"
			case "bad manifest":
				manifest.Identity.PVCUID = ""
			case "missing checksum":
				expected = expected[:1]
			case "duplicate checksum":
				expected[1] = expected[0]
			case "changed plan":
				expected[1].Plan.OffsetBytes = 0
			case "bad digest":
				expected[1].SHA256 = "private-checksum-sentinel"
			case "uppercase digest":
				expected[1].SHA256 = strings.ToUpper(expected[1].SHA256)
			case "missing source":
				delete(sources, manifest.Jobs[1].Filename)
			case "nil source":
				sources[manifest.Jobs[1].Filename] = nil
			case "signed offset overflow":
				manifest.Budget.CapacityBytes = math.MaxUint64
				manifest.Jobs[1].SizeBytes = 1 << 63
				expected[1].Plan = manifest.Jobs[1]
			}
			result := VerifyChecksums(context.Background(), manifest, expected, sources)
			if reads != 0 || result.Status != Fail || result.VerifiedBytes != nil || !integrityHasCode(result, "INVALID_REQUEST") {
				t.Fatal("invalid extent map reached a reader or passed")
			}
		})
	}
}

func TestVerifyChecksumsReaderFailuresAreBoundedAndSanitized(t *testing.T) {
	for _, name := range []string{"zero progress", "short with nil", "negative count", "oversized count", "read error", "full EOF"} {
		t.Run(name, func(t *testing.T) {
			manifest, expected, data := checksumFixture(t)
			sources := memorySources(data)
			reads := 0
			last := manifest.Jobs[1]
			sources[last.Filename] = checksumReadFunc(func(buffer []byte, offset int64) (int, error) {
				reads++
				switch name {
				case "zero progress":
					return 0, nil
				case "short with nil":
					return len(buffer) - 1, nil
				case "negative count":
					return -1, nil
				case "oversized count":
					return len(buffer) + 1, nil
				case "read error":
					return len(buffer), errors.New("private-reader-sentinel")
				default:
					copy(buffer, data[last.Filename][offset:])
					return len(buffer), io.EOF
				}
			})
			result := VerifyChecksums(context.Background(), manifest, expected, sources)
			if reads != 1 {
				t.Fatal("failed or completed finite read was retried")
			}
			if name == "full EOF" {
				if result.Status != Pass {
					t.Fatal("ReaderAt's complete terminal EOF was rejected")
				}
			} else if result.Status != Fail || result.VerifiedBytes != nil || result.Jobs[1].VerifiedBytes != nil {
				t.Fatal("invalid reader result became verified coverage")
			}
			raw, err := json.Marshal(result)
			if err != nil || bytes.Contains(raw, []byte("private-reader-sentinel")) {
				t.Fatal("raw reader error escaped into evidence")
			}
		})
	}
}

func TestVerifyChecksumsCancellationAndInputPinning(t *testing.T) {
	manifest, expected, data := checksumFixture(t)
	sources := memorySources(data)
	first := sources[manifest.Jobs[0].Filename]
	sources[manifest.Jobs[0].Filename] = checksumReadFunc(func(buffer []byte, offset int64) (int, error) {
		manifest.Jobs[1].OffsetBytes = math.MaxUint64
		delete(sources, manifest.Jobs[1].Filename)
		expected[1].SHA256 = "replacement"
		return first.ReadAt(buffer, offset)
	})
	if result := VerifyChecksums(context.Background(), manifest, expected, sources); result.Status != Pass {
		t.Fatal("caller mutation redirected the pinned extent map")
	}
	manifest, expected, data = checksumFixture(t)
	manifest.Jobs = manifest.Jobs[:1]
	manifest.Jobs[0].Depth = 1
	manifest.Budget.MaxMemoryBytes = 4096
	expected = expected[:1]
	expected[0].Plan = manifest.Jobs[0]
	sources = memorySources(data)
	delete(sources, "job-b.bin")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	sources[manifest.Jobs[0].Filename] = checksumReadFunc(func(buffer []byte, offset int64) (int, error) {
		reads++
		cancel()
		return bytes.NewReader(data[manifest.Jobs[0].Filename]).ReadAt(buffer, offset)
	})
	result := VerifyChecksums(ctx, manifest, expected, sources)
	if reads != 1 || result.Status != Fail || result.VerifiedBytes != nil || result.Jobs[0].ReadBytes != 4096 || !integrityHasCode(result, "PROCESS_ERROR") {
		t.Fatal("cancellation allowed more reads or claimed verification")
	}
}

func TestVerifyChecksumsCancelledBeforeReading(t *testing.T) {
	manifest, expected, data := checksumFixture(t)
	reads := 0
	sources := memorySources(data)
	for filename := range sources {
		sources[filename] = checksumReadFunc(func(_ []byte, _ int64) (int, error) { reads++; return 0, io.EOF })
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := VerifyChecksums(ctx, manifest, expected, sources)
	if reads != 0 || result.Status != Fail || !integrityHasCode(result, "PROCESS_ERROR") || result.VerifiedBytes != nil {
		t.Fatal("canceled verification read data or passed")
	}
}
