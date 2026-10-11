package driver

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const (
	nqnA = "nqn.2011-06.com.truenas:uuid:a:pvc-a"
	nqnB = "nqn.2011-06.com.truenas:uuid:b:pvc-b"
)

// fakeSysBlock builds /sys/block-style entries. owners maps an owner directory name
// (controller "nvme1" or subsystem "nvme-subsys0") to its NQN; devs maps a block device
// name to its owner.
func fakeSysBlock(t *testing.T, owners, devs, nsids map[string]string) string {
	t.Helper()
	root := t.TempDir()
	block := filepath.Join(root, "block")
	ownersDir := filepath.Join(root, "owners")
	for owner, nqn := range owners {
		dir := filepath.Join(ownersDir, owner)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "subsysnqn"), []byte(nqn+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for dev, owner := range devs {
		dir := filepath.Join(block, dev)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(ownersDir, owner), filepath.Join(dir, "device")); err != nil {
			t.Fatal(err)
		}
		nsid := "1"
		if v, ok := nsids[dev]; ok {
			nsid = v
		}
		if err := os.WriteFile(filepath.Join(dir, "nsid"), []byte(nsid+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return block
}

// With native NVMe multipath, the visible block device is named after the subsystem
// instance, not the controller: controller nvme0 can belong to subsystem nvme-subsys1,
// whose namespace is /dev/nvme1n1. Building "/dev/<controller>n1" then picks ANOTHER
// volume's disk. Resolution must follow the device's own subsysnqn.
func TestNVMeNamespaceDeviceForNQNWithMultipathNumbering(t *testing.T) {
	block := fakeSysBlock(t,
		map[string]string{"nvme-subsys0": nqnA, "nvme-subsys1": nqnB, "nvme0": nqnB, "nvme1": nqnA},
		map[string]string{
			"nvme0n1":   "nvme-subsys0", // head device for subsystem 0 (NQN A)
			"nvme1n1":   "nvme-subsys1", // head device for subsystem 1 (NQN B)
			"nvme0c1n1": "nvme1",        // hidden per-path device: must be ignored
			"nvme1c0n1": "nvme0",        // hidden per-path device: must be ignored
		}, nil)

	for nqn, want := range map[string]string{nqnA: "/dev/nvme0n1", nqnB: "/dev/nvme1n1"} {
		got, err := nvmeNamespaceDeviceForNQN(block, nqn)
		if err != nil || got != want {
			t.Errorf("nvmeNamespaceDeviceForNQN(%s) = %q, %v; want %q", nqn, got, err, want)
		}
	}
}

func TestNVMeNamespaceDeviceForNQNWithoutMultipath(t *testing.T) {
	block := fakeSysBlock(t,
		map[string]string{"nvme3": nqnA},
		map[string]string{"nvme3n1": "nvme3", "nvme3n1p1": "nvme3"}, nil)

	got, err := nvmeNamespaceDeviceForNQN(block, nqnA)
	if err != nil || got != "/dev/nvme3n1" {
		t.Fatalf("nvmeNamespaceDeviceForNQN() = %q, %v; want /dev/nvme3n1 (partitions ignored)", got, err)
	}
}

func TestNVMeNamespaceDeviceForNQNNotFound(t *testing.T) {
	block := fakeSysBlock(t, map[string]string{"nvme0": nqnA}, map[string]string{"nvme0n1": "nvme0"}, nil)
	if _, err := nvmeNamespaceDeviceForNQN(block, nqnB); !errors.Is(err, ErrNVMeDeviceNotFound) {
		t.Fatalf("nvmeNamespaceDeviceForNQN(unknown) error = %v, want ErrNVMeDeviceNotFound", err)
	}
}

// Volumes use independent subsystems with NSID 1. If a subsystem ever exposes several
// namespaces, pick NSID 1; never guess between ambiguous candidates.
func TestNVMeNamespaceDeviceForNQNPrefersNSID1AndFailsClosedOnAmbiguity(t *testing.T) {
	block := fakeSysBlock(t,
		map[string]string{"nvme-subsys0": nqnA},
		map[string]string{"nvme0n1": "nvme-subsys0", "nvme0n2": "nvme-subsys0"},
		map[string]string{"nvme0n1": "1", "nvme0n2": "2"})
	got, err := nvmeNamespaceDeviceForNQN(block, nqnA)
	if err != nil || got != "/dev/nvme0n1" {
		t.Fatalf("nvmeNamespaceDeviceForNQN() = %q, %v; want /dev/nvme0n1", got, err)
	}

	block = fakeSysBlock(t,
		map[string]string{"nvme-subsys0": nqnA},
		map[string]string{"nvme0n2": "nvme-subsys0", "nvme0n3": "nvme-subsys0"},
		map[string]string{"nvme0n2": "2", "nvme0n3": "3"})
	if got, err := nvmeNamespaceDeviceForNQN(block, nqnA); err == nil {
		t.Fatalf("nvmeNamespaceDeviceForNQN() with two non-NSID-1 namespaces = %q, nil; want an error", got)
	}
}

// Reverse lookup (unstage without metadata): the NQN must come from the device's own
// owner, not from a controller name derived by stripping "n1".
func TestNVMeNQNForDeviceUsesDeviceOwner(t *testing.T) {
	block := fakeSysBlock(t,
		map[string]string{"nvme-subsys1": nqnB, "nvme1": nqnA},
		map[string]string{"nvme1n1": "nvme-subsys1"}, nil)
	got, err := nvmeNQNForDevice(block, "/dev/nvme1n1")
	if err != nil || got != nqnB {
		t.Fatalf("nvmeNQNForDevice(/dev/nvme1n1) = %q, %v; want %q (controller nvme1 belongs to another NQN)", got, err, nqnB)
	}
}
