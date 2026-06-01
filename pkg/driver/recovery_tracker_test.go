package driver

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// TestMountTrackerConcurrentSave locks in the fix for the save race: concurrent
// stage operations must not corrupt or lose persisted state.
func TestMountTrackerConcurrentSave(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	tr := newMountTracker(statePath)

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			tr.Add(stagedVolume{
				VolumeID:    fmt.Sprintf("pvc-%d", i),
				StagingPath: fmt.Sprintf("/sp/%d", i),
				DevicePath:  fmt.Sprintf("/dev/nvme%dn1", i),
				FSType:      "xfs",
				Protocol:    ProtocolNVMeOF,
			})
		}(i)
	}
	wg.Wait()

	// The persisted file must be valid JSON reloading to all n entries.
	reloaded := newMountTracker(statePath)
	if got := len(reloaded.List()); got != n {
		t.Fatalf("reloaded %d entries, want %d (concurrent save lost/corrupted state)", got, n)
	}
	for i := range n {
		if _, ok := reloaded.ByDevice(fmt.Sprintf("nvme%dn1", i)); !ok {
			t.Errorf("entry nvme%dn1 missing after concurrent save", i)
		}
	}
}

func TestMountTrackerAddGetRemove(t *testing.T) {
	dir := t.TempDir()
	tr := newMountTracker(filepath.Join(dir, "state.json"))

	tr.Add(stagedVolume{
		VolumeID:    "pvc-1",
		StagingPath: "/var/lib/kubelet/.../globalmount",
		DevicePath:  "/dev/nvme0n1",
		FSType:      "xfs",
		Protocol:    ProtocolNVMeOF,
	})

	if v, ok := tr.ByDevice("nvme0n1"); !ok || v.VolumeID != "pvc-1" {
		t.Fatalf("ByDevice(nvme0n1) = %+v, %v; want pvc-1", v, ok)
	}
	// Match must work with a /dev/ prefix too.
	if _, ok := tr.ByDevice("/dev/nvme0n1"); !ok {
		t.Errorf("ByDevice(/dev/nvme0n1) not found")
	}
	if _, ok := tr.ByDevice("nvme9n9"); ok {
		t.Errorf("ByDevice(nvme9n9) unexpectedly found")
	}

	tr.Remove("/var/lib/kubelet/.../globalmount")
	if _, ok := tr.ByDevice("nvme0n1"); ok {
		t.Errorf("device still tracked after Remove")
	}
}

func TestMountTrackerPersistence(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	tr := newMountTracker(statePath)
	tr.Add(stagedVolume{VolumeID: "pvc-7", StagingPath: "/sp/7", DevicePath: "/dev/nvme7n1", FSType: "ext4", Protocol: ProtocolISCSI})

	// A fresh tracker pointed at the same path must recover the record.
	reloaded := newMountTracker(statePath)
	v, ok := reloaded.ByDevice("nvme7n1")
	if !ok {
		t.Fatalf("reloaded tracker lost the record")
	}
	if v.FSType != "ext4" || v.VolumeID != "pvc-7" {
		t.Errorf("reloaded record = %+v, want pvc-7/ext4", v)
	}
}

func TestMountTrackerNilSafe(t *testing.T) {
	var tr *mountTracker // disabled recovery leaves this nil
	tr.Add(stagedVolume{VolumeID: "x"})
	tr.Remove("/sp")
	if _, ok := tr.ByDevice("nvme0n1"); ok {
		t.Errorf("nil tracker should report not found")
	}
	if got := tr.List(); got != nil {
		t.Errorf("nil tracker List() = %v, want nil", got)
	}
}
