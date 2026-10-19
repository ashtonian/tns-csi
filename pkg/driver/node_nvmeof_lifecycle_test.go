package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNVMeReuseRetainsUnreadyConnection(t *testing.T) {
	useFakeSysBlock(t, map[string]string{}, map[string]string{})
	fakeSysClassNVMe(t, map[string]string{"nvme4": nqnA})
	fakeNVMeTools(t, "", errors.New("CLI unavailable"))
	s := NewNodeService("test", nil, true, nil, false, 1)
	disconnects := 0
	s.disconnectNVMeOFFn = func(context.Context, string) error {
		disconnects++
		return nil
	}
	resp, _, err := s.tryReuseExistingConnection(context.Background(), &nvmeOFConnectionParams{nqn: nqnA}, "volume", filepath.Join(t.TempDir(), "stage"), nil, false, nil)
	if resp != nil || status.Code(err) != codes.Unavailable || disconnects != 0 {
		t.Fatalf("reuse = %v, %v; disconnects=%d; want Unavailable without disconnect", resp, err, disconnects)
	}
}

func TestNVMeReusePreservesUnknownDiscovery(t *testing.T) {
	useFakeSysBlock(t, map[string]string{}, map[string]string{})
	fakeNVMeTools(t, "", errors.New("CLI unavailable"))
	root := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(root, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	orig := sysClassNVMePath
	sysClassNVMePath = root
	t.Cleanup(func() { sysClassNVMePath = orig })
	s := NewNodeService("test", nil, true, nil, false, 1)
	resp, _, err := s.tryReuseExistingConnection(context.Background(), &nvmeOFConnectionParams{nqn: nqnA}, "volume", filepath.Join(t.TempDir(), "stage"), nil, false, nil)
	if resp != nil || status.Code(err) != codes.Unavailable {
		t.Fatalf("reuse = %v, %v; want Unavailable rather than connect fallback", resp, err)
	}
}

func TestNVMeStagingMetadataRejectsIdentityConflict(t *testing.T) {
	stage := filepath.Join(t.TempDir(), "stage")
	if err := writeNVMeStagingNQN(stage, nqnA); err != nil {
		t.Fatal(err)
	}
	if err := writeNVMeStagingNQN(stage, nqnB); err == nil {
		t.Fatal("conflicting stage replaced retained NQN")
	}
	if got, err := readNVMeStagingNQN(stage); err != nil || got != nqnA {
		t.Fatalf("retained NQN = %q, %v; want original identity", got, err)
	}
}

func TestNVMeRemovalRequiresReadableIdentities(t *testing.T) {
	for _, state := range []string{"missing-root", "missing-identity", "empty-identity", "unreadable-identity"} {
		t.Run(state, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "nvme")
			if state != "missing-root" {
				if err := os.MkdirAll(filepath.Join(root, "nvme4"), 0o750); err != nil {
					t.Fatal(err)
				}
			}
			identity := filepath.Join(root, "nvme4", "subsysnqn")
			if state == "empty-identity" {
				if err := os.WriteFile(identity, []byte("\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if state == "unreadable-identity" {
				if err := os.Mkdir(identity, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			if present, err := nvmeNQNPresent(root, nqnA); err == nil {
				t.Fatalf("presence = %v, nil; incomplete evidence certified absence", present)
			}
		})
	}
}

// The kernel removes a controller's /sys/class/nvme link before its attributes, so an
// entry that vanished after listing is absent, not unknown.
func TestNVMeControllerRemovedAfterListingIsAbsent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nvme")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "removed-nvme4"), filepath.Join(root, "nvme4")); err != nil {
		t.Fatal(err)
	}
	writeTestSubsystemNQN(t, root, "nvme5", nqnB)
	if present, err := nvmeNQNPresent(root, nqnA); err != nil || present {
		t.Fatalf("presence = %v, %v; want false, nil", present, err)
	}
	useFakeSysBlock(t, map[string]string{}, map[string]string{})
	origClass := sysClassNVMePath
	sysClassNVMePath = root
	t.Cleanup(func() { sysClassNVMePath = origClass })
	s := NewNodeService("test", nil, true, nil, false, 1)
	if _, _, err := s.findNVMeDeviceFromSys(context.Background(), nqnA); !errors.Is(err, ErrNVMeDeviceNotFound) {
		t.Fatalf("sysfs lookup = %v; want ErrNVMeDeviceNotFound", err)
	}
}

func TestNVMeNamespaceLookupPreservesIdentityReadFailure(t *testing.T) {
	block := fakeSysBlock(t, map[string]string{"nvme4": nqnA}, map[string]string{"nvme4n1": "nvme4"}, nil)
	identity := filepath.Join(block, "nvme4n1", "device", "subsysnqn")
	if err := os.Remove(identity); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(identity, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := nvmeNamespaceDeviceForNQN(block, nqnA); err == nil || errors.Is(err, ErrNVMeDeviceNotFound) {
		t.Fatalf("lookup error = %v; want unreadable identity rather than absence", err)
	}
}
