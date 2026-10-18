//go:build linux

package volumeprobe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const validXFSRoot = "bs_ino = 128\nbs_version = 5\nbs_sick = 0x0\nbs_checked = 0x7\nbs_mode = 040755\n"

func TestParseXFSRootRequiresPositiveV5Evidence(t *testing.T) {
	for _, data := range []string{"", strings.ReplaceAll(validXFSRoot, "bs_version = 5", "bs_version = 1"), strings.ReplaceAll(validXFSRoot, "128", "0"), strings.ReplaceAll(validXFSRoot, "040755", "0100644"), strings.ReplaceAll(validXFSRoot, "0x0", "0x10000"), validXFSRoot + "bs_ino = 129\n", strings.ReplaceAll(validXFSRoot, "0x0", "invalid"), "usage: bulkstat_single"} {
		if result := parseXFSRoot([]byte(data)); result.Code != Unknown {
			t.Fatalf("accepted %q: %+v", data, result)
		}
	}
	if result := parseXFSRoot([]byte(validXFSRoot)); result.Code != OK || result.RootChecked != 7 {
		t.Fatal(result)
	}
	if result := parseXFSRoot([]byte(strings.Replace(validXFSRoot, "0x0", "0x8", 1))); result.Code != OK || result.RootSick != 8 {
		t.Fatal(result)
	}
}

func TestXFSCommandChecksDiagnosticsEvenWithZeroExit(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("PATH", directory)
	file, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	leaseA, err := os.CreateTemp(t.TempDir(), "a")
	if err != nil {
		t.Fatal(err)
	}
	defer leaseA.Close()
	leaseB, err := os.CreateTemp(t.TempDir(), "b")
	if err != nil {
		t.Fatal(err)
	}
	defer leaseB.Close()
	for _, test := range []struct{ name, body, code string }{
		{"valid", "printf '" + validXFSRoot + "'", OK},
		{"zero-exit-EIO", "printf 'xfrog_bulkstat_single: Input/output error\\n' >&2", IOFailure},
		{"unsupported", "printf 'Operation not supported\\n' >&2", Unsupported},
		{"bad-version", "printf 'version must be 1 or 5\\n' >&2", Unknown},
		{"empty", "exit 0", Unknown},
		{"malformed", "printf 'bs_ino = 128\\n'", Unknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			script := "#!/bin/sh\n[ \"$1\" = '-r' ] && [ \"$2\" = '-c' ] && [ \"$3\" = 'bulkstat_single -v 5 root' ] && [ \"$4\" = '/proc/self/fd/5' ] || exit 9\n[ -f /proc/self/fd/3 ] && [ -f /proc/self/fd/4 ] && [ -d /proc/self/fd/5 ] || exit 10\n" + test.body + "\n"
			if err := os.WriteFile(filepath.Join(directory, "xfs_io"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			if result := probeXFS(context.Background(), file, []*os.File{leaseA, leaseB}); result.Code != test.code {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func requestForDirectory(t *testing.T, path string) Request {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var st unix.Stat_t
	if statErr := unix.Fstat(int(file.Fd()), &st); statErr != nil {
		t.Fatal(statErr)
	}
	id, err := descriptorMountID(file)
	if err != nil {
		t.Fatal(err)
	}
	return Request{Path: path, MountID: id, Device: fmt.Sprintf("%d:%d", unix.Major(st.Dev), unix.Minor(st.Dev)), Filesystem: "test-filesystem"}
}

func TestLinuxHelperObservesReadOnlyAndChecksMountIdentity(t *testing.T) {
	path := t.TempDir()
	dataPath := filepath.Join(path, "sentinel")
	if err := os.WriteFile(dataPath, []byte("preserve application data"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := newTestRunner(t, t.TempDir())
	request := requestForDirectory(t, path)
	result, err := runner.Probe(context.Background(), "v", request)
	if err != nil || result.Code != OK || result.TotalBytes == 0 || result.FreeBytes > result.TotalBytes {
		t.Fatalf("observation: %+v %v", result, err)
	}
	data, err := os.ReadFile(dataPath)
	if err != nil || string(data) != "preserve application data" {
		t.Fatal("application data changed")
	}
	request.MountID++
	if result, err := runner.Probe(context.Background(), "v", request); err != nil || result.Code != IdentityChanged {
		t.Fatalf("changed mount: %+v %v", result, err)
	}
	request = requestForDirectory(t, path)
	request.Filesystem = "xfs"
	if result, err := runner.Probe(context.Background(), "v", request); err != nil || result.Code != IdentityChanged {
		t.Fatalf("wrong filesystem: %+v %v", result, err)
	}
	request.Path = filepath.Join(path, "missing")
	if result, err := runner.Probe(context.Background(), "v", request); err != nil || result.Code != Missing {
		t.Fatalf("missing path: %+v %v", result, err)
	}
}

func TestMetadataReadIsBoundedAndStrict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata")
	if value, err := readMetadata(path); err != nil || value != "" {
		t.Fatal(value, err)
	}
	for _, data := range []string{"", "\n", "a\nb", strings.Repeat("x", 4097), "a\x00b"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readMetadata(path); !errors.Is(err, ErrProtocol) {
			t.Fatalf("accepted invalid metadata %q: %v", data, err)
		}
	}
	if err := os.WriteFile(path, []byte("nqn.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if value, err := readMetadata(path); err != nil || value != "nqn.test" {
		t.Fatal(value, err)
	}
}

func TestLinuxErrorClassification(t *testing.T) {
	for _, test := range []struct {
		err  error
		code string
	}{{unix.EIO, IOFailure}, {unix.EUCLEAN, IOFailure}, {os.ErrNotExist, Missing}, {unix.ENOTTY, Unsupported}, {unix.EOPNOTSUPP, Unsupported}, {unix.EACCES, Unknown}} {
		if got := classifyError(test.err); got != test.code {
			t.Fatalf("%v: %s", test.err, got)
		}
	}
}

func TestLinuxMountResolutionIsBoundedAndExact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink("/", path); err != nil {
		t.Fatal(err)
	}
	runner := newTestRunner(t, t.TempDir())
	result, err := runner.Probe(context.Background(), "v", Request{Path: path, ResolveMount: true})
	if err != nil || result.Code != OK || result.Mount == nil || result.Mount.Path != "/" || result.Mount.MountID <= 0 {
		t.Fatalf("resolution: %+v %v", result, err)
	}
	result, err = runner.Probe(context.Background(), "v", Request{Path: t.TempDir(), ResolveMount: true})
	if err != nil || result.Code != Missing {
		t.Fatalf("ordinary directory became a mount: %+v %v", result, err)
	}
	if mountPath, err := containingMount("/unmounted-child"); err != nil || mountPath != "/" {
		t.Fatal(mountPath, err)
	}
}

func TestLinuxMountResolutionCapturesRawIdentityBeforeObservation(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "raw-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", file.Name()} {
		result := resolveMountWithStat(context.Background(), path, func(actual string, stat *unix.Stat_t) error {
			if actual != path {
				t.Errorf("statted %q, want %q", actual, path)
			}
			*stat = unix.Stat_t{Mode: unix.S_IFBLK, Rdev: unix.Mkdev(259, 0)}
			return nil
		})
		if result.Code != OK || result.RawDevice != "259:0" || result.Mount == nil || !result.Mount.Raw || result.Mount.MountID <= 0 {
			t.Fatalf("raw resolution %q: %+v", path, result)
		}
	}
	for _, test := range []struct {
		err  error
		code string
	}{{unix.EIO, IOFailure}, {os.ErrNotExist, Missing}, {unix.EACCES, Unknown}} {
		result := resolveMountWithStat(context.Background(), "/", func(string, *unix.Stat_t) error { return test.err })
		if result.Code != test.code || result.RawDevice != "" || result.Mount != nil {
			t.Fatalf("failed raw stat: %+v", result)
		}
	}
	result := resolveMountWithStat(context.Background(), file.Name(), func(_ string, stat *unix.Stat_t) error {
		*stat = unix.Stat_t{Mode: unix.S_IFCHR, Rdev: unix.Mkdev(1, 3)}
		return nil
	})
	if result.Code != Missing || result.RawDevice != "" {
		t.Fatalf("character device became a raw volume: %+v", result)
	}
}

func TestRawCapacityAndSCSIStateRequireValidEvidence(t *testing.T) {
	for _, data := range []string{"", "0", "-1", "not-a-number", "18446744073709551615", strings.Repeat("1", 65)} {
		if result := parseRawCapacity([]byte(data), "8:0"); result.Code != Unknown {
			t.Fatalf("accepted %q: %+v", data, result)
		}
	}
	if result := parseRawCapacity([]byte("8\n"), "8:0"); result.Code != OK || result.TotalBytes != 4096 || result.RawDevice != "8:0" {
		t.Fatal(result)
	}
	for state, want := range map[string]string{"running": OK, "": Unknown, "blocked": TransportUnavailable, "offline": TransportUnavailable} {
		if got := classifySCSIState(state); got != want {
			t.Fatalf("state %q: %s", state, got)
		}
	}
}

func TestRawCapacityAndSCSIStateUseSyntheticSysfs(t *testing.T) {
	root := t.TempDir()
	original := sysDevBlockPath
	sysDevBlockPath = filepath.Join(root, "dev-block")
	t.Cleanup(func() { sysDevBlockPath = original })
	if err := os.MkdirAll(sysDevBlockPath, 0o700); err != nil {
		t.Fatal(err)
	}
	device := filepath.Join(root, "devices", "sda")
	if err := os.MkdirAll(filepath.Join(device, "device"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(device, filepath.Join(sysDevBlockPath, "8:0")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(device, "size"), []byte("8\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for state, want := range map[string]string{"running": OK, "": Unknown, "blocked": TransportUnavailable} {
		if err := os.WriteFile(filepath.Join(device, "device", "state"), []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}
		result := rawCapacity(unix.Mkdev(8, 0))
		if result.Code != want {
			t.Fatalf("state %q: %+v", state, result)
		}
		if want == OK && (result.TotalBytes != 4096 || result.RawDevice != "8:0") {
			t.Fatal(result)
		}
	}
	if err := os.Remove(filepath.Join(device, "device", "state")); err != nil {
		t.Fatal(err)
	}
	if result := rawCapacity(unix.Mkdev(8, 0)); result.Code != Unknown {
		t.Fatal(result)
	}
}
