package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeControllers adds controller entries with a state file under an owner directory
// built by fakeSysBlock: under an nvme-subsysN owner they are the subsystem's paths; for
// a controller owner ("nvme3") pass the owner itself to give it a state.
func fakeControllers(t *testing.T, block, owner string, states map[string]string) {
	t.Helper()
	ownerDir := filepath.Join(filepath.Dir(block), "owners", owner)
	for ctrl, state := range states {
		dir := ownerDir
		if ctrl != owner {
			dir = filepath.Join(ownerDir, ctrl)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "state"), []byte(state+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Under native multipath /dev/nvme1n1 is named after subsystem instance 1, and its
// controllers can be nvme0 and nvme2; "nvme1" may be a different volume's controller.
// The controller must come from the namespace's own sysfs owner.
func TestNVMeControllerForNamespace(t *testing.T) {
	block := fakeSysBlock(t,
		map[string]string{"nvme-subsys1": nqnB, "nvme3": nqnA, "nvme1": nqnA},
		map[string]string{"nvme1n1": "nvme-subsys1", "nvme3n1": "nvme3"}, nil)
	fakeControllers(t, block, "nvme-subsys1", map[string]string{"nvme0": "connecting", "nvme2": "live"})
	fakeControllers(t, block, "nvme3", map[string]string{"nvme3": "live"})
	fakeControllers(t, block, "nvme1", map[string]string{"nvme1": "connecting"}) // decoy: name-derived guess

	cases := []struct {
		dev, wantCtrl, wantState string
	}{
		{"/dev/nvme1n1", "/dev/nvme2", "live"}, // multipath: a live path of its own subsystem
		{"/dev/nvme3n1", "/dev/nvme3", "live"}, // no multipath: the owning controller
		{"/dev/nvme3n1p1", "/dev/nvme3", "live"},
	}
	for _, tc := range cases {
		ctrl, state, err := nvmeControllerForNamespace(block, tc.dev)
		if err != nil || ctrl != tc.wantCtrl || state != tc.wantState {
			t.Errorf("nvmeControllerForNamespace(%s) = %q, %q, %v; want %q, %q", tc.dev, ctrl, state, err, tc.wantCtrl, tc.wantState)
		}
	}
	if _, _, err := nvmeControllerForNamespace(block, "/dev/sda"); err == nil {
		t.Error("non-NVMe device resolved to a controller")
	}

	t.Run("no live path falls back to the first controller", func(t *testing.T) {
		b := fakeSysBlock(t, map[string]string{"nvme-subsys5": nqnA}, map[string]string{"nvme5n1": "nvme-subsys5"}, nil)
		fakeControllers(t, b, "nvme-subsys5", map[string]string{"nvme9": "connecting", "nvme4": "resetting"})
		ctrl, state, err := nvmeControllerForNamespace(b, "/dev/nvme5n1")
		if err != nil || ctrl != "/dev/nvme4" || state != "resetting" {
			t.Fatalf("got %q, %q, %v; want /dev/nvme4, resetting", ctrl, state, err)
		}
	})

	t.Run("health reads the resolved controller", func(t *testing.T) {
		orig := sysBlockPath
		sysBlockPath = block
		t.Cleanup(func() { sysBlockPath = orig })
		state, err := getNVMeControllerState("/dev/nvme1n1")
		if err != nil || state != "live" {
			t.Fatalf("getNVMeControllerState(/dev/nvme1n1) = %q, %v; want live (not the decoy nvme1)", state, err)
		}
	})
}

// fakeNVMeTools replaces nvme list-subsys and ns-rescan for one test and counts rescans.
func fakeNVMeTools(t *testing.T, listSubsys string, listErr error) *int {
	t.Helper()
	rescans := 0
	origList, origRescan := nvmeListSubsysCmd, nvmeNSRescanCmd
	nvmeListSubsysCmd = func(context.Context) ([]byte, error) { return []byte(listSubsys), listErr }
	nvmeNSRescanCmd = func(context.Context, string) { rescans++ }
	t.Cleanup(func() { nvmeListSubsysCmd, nvmeNSRescanCmd = origList, origRescan })
	return &rescans
}

// fakeSysClassNVMe points the controller scan at a fake /sys/class/nvme holding one
// controller per name -> NQN.
func fakeSysClassNVMe(t *testing.T, ctrls map[string]string) {
	t.Helper()
	root := t.TempDir()
	for name, nqn := range ctrls {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name, "subsysnqn"), []byte(nqn+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	orig := sysClassNVMePath
	sysClassNVMePath = root
	t.Cleanup(func() { sysClassNVMePath = orig })
}

// When list-subsys already names the controller but the namespace is not in sysfs yet,
// the lookup must report that controller and leave rescans to waitForNVMeDevice's
// cadence (first sighting, then every 5th poll). It used to fall through to the sysfs
// scan, which ran ns-rescan on every 2s poll and lost the controller name.
func TestNVMeLookupKeepsControllerWithoutRescanning(t *testing.T) {
	useFakeSysBlock(t, map[string]string{}, map[string]string{})
	fakeSysClassNVMe(t, map[string]string{"nvme4": nqnA})
	rescans := fakeNVMeTools(t, `{"Subsystems": [{"NQN": "`+nqnA+`", "Paths": [{"Name": "nvme4", "State": "live"}]}]}`, nil)

	s := &NodeService{}
	dev, ctrl, err := s.findNVMeDeviceByNQNWithController(context.Background(), nqnA)
	if dev != "" || ctrl != "nvme4" || !errors.Is(err, ErrNVMeDeviceNotFound) {
		t.Fatalf("got %q, %q, %v; want \"\", nvme4, ErrNVMeDeviceNotFound", dev, ctrl, err)
	}
	if *rescans != 0 {
		t.Fatalf("lookup ran %d ns-rescans; rescans belong to the wait loop's cadence", *rescans)
	}
}

// A connected controller whose namespace never appears is a stale connection. It must
// be reported as unhealthy so tryReuseExistingConnection disconnects it at once; "not
// found" made staging reconnect and wait out the full 60s device timeout first.
func TestStaleControllerWithoutNamespaceIsUnhealthy(t *testing.T) {
	useFakeSysBlock(t, map[string]string{}, map[string]string{})
	fakeSysClassNVMe(t, map[string]string{"nvme4": nqnA})
	fakeNVMeTools(t, "", errors.New("nvme: command not found"))

	s := &NodeService{}
	if _, err := s.findNVMeDeviceByNQNFromSys(context.Background(), nqnA); !errors.Is(err, ErrNVMeDeviceUnhealthy) {
		t.Fatalf("findNVMeDeviceByNQNFromSys() error = %v, want ErrNVMeDeviceUnhealthy", err)
	}
}
