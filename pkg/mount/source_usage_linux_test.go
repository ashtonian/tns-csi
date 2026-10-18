//go:build linux

package mount

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRawDeviceBindMountUsage(t *testing.T) {
	info := "42 30 0:7 /nvme4n1 /pods/raw rw - devtmpfs udev rw\n"
	raw := rawMountSource{root: "/nvme4n1", major: 0, minor: 7}
	for _, tc := range []struct {
		root string
		want bool
	}{{"/nvme4n1", true}, {"/nvme4n10", false}, {"/nvme5n1", false}} {
		raw.root = tc.root
		got, err := isDeviceInMountInfo(259, 4, strings.NewReader(info), raw)
		if err != nil || got != tc.want {
			t.Fatalf("raw root %q: usage=%v, %v; want %v", tc.root, got, err, tc.want)
		}
	}
}

func TestRawSourceRootUsesSourceMountMapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mountinfo")
	info := "30 20 0:7 / /dev rw - devtmpfs udev rw\n31 20 0:7 /subdir /alias rw - devtmpfs udev rw\n"
	if err := os.WriteFile(path, []byte(info), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := rawSourceForMountInfo("/alias/nvme4n1", unix.Mkdev(0, 7), path)
	if err != nil || got.root != "/subdir/nvme4n1" || got.minor != 7 {
		t.Fatalf("source root=%+v, %v", got, err)
	}
	if _, err := rawSourceForMountInfo("/other/nvme4n1", unix.Mkdev(0, 7), path); err == nil {
		t.Fatal("unidentified source mount certified unused")
	}
}
