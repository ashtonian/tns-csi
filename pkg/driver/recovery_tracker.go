package driver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// defaultMountTrackerStatePath is where the tracker persists its state so it
// survives a plugin pod restart while the host mounts persist. /var/lib/tns-csi
// is a fixed, host-backed hostPath in the node DaemonSet (the "registry-dir"
// volume), independent of the configurable kubelet path.
const defaultMountTrackerStatePath = "/var/lib/tns-csi/recovery-mounts.json"

// stagedVolume is the authoritative record of one block volume this node has
// staged. The reconciler correlates kernel/device events to volumes through
// these records rather than inferring the device->PV->PVC chain from /proc.
type stagedVolume struct {
	StagedAt    time.Time `json:"stagedAt"`
	VolumeID    string    `json:"volumeId"`
	StagingPath string    `json:"stagingPath"`
	DevicePath  string    `json:"devicePath"`
	FSType      string    `json:"fsType"`
	Protocol    string    `json:"protocol"`
	AutoRepair  string    `json:"autoRepair,omitempty"`
}

// mountTracker records, per staging path, what this node has staged. It is safe
// for concurrent use and best-effort persists to a node-local JSON file.
type mountTracker struct {
	byStaging map[string]stagedVolume
	statePath string
	mu        sync.RWMutex
	// saveMu serializes persistence so concurrent stage/unstage operations
	// cannot interleave writes to the state file. Distinct from mu (which guards
	// the map) so a save never holds the map lock during file I/O.
	saveMu sync.Mutex
}

// newMountTracker creates a tracker and loads any persisted state.
func newMountTracker(statePath string) *mountTracker {
	t := &mountTracker{
		byStaging: make(map[string]stagedVolume),
		statePath: statePath,
	}
	t.load()
	return t
}

// Add records (or replaces) a staged volume and persists the state.
func (t *mountTracker) Add(v stagedVolume) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if v.StagedAt.IsZero() {
		v.StagedAt = time.Now()
	}
	t.byStaging[v.StagingPath] = v
	t.mu.Unlock()
	t.save()
}

// Remove drops the record for a staging path and persists the state.
func (t *mountTracker) Remove(stagingPath string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	_, existed := t.byStaging[stagingPath]
	delete(t.byStaging, stagingPath)
	t.mu.Unlock()
	if existed {
		t.save()
	}
}

// ByDevice returns the staged volume backed by the given device, matching on the
// bare device name (e.g. "nvme0n1") so callers can pass either "/dev/nvme0n1" or
// the kernel name from a log line.
func (t *mountTracker) ByDevice(device string) (stagedVolume, bool) {
	if t == nil {
		return stagedVolume{}, false
	}
	want := baseDeviceName(device)
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, v := range t.byStaging {
		if baseDeviceName(v.DevicePath) == want {
			return v, true
		}
	}
	return stagedVolume{}, false
}

// List returns a snapshot of all tracked volumes.
func (t *mountTracker) List() []stagedVolume {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]stagedVolume, 0, len(t.byStaging))
	for _, v := range t.byStaging {
		out = append(out, v)
	}
	return out
}

// baseDeviceName strips a leading /dev/ (and any directory) so device names from
// different sources compare equal.
func baseDeviceName(dev string) string {
	dev = strings.TrimSpace(dev)
	if dev == "" {
		return ""
	}
	return filepath.Base(dev)
}

// load reads persisted state. Missing or unreadable state is not an error: the
// in-memory map is the source of truth, and the next stage of any volume
// re-persists it, so a lost file is self-healing.
func (t *mountTracker) load() {
	data, err := os.ReadFile(t.statePath)
	if err != nil {
		if !os.IsNotExist(err) {
			klog.Warningf("recovery: could not read mount tracker state %s: %v", t.statePath, err)
		}
		return
	}
	var vols []stagedVolume
	unmarshalErr := json.Unmarshal(data, &vols)
	if unmarshalErr != nil {
		klog.Warningf("recovery: could not parse mount tracker state %s: %v", t.statePath, unmarshalErr)
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, v := range vols {
		t.byStaging[v.StagingPath] = v
	}
}

// save persists state atomically (unique temp file + rename). It is serialized
// by saveMu so concurrent stage/unstage operations cannot interleave writes, and
// the snapshot is taken inside the critical section so the last save to complete
// always persists the freshest map. Best-effort: failures are logged, not fatal.
func (t *mountTracker) save() {
	t.saveMu.Lock()
	defer t.saveMu.Unlock()

	t.mu.RLock()
	vols := make([]stagedVolume, 0, len(t.byStaging))
	for _, v := range t.byStaging {
		vols = append(vols, v)
	}
	t.mu.RUnlock()

	data, err := json.Marshal(vols)
	if err != nil {
		klog.Warningf("recovery: could not marshal mount tracker state: %v", err)
		return
	}
	dir := filepath.Dir(t.statePath)
	mkErr := os.MkdirAll(dir, 0o750)
	if mkErr != nil {
		klog.Warningf("recovery: could not create mount tracker state dir: %v", mkErr)
		return
	}
	tmp, err := os.CreateTemp(dir, "recovery-mounts-*.tmp")
	if err != nil {
		klog.Warningf("recovery: could not create mount tracker temp file: %v", err)
		return
	}
	tmpName := tmp.Name()

	_, writeErr := tmp.Write(data)
	if writeErr != nil {
		_ = tmp.Close()        //nolint:errcheck // best-effort cleanup on the error path
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup on the error path
		klog.Warningf("recovery: could not write mount tracker state: %v", writeErr)
		return
	}
	closeErr := tmp.Close()
	if closeErr != nil {
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup on the error path
		klog.Warningf("recovery: could not close mount tracker temp file: %v", closeErr)
		return
	}
	renErr := os.Rename(tmpName, t.statePath)
	if renErr != nil {
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup on the error path
		klog.Warningf("recovery: could not persist mount tracker state: %v", renErr)
	}
}
