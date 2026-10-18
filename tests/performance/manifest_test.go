package performance

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func testManifest(t *testing.T) Manifest {
	t.Helper()
	manifest, err := DecodeManifest(fixture(t, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestManifestGuards(t *testing.T) {
	t.Parallel()
	cases := []struct {
		change func(*Manifest)
		name   string
	}{
		{name: "unknown schema", change: func(m *Manifest) { m.SchemaVersion++ }},
		{name: "missing run", change: func(m *Manifest) { m.RunID = "" }},
		{name: "unknown scenario", change: func(m *Manifest) { m.ScenarioID = "B99" }},
		{name: "unknown phase", change: func(m *Manifest) { m.Phase = "randrw" }},
		{name: "unknown fio", change: func(m *Manifest) { m.FioVersion = "fio-next" }},
		{name: "mutable image", change: func(m *Manifest) { m.RunnerImage = "example.invalid/tools:latest" }},
		{name: "invalid digest", change: func(m *Manifest) { m.RunnerImage = "repo@sha256:" + strings.Repeat("z", 64) }},
		{name: "Q01 missing PVC", change: func(m *Manifest) { m.Identity.PVCUID = "" }},
		{name: "Q01 claimRef mismatch", change: func(m *Manifest) { m.Identity.ClaimRefUID = "other-pvc" }},
		{name: "Q01 missing node", change: func(m *Manifest) { m.Identity.NodeUID = "" }},
		{name: "Q02 prefix collision", change: func(m *Manifest) { m.Identity.Dataset = "tank/lab-ab/volume-a" }},
		{name: "Q02 parent traversal", change: func(m *Manifest) { m.DatasetParent = "tank/../lab-a" }},
		{name: "Q02 filename traversal", change: func(m *Manifest) { m.Jobs[0].Filename = "../other" }},
		{name: "Q02 absolute filename", change: func(m *Manifest) { m.Jobs[0].Filename = "/dev/nvme0n1" }},
		{name: "Q06 raw mount", change: func(m *Manifest) { m.Identity.MountPath = "/dev/nvme0n1" }},
		{name: "Q06 mount traversal", change: func(m *Manifest) { m.Identity.MountPath = "/mnt/lab/../other" }},
		{name: "Q06 missing UUID", change: func(m *Manifest) { m.Identity.FilesystemUUID = "" }},
		{name: "Q06 zero UUID", change: func(m *Manifest) { m.Identity.NamespaceUUID = "00000000-0000-0000-0000-000000000000" }},
		{name: "Q06 wrong filesystem", change: func(m *Manifest) { m.Identity.FilesystemType = "nfs" }},
		{name: "Q08 file capacity", change: func(m *Manifest) { m.Budget.CapacityBytes = 20479 }},
		{name: "Q08 memory budget", change: func(m *Manifest) { m.Budget.MaxMemoryBytes-- }},
		{name: "Q08 missing headroom", change: func(m *Manifest) { m.Budget.HeadroomBytes = 0 }},
		{name: "Q08 memory overflow", change: func(m *Manifest) { m.Jobs[0].Depth = math.MaxUint64 }},
		{name: "Q08 offset overflow", change: func(m *Manifest) { m.Jobs[0].OffsetBytes = math.MaxUint64 - 4095 }},
		{name: "Q08 capacity overflow", change: func(m *Manifest) { m.Budget.HeadroomBytes = math.MaxUint64 }},
		{name: "Q13 shared file", change: func(m *Manifest) { m.Jobs[1].Filename = m.Jobs[0].Filename }},
		{name: "duplicate job", change: func(m *Manifest) { m.Jobs[1].Name = m.Jobs[0].Name }},
		{name: "unaligned size", change: func(m *Manifest) { m.Jobs[0].SizeBytes++ }},
		{name: "unaligned offset", change: func(m *Manifest) { m.Jobs[0].OffsetBytes++ }},
		{name: "zero block size", change: func(m *Manifest) { m.Jobs[0].BlockSizeBytes = 0 }},
		{name: "no jobs", change: func(m *Manifest) { m.Jobs = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			manifest := testManifest(t)
			tc.change(&manifest)
			if err := manifest.Validate(); !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("expected invalid manifest, got %v", err)
			}
		})
	}
}

func TestManifestBudgetBoundary(t *testing.T) {
	manifest := testManifest(t)
	manifest.Budget.CapacityBytes = 20480
	if err := manifest.Validate(); err != nil {
		t.Fatalf("exact capacity and memory budgets must pass: %v", err)
	}
	manifest.Identity.FilesystemType = "ext4"
	manifest.Phase = "read"
	if err := manifest.Validate(); err != nil {
		t.Fatalf("ext4/read compatibility: %v", err)
	}
}

func TestObservedIdentityMustMatchEveryField(t *testing.T) {
	manifest := testManifest(t)
	if err := manifest.CheckIdentity(manifest.Identity); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		change func(*Identity)
		name   string
	}{
		{name: "PVC", change: func(id *Identity) { id.PVCUID = "different" }},
		{name: "PV", change: func(id *Identity) { id.PVUID = "different" }},
		{name: "claimRef", change: func(id *Identity) { id.ClaimRefUID = "different" }},
		{name: "node", change: func(id *Identity) { id.NodeUID = "different" }},
		{name: "NQN", change: func(id *Identity) { id.NQN = "nqn.2026-10.example.lab:other" }},
		{name: "NSID", change: func(id *Identity) { id.NSID++ }},
		{name: "namespace", change: func(id *Identity) { id.NamespaceUUID = "33333333-3333-4333-8333-333333333333" }},
		{name: "filesystem", change: func(id *Identity) { id.FilesystemUUID = "" }},
		{name: "mount", change: func(id *Identity) { id.MountPath = "/mnt/other" }},
		{name: "backend", change: func(id *Identity) { id.Dataset = "tank/lab-ab/volume-a" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := manifest.Identity
			tc.change(&observed)
			if err := manifest.CheckIdentity(observed); !errors.Is(err, ErrIdentityMismatch) {
				t.Fatalf("changed identity accepted: %v", err)
			}
		})
	}
}

func TestDecodeManifestRejectsAmbiguityWithoutEcho(t *testing.T) {
	valid := fixture(t, "manifest.json")
	const sentinel = "private-sentinel-do-not-echo"
	cases := map[string][]byte{
		"empty":                nil,
		"null":                 []byte("null"),
		"array":                []byte("[]"),
		"truncated":            valid[:len(valid)/2],
		"second document":      append(bytes.Clone(valid), valid...),
		"duplicate top key":    bytes.Replace(valid, []byte(`"schema_version": 1`), []byte(`"schema_version": 2, "schema_version": 1`), 1),
		"duplicate nested key": bytes.Replace(valid, []byte(`"nsid": 1`), []byte(`"nsid": 2, "nsid": 1`), 1),
		"unknown key":          bytes.Replace(valid, []byte(`"schema_version": 1`), []byte(`"api_key": "`+sentinel+`", "schema_version": 1`), 1),
		"nested secret":        bytes.Replace(valid, []byte(`"nsid": 1`), []byte(`"token": "`+sentinel+`", "nsid": 1`), 1),
		"wrong case":           bytes.Replace(valid, []byte(`"run_id"`), []byte(`"RUN_ID"`), 1),
		"missing zero offset":  bytes.Replace(valid, []byte(`"offset_bytes": 0,`), nil, 1),
		"null zero offset":     bytes.Replace(valid, []byte(`"offset_bytes": 0`), []byte(`"offset_bytes": null`), 1),
		"fractional counter":   bytes.Replace(valid, []byte(`"offset_bytes": 0`), []byte(`"offset_bytes": 0.1`), 1),
		"overflow counter":     bytes.Replace(valid, []byte(`"offset_bytes": 0`), []byte(`"offset_bytes": 18446744073709551616`), 1),
		"negative counter":     bytes.Replace(valid, []byte(`"offset_bytes": 0`), []byte(`"offset_bytes": -1`), 1),
		"oversized request":    bytes.Repeat([]byte(" "), (1<<20)+1),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			manifest, err := DecodeManifest(data)
			if !errors.Is(err, ErrInvalidManifest) || manifest.RunID != "" {
				t.Fatalf("expected empty request and sanitized error: %v", err)
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Fatal("credential echoed in error")
			}
		})
	}
}

func TestDecodeManifestPreservesLargeIntegers(t *testing.T) {
	manifest := testManifest(t)
	manifest.Jobs = manifest.Jobs[:1]
	manifest.Jobs[0].SizeBytes = (1 << 53) + 4096
	manifest.Budget.CapacityBytes = math.MaxUint64
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeManifest(data)
	if err != nil || decoded.Jobs[0].SizeBytes != manifest.Jobs[0].SizeBytes {
		t.Fatalf("integer rounded or rejected: %v", err)
	}
}

func FuzzDecodeManifest(f *testing.F) {
	data, err := os.ReadFile("testdata/manifest.json")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(data)
	f.Add([]byte(`{"schema_version":1,"api_key":"sentinel"}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		manifest, err := DecodeManifest(raw)
		if err == nil && manifest.Validate() != nil {
			t.Fatal("decoder accepted invalid request")
		}
		if err != nil && err.Error() != ErrInvalidManifest.Error() {
			t.Fatal("decoder returned raw input details")
		}
	})
}
