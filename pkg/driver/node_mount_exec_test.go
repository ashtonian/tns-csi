//go:build linux

package driver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fenio/tns-csi/pkg/mount"
)

// fakeMountBinary installs a "mount" script for one test and shortens the wait delay.
func fakeMountBinary(t *testing.T, script string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mount")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	origBin, origDelay := mountBinary, mountWaitDelay
	mountBinary, mountWaitDelay = bin, 200*time.Millisecond
	t.Cleanup(func() { mountBinary, mountWaitDelay = origBin, origDelay })
}

func fakeMountTable(t *testing.T, mounted bool) {
	t.Helper()
	orig := lookupMountedEntry
	lookupMountedEntry = func(context.Context, string) (mount.Entry, bool, error) {
		return mount.Entry{Source: "server:/export", FSType: "nfs"}, mounted, nil
	}
	t.Cleanup(func() { lookupMountedEntry = orig })
}

// mount.nfs can start rpc.statd (and iscsiadm can start iscsid), which inherit mount's
// output pipe. mount itself exits 0, but the pipe stays open, and after WaitDelay the
// command reports exec.ErrWaitDelay. That used to fail a mount that had succeeded; the
// mount table decides instead.
func TestRunMountTreatsWaitDelayWithMountedTargetAsSuccess(t *testing.T) {
	fakeMountBinary(t, "sleep 5 &\nexit 0")

	t.Run("target mounted", func(t *testing.T) {
		fakeMountTable(t, true)
		if _, err := runMount(context.Background(), 10*time.Second, "server:/export", "/staging"); err != nil {
			t.Fatalf("runMount() = %v, want nil: the target is mounted", err)
		}
	})

	t.Run("target not mounted", func(t *testing.T) {
		fakeMountTable(t, false)
		if _, err := runMount(context.Background(), 10*time.Second, "server:/export", "/staging"); err == nil {
			t.Fatal("runMount() = nil although the target is not mounted")
		}
	})
}

func TestRunMountReportsFailures(t *testing.T) {
	fakeMountBinary(t, "echo 'mount: permission denied'\nexit 32")
	fakeMountTable(t, false)
	out, err := runMount(context.Background(), 10*time.Second, "server:/export", "/staging")
	if err == nil || string(out) != "mount: permission denied\n" {
		t.Fatalf("runMount() = %q, %v; want the failure and its output", out, err)
	}
}
