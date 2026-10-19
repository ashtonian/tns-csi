package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fenio/tns-csi/pkg/mount"
)

func TestVerifyNVMeBlockStagingSource(t *testing.T) {
	useFakeSysBlock(t, map[string]string{"nvme4": nqnA, "nvme5": nqnB}, map[string]string{"nvme4n1": "nvme4", "nvme5n1": "nvme5"})
	origLookup := lookupMountedEntry
	lookupMountedEntry = func(context.Context, string) (mount.Entry, bool, error) { return mount.Entry{}, false, nil }
	t.Cleanup(func() { lookupMountedEntry = origLookup })
	devices := t.TempDir()
	for _, name := range []string{"nvme4n1", "nvme5n1"} {
		if err := os.WriteFile(filepath.Join(devices, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := func(device string) func(*testing.T, string) {
		return func(t *testing.T, path string) {
			t.Helper()
			if err := os.Symlink(filepath.Join(devices, device), path); err != nil {
				t.Fatal(err)
			}
		}
	}
	tests := []struct {
		setup   func(*testing.T, string)
		wantErr error
		name    string
	}{
		{name: "absent", setup: func(*testing.T, string) {}},
		{name: "kubelet placeholder directory", setup: func(t *testing.T, path string) {
			t.Helper()
			if err := os.Mkdir(path, 0o750); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink to requested namespace", setup: link("nvme4n1")},
		{name: "populated directory", wantErr: ErrNVMeIdentityConflict, setup: func(t *testing.T, path string) {
			t.Helper()
			if err := os.Mkdir(path, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "data"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "regular file", wantErr: ErrNVMeIdentityConflict, setup: func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink to another namespace", wantErr: ErrNVMeIdentityConflict, setup: link("nvme5n1")},
	}
	s := NewNodeService("test", nil, true, nil, false, 1)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "staging")
			tt.setup(t, path)
			if err := s.verifyNVMeStagingSource(context.Background(), path, nqnA, true); !errors.Is(err, tt.wantErr) {
				t.Fatalf("verifyNVMeStagingSource = %v; want %v", err, tt.wantErr)
			}
		})
	}
}

// kubelet creates the raw-block staging path as a directory before NodeStageVolume.
func TestStageBlockDeviceReplacesKubeletPlaceholder(t *testing.T) {
	device := filepath.Join(t.TempDir(), "nvme4n1")
	if err := os.WriteFile(device, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.Mkdir(staging, 0o750); err != nil {
		t.Fatal(err)
	}
	s := NewNodeService("test", nil, true, nil, false, 1)
	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := s.stageBlockDevice(device, staging); err != nil {
			t.Fatalf("stage attempt %d: %v", attempt, err)
		}
		if target, err := os.Readlink(staging); err != nil || target != device {
			t.Fatalf("stage attempt %d: staging link = %q, %v; want %q", attempt, target, err, device)
		}
	}
}
