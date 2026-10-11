//go:build linux

package mount

import (
	"os"
	"path/filepath"
	"testing"
)

// After "mount --move", or with mounts propagated in from another namespace, mountinfo
// lines are not in stacking order: here the upper mount (41, parent 40) is listed before
// the mount it sits on (40). The topmost mount is the one no other mount on the same
// point uses as its parent, not the last line.
func TestLookupMountTopmostFollowsParentChainNotLineOrder(t *testing.T) {
	info := writeMountInfo(t, `22 1 0:21 / / rw,relatime - overlay none rw
41 40 259:3 / /mnt/stack rw,relatime - xfs /dev/top rw
40 22 259:2 / /mnt/stack rw,relatime - ext4 /dev/bottom rw
`)
	e, ok, err := lookupMount(info, "/mnt/stack")
	if err != nil || !ok || e.Source != "/dev/top" || e.FSType != "xfs" {
		t.Fatalf("lookupMount() = %+v, %v, %v; want the top mount /dev/top (xfs)", e, ok, err)
	}
}

// The kernel escapes a backslash in a path as \134; it must decode to one backslash.
func TestLookupMountDecodesEscapedBackslash(t *testing.T) {
	info := writeMountInfo(t, `22 1 0:21 / / rw,relatime - overlay none rw
39 22 0:46 / /mnt/a\134b rw - cifs //nas/x\134y rw
`)
	e, ok, err := lookupMount(info, `/mnt/a\b`)
	if err != nil || !ok || e.Source != `//nas/x\y` {
		t.Fatalf("lookupMount() = %+v, %v, %v; want source //nas/x\\y", e, ok, err)
	}
	if got := unescapeMountField(`a\134\040b`); got != `a\ b` {
		t.Fatalf("unescapeMountField = %q, want %q", got, `a\ b`)
	}
}

// A path whose parent is a regular file cannot be a mount point. Resolving it returns
// ENOTDIR, which must mean "not mounted", not an error that fails unstage forever.
func TestCanonicalMountTargetWithFileParentIsNotAnError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(file, "sub", "globalmount")
	got, err := canonicalMountTarget(target)
	if err != nil || got != target {
		t.Fatalf("canonicalMountTarget(%s) = %q, %v; want the cleaned path and no error", target, got, err)
	}
}
