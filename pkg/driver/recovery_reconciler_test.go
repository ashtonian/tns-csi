package driver

import (
	"strings"
	"testing"
	"time"
)

func TestPodUIDFromMountPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/var/lib/kubelet/pods/abc-123/volumes/kubernetes.io~csi/pvc-xyz/mount", "abc-123"},
		{"/var/lib/kubelet/pods/4d5e/volumes/kubernetes.io~csi/pvc-1/mount", "4d5e"},
		{"/var/lib/kubelet/plugins/kubernetes.io/csi/tns.csi.io/hash/globalmount", ""},
		{"/no/pods/here", ""},
	}
	for _, tt := range tests {
		if got := podUIDFromMountPath(tt.path); got != tt.want {
			t.Errorf("podUIDFromMountPath(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestIsOurGlobalmount(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/var/lib/kubelet/plugins/kubernetes.io/csi/tns.csi.io/abc/globalmount", true},
		{"/var/lib/kubelet/plugins/kubernetes.io/csi/other.csi.io/abc/globalmount", false},
		{"/var/lib/kubelet/pods/uid/volumes/kubernetes.io~csi/tns.csi.io-pv/mount", false},
		{"/var/lib/kubelet/plugins/kubernetes.io/csi/tns.csi.io/abc/globalmount/sub", false},
	}
	for _, tt := range tests {
		if got := isOurGlobalmount(tt.path); got != tt.want {
			t.Errorf("isOurGlobalmount(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestIsPodBindMount(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/var/lib/kubelet/pods/uid/volumes/kubernetes.io~csi/pvc-1/mount", true},
		{"/var/lib/kubelet/plugins/kubernetes.io/csi/tns.csi.io/abc/globalmount", false},
		{"/var/lib/kubelet/pods/uid/volumes/kubernetes.io~secret/foo", false},
	}
	for _, tt := range tests {
		if got := isPodBindMount(tt.path); got != tt.want {
			t.Errorf("isPodBindMount(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestIsBlockDevice(t *testing.T) {
	tests := []struct {
		dev  string
		want bool
	}{
		{"/dev/nvme0n1", true},
		{"/dev/sda1", true},
		{"/dev/dm-3", true},
		{"tmpfs", false},
		{"/dev/loop0", false},
	}
	for _, tt := range tests {
		if got := isBlockDevice(tt.dev); got != tt.want {
			t.Errorf("isBlockDevice(%q) = %v, want %v", tt.dev, got, tt.want)
		}
	}
}

func TestParseProcMounts(t *testing.T) {
	in := strings.NewReader(strings.Join([]string{
		"/dev/nvme0n1 /var/lib/kubelet/plugins/kubernetes.io/csi/tns.csi.io/h/globalmount xfs rw 0 0",
		"proc /proc proc rw,nosuid 0 0",
		"malformed line",
		"/dev/sda1 /var/lib/kubelet/pods/u/volumes/kubernetes.io~csi/pvc-1/mount ext4 rw 0 0",
	}, "\n"))
	mounts := parseProcMounts(in)
	if len(mounts) != 3 {
		t.Fatalf("parseProcMounts returned %d mounts, want 3 (malformed skipped)", len(mounts))
	}
	if mounts[0].device != "/dev/nvme0n1" || mounts[0].fstype != "xfs" {
		t.Errorf("first mount = %+v", mounts[0])
	}
	if !isOurGlobalmount(mounts[0].mountpoint) {
		t.Errorf("first mount should be a tns globalmount: %q", mounts[0].mountpoint)
	}
}

func TestReconcilerCooldown(t *testing.T) {
	r := &recoveryReconciler{
		cfg:        RecoveryConfig{Cooldown: 5 * time.Minute},
		lastAction: make(map[string]time.Time),
	}
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if !r.coolDownOK("nvme0n1", now) {
		t.Fatal("first action should be allowed")
	}
	r.recordAction("nvme0n1", now)
	if r.coolDownOK("nvme0n1", now.Add(time.Minute)) {
		t.Error("action within cooldown should be blocked")
	}
	if !r.coolDownOK("nvme0n1", now.Add(6*time.Minute)) {
		t.Error("action after cooldown should be allowed")
	}
	if !r.coolDownOK("nvme1n1", now.Add(time.Minute)) {
		t.Error("different device should be independent")
	}
}

func TestReconcilerBindDebounce(t *testing.T) {
	r := &recoveryReconciler{bindFailures: make(map[string]int)}
	if n := r.bumpBindFailure("uid-1"); n != 1 {
		t.Errorf("first bump = %d, want 1", n)
	}
	if n := r.bumpBindFailure("uid-1"); n != 2 {
		t.Errorf("second bump = %d, want 2", n)
	}
	r.clearBindFailure("uid-1")
	if n := r.bumpBindFailure("uid-1"); n != 1 {
		t.Errorf("after clear, bump = %d, want 1", n)
	}
}
