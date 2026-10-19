package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/fenio/tns-csi/pkg/mount"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNVMeReuseRetainsFailedCapacity(t *testing.T) {
	for _, failure := range []string{"zero", "probe-error", "changed-identity"} {
		t.Run(failure, func(t *testing.T) {
			useFakeSysBlock(t, map[string]string{"nvme4": nqnA}, map[string]string{"nvme4n1": "nvme4"})
			fakeNVMeTools(t, `{"Subsystems":[{"NQN":"`+nqnA+`","Paths":[{"Name":"nvme4","State":"live"}]}]}`, nil)
			s := NewNodeService("test", nil, true, nil, false, 1)
			s.nvmeOps.rescan = func(context.Context, string) error {
				if failure == "changed-identity" {
					return os.WriteFile(filepath.Join(sysBlockPath, "nvme4n1", "device", "subsysnqn"), []byte(nqnB), 0o600)
				}
				return nil
			}
			s.nvmeOps.readSize = func(context.Context, string) (int64, error) {
				if failure == "probe-error" {
					return 0, errors.New("capacity I/O failed")
				}
				return 0, nil
			}
			s.disconnectNVMeOFFn = func(context.Context, string) error { t.Fatal("recovery disconnected target"); return nil }
			s.nvmeOps.stage = func(context.Context, string, string, string, *csi.VolumeCapability, bool, map[string]string) (*csi.NodeStageVolumeResponse, error) {
				t.Fatal("unready device reached staging")
				return &csi.NodeStageVolumeResponse{}, nil
			}
			_, _, err := s.tryReuseExistingConnection(context.Background(), &nvmeOFConnectionParams{nqn: nqnA}, "volume", "unused", nil, false, nil)
			want := codes.Unavailable
			if failure == "changed-identity" {
				want = codes.FailedPrecondition
			}
			if status.Code(err) != want {
				t.Fatalf("reuse error = %v; want %v", err, want)
			}
		})
	}
}

func TestNVMeUnstageRefusesUnsafeScope(t *testing.T) {
	for _, failure := range []string{"wrong-source", "published-user", "unknown-users", "extra-namespace"} {
		t.Run(failure, func(t *testing.T) {
			devs := map[string]string{"nvme4n1": "nvme4", "nvme5n1": "nvme5"}
			nsids := map[string]string{}
			if failure == "extra-namespace" {
				devs["nvme4n2"] = "nvme4"
				nsids["nvme4n2"] = "2"
			}
			origBlock := sysBlockPath
			sysBlockPath = fakeSysBlock(t, map[string]string{"nvme4": nqnA, "nvme5": nqnB}, devs, nsids)
			t.Cleanup(func() { sysBlockPath = origBlock })
			fakeSysClassNVMe(t, map[string]string{"nvme4": nqnA, "nvme5": nqnB})
			s := NewNodeService("test", nil, true, nil, false, 1)
			s.isSourceMountedFn = func(context.Context, string) (bool, error) {
				if failure == "unknown-users" {
					return false, errors.New("host mount evidence unavailable")
				}
				return failure == "published-user", nil
			}
			s.nvmeOps.unmount = func(context.Context, string) error { t.Fatal("wrong source was unmounted"); return nil }
			s.disconnectNVMeOFFn = func(context.Context, string) error { t.Fatal("unsafe scope was disconnected"); return nil }
			origLookup := lookupMountedEntry
			lookupMountedEntry = func(context.Context, string) (mount.Entry, bool, error) {
				return mount.Entry{Source: "/dev/nvme5n1", FSType: "xfs"}, failure == "wrong-source", nil
			}
			t.Cleanup(func() { lookupMountedEntry = origLookup })
			stage := filepath.Join(t.TempDir(), "stage")
			if err := os.WriteFile(stage+nvmePolicyMarkerSuffix, []byte(nvmeRecordName("volume")), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := s.unstageNVMeOFVolume(context.Background(), &csi.NodeUnstageVolumeRequest{VolumeId: "volume", StagingTargetPath: stage}, map[string]string{"nqn": nqnA})
			want := codes.FailedPrecondition
			if failure == "unknown-users" {
				want = codes.Unavailable
			}
			if status.Code(err) != want {
				t.Fatalf("unstage = %v; want %v", err, want)
			}
			if got, readErr := readNVMeStagingNQN(stage); readErr != nil || got != nqnA {
				t.Fatalf("retained NQN = %q, %v", got, readErr)
			}
			if _, statErr := os.Stat(stage + nvmePolicyMarkerSuffix); statErr != nil {
				t.Fatalf("refused unstage removed policy marker: %v", statErr)
			}
		})
	}
}

// kubelet removes the staging path's parent directory after unstage and fails while it is not empty.
func TestNVMeUnstageRemovesStagingMetadata(t *testing.T) {
	useFakeSysBlock(t, map[string]string{"nvme4": nqnA}, map[string]string{"nvme4n1": "nvme4"})
	fakeSysClassNVMe(t, map[string]string{"nvme4": nqnA})
	origLookup := lookupMountedEntry
	lookupMountedEntry = func(context.Context, string) (mount.Entry, bool, error) { return mount.Entry{}, false, nil }
	t.Cleanup(func() { lookupMountedEntry = origLookup })
	s := NewNodeService("test", nil, true, nil, false, 1)
	s.nvmePolicyDirectory = filepath.Join(t.TempDir(), "records")
	s.isSourceMountedFn = func(context.Context, string) (bool, error) { return false, nil }
	s.disconnectNVMeOFFn = func(context.Context, string) error { return nil }
	volumeDir := t.TempDir()
	stage := filepath.Join(volumeDir, "globalmount")
	if err := os.Mkdir(stage, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := writeNVMeStagingNQN(stage, nqnA); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.nvmePolicyDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(s.nvmePolicyDirectory, nvmeRecordName("volume"))
	for _, path := range []string{record, stage + nvmePolicyMarkerSuffix} {
		if err := os.WriteFile(path, []byte(nvmeRecordName("volume")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.unstageNVMeOFVolume(context.Background(), &csi.NodeUnstageVolumeRequest{VolumeId: "volume", StagingTargetPath: stage}, map[string]string{"nqn": nqnA}); err != nil {
		t.Fatalf("unstage: %v", err)
	}
	if err := os.Remove(stage); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(volumeDir); err != nil {
		t.Fatalf("staging parent is not removable after unstage: %v", err)
	}
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("durable policy record was not retained: %v", err)
	}
}

func TestNVMeUnstageExcludesIntendedStagingMount(t *testing.T) {
	useFakeSysBlock(t, map[string]string{"nvme4": nqnA}, map[string]string{"nvme4n1": "nvme4"})
	fakeSysClassNVMe(t, map[string]string{"nvme4": nqnA})
	mounted := true
	origLookup := lookupMountedEntry
	lookupMountedEntry = func(context.Context, string) (mount.Entry, bool, error) {
		return mount.Entry{Source: "/dev/nvme4n1", FSType: "xfs"}, mounted, nil
	}
	t.Cleanup(func() { lookupMountedEntry = origLookup })
	s := NewNodeService("test", nil, true, nil, false, 1)
	s.nvmeOps.unmount = func(context.Context, string) error { mounted = false; return nil }
	s.isSourceMountedFn = func(context.Context, string) (bool, error) { return mounted, nil }
	disconnects := 0
	s.disconnectNVMeOFFn = func(context.Context, string) error { disconnects++; return nil }
	stage := filepath.Join(t.TempDir(), "stage")
	resp, err := s.unstageNVMeOFVolume(context.Background(), &csi.NodeUnstageVolumeRequest{VolumeId: "volume", StagingTargetPath: stage}, map[string]string{"nqn": nqnA})
	if err != nil || resp == nil || mounted || disconnects != 1 {
		t.Fatalf("unstage=%v, %v; mounted=%v, disconnects=%d", resp, err, mounted, disconnects)
	}
}

func TestNVMeConnectStageFailureNeverResets(t *testing.T) {
	for _, phase := range []string{"connect", "live", "device", "stage"} {
		t.Run(phase, func(t *testing.T) {
			s := NewNodeService("test", nil, true, nil, false, 1)
			var calls []string
			step := func(name string) error {
				calls = append(calls, name)
				if name == phase {
					return status.Error(codes.Unavailable, "injected readiness failure")
				}
				return nil
			}
			s.nvmeOps.connect = func(context.Context, *nvmeOFConnectionParams) error { return step("connect") }
			s.nvmeOps.waitLive = func(context.Context, string, time.Duration) error { return step("live") }
			s.nvmeOps.waitDevice = func(context.Context, string, time.Duration) (string, error) { return "/dev/nvme4n1", step("device") }
			s.nvmeOps.stage = func(context.Context, string, string, string, *csi.VolumeCapability, bool, map[string]string) (*csi.NodeStageVolumeResponse, error) {
				return nil, step("stage")
			}
			s.disconnectNVMeOFFn = func(context.Context, string) error { t.Fatal("recovery disconnected target"); return nil }
			_, err := s.connectAndStageDevice(context.Background(), &nvmeOFConnectionParams{nqn: nqnA}, "volume", "unused", nil, false, nil, "")
			if status.Code(err) != codes.Unavailable {
				t.Fatalf("stage = %v; want Unavailable", err)
			}
			all := []string{"connect", "live", "device", "stage"}
			for i, name := range all {
				if name == phase && !reflect.DeepEqual(calls, all[:i+1]) {
					t.Fatalf("operations=%v; want %v", calls, all[:i+1])
				}
			}
		})
	}
}

func TestNVMeConnectStageCancellationStopsBeforeReadiness(t *testing.T) {
	s := NewNodeService("test", nil, true, nil, false, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.nvmeOps.connect = func(context.Context, *nvmeOFConnectionParams) error { cancel(); return nil }
	s.nvmeOps.waitLive = func(context.Context, string, time.Duration) error {
		t.Fatal("canceled call continued readiness work")
		return nil
	}
	_, err := s.connectAndStageDevice(ctx, &nvmeOFConnectionParams{nqn: nqnA}, "volume", "unused", nil, false, nil, "")
	if status.Code(err) != codes.Canceled {
		t.Fatalf("stage = %v; want Canceled", err)
	}
}

func TestNVMeSemaphoreCancellationKeepsContextCode(t *testing.T) {
	useFakeSysBlock(t, map[string]string{}, map[string]string{})
	fakeSysClassNVMe(t, map[string]string{})
	fakeNVMeTools(t, `{"Subsystems":[]}`, nil)
	s := NewNodeService("test", nil, true, nil, false, 1)
	s.nvmeConnectSem <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.nvmeOps.checkCLI = func(context.Context) error { cancel(); return nil }
	_, err := s.stageNVMeOFVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId: "volume", StagingTargetPath: filepath.Join(t.TempDir(), "stage"),
		VolumeCapability: &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}},
	}, map[string]string{"nqn": nqnA, "server": "192.0.2.10"})
	if status.Code(err) != codes.Canceled || len(s.nvmeConnectSem) != 1 {
		t.Fatalf("stage=%v, held permits=%d; want Canceled and original permit", err, len(s.nvmeConnectSem))
	}
}
