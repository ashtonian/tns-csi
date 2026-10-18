package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/fenio/tns-csi/pkg/mount"
	"github.com/fenio/tns-csi/pkg/volumeprobe"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func xfsHealthFixture(t *testing.T) (service *NodeService, stage string) {
	t.Helper()
	s, _, _ := nvmeLimitFixture(t, false)
	writeNVMeFixtureValue(t, filepath.Join(filepath.Dir(sysBlockPath), "owners", "nvme7", "state"), "live")
	path := filepath.Join(t.TempDir(), "stage")
	s.healthMountFn = func(context.Context, string) (mount.Entry, bool, error) {
		return mount.Entry{Source: "/dev/nvme0n1", FSType: fsTypeXFS, Device: "259:0", MountID: 42}, true, nil
	}
	s.healthProbeFn = func(context.Context, string, volumeprobe.Request) (volumeprobe.Result, error) {
		return volumeprobe.Result{Code: volumeprobe.OK, StagingNQN: nqnA, TotalBytes: 1024, FreeBytes: 512, AvailableBytes: 256, TotalInodes: 100, FreeInodes: 80}, nil
	}
	return s, path
}

func TestNVMeHealthDoesNotCertifyUnreadableController(t *testing.T) {
	s, path := xfsHealthFixture(t)
	if err := os.Remove(filepath.Join(filepath.Dir(sysBlockPath), "owners", "nvme7", "state")); err != nil {
		t.Fatal(err)
	}
	s.healthProbeFn = func(context.Context, string, volumeprobe.Request) (volumeprobe.Result, error) {
		t.Fatal("probed after unknown transport state")
		return volumeprobe.Result{}, nil
	}
	if _, err := s.NodeGetVolumeHealth(context.Background(), &csi.NodeGetVolumeHealthRequest{VolumeId: "v", StagingTargetPath: path}); status.Code(err) != codes.Unavailable {
		t.Fatalf("unreadable controller: %v", err)
	}
}

func TestXFSHealthResultMapping(t *testing.T) {
	for _, test := range []struct {
		name, code string
		sick       uint64
		want       codes.Code
		adverse    csi.VolumeHealthErrorType
	}{
		{"usable", volumeprobe.OK, 0, codes.OK, 0},
		{"shutdown", volumeprobe.IOFailure, 0, codes.OK, csi.VolumeHealthErrorType_INACCESSIBLE},
		{"sick-root", volumeprobe.OK, 8, codes.OK, csi.VolumeHealthErrorType_DEGRADED},
		{"unsupported", volumeprobe.Unsupported, 0, codes.Unavailable, 0},
		{"unknown", volumeprobe.Unknown, 0, codes.Unavailable, 0},
		{"changed", volumeprobe.IdentityChanged, 0, codes.Unavailable, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, path := xfsHealthFixture(t)
			s.healthProbeFn = func(context.Context, string, volumeprobe.Request) (volumeprobe.Result, error) {
				return volumeprobe.Result{Code: test.code, RootSick: test.sick, StagingNQN: nqnA}, nil
			}
			response, err := s.NodeGetVolumeHealth(context.Background(), &csi.NodeGetVolumeHealthRequest{VolumeId: "v", StagingTargetPath: path})
			if status.Code(err) != test.want {
				t.Fatalf("health: %+v %v", response, err)
			}
			if err != nil {
				return
			}
			statuses := response.GetVolumeHealth().GetHealthStatuses()
			if test.adverse == 0 {
				if len(statuses) != 0 {
					t.Fatal(statuses)
				}
				return
			}
			if len(statuses) != 1 || statuses[0].GetStatus() != test.adverse {
				t.Fatal(statuses)
			}
		})
	}
}

func TestHealthRejectsMountAndNamespaceChanges(t *testing.T) {
	for _, change := range []string{"mount-id", "nqn", "devnode", "uuid", "empty-state", "missing-binding", "wrong-binding", "missing-record"} {
		t.Run(change, func(t *testing.T) {
			s, path := xfsHealthFixture(t)
			original := s.healthProbeFn
			s.healthProbeFn = func(ctx context.Context, id string, request volumeprobe.Request) (volumeprobe.Result, error) {
				result, err := original(ctx, id, request)
				switch change {
				case "mount-id":
					s.healthMountFn = func(context.Context, string) (mount.Entry, bool, error) {
						return mount.Entry{Source: "/dev/nvme0n1", FSType: fsTypeXFS, Device: "259:0", MountID: 43}, true, nil
					}
				case "nqn":
					writeNVMeFixtureValue(t, filepath.Join(filepath.Dir(sysBlockPath), "owners", "nvme7", "subsysnqn"), nqnB)
				case "devnode":
					s.nvmeDeviceNumberFn = func(string) (string, error) { return "259:1", nil }
				case "uuid":
					writeNVMeFixtureValue(t, filepath.Join(sysBlockPath, "nvme0n1", "uuid"), "22222222-2222-2222-2222-222222222222")
				case "empty-state":
					writeNVMeFixtureValue(t, filepath.Join(filepath.Dir(sysBlockPath), "owners", "nvme7", "state"), "")
				case "missing-binding":
					result.StagingNQN = ""
				case "wrong-binding":
					result.StagingNQN = nqnB
				case "missing-record":
					result.PolicyRecord = nvmeRecordName(id)
				}
				return result, err
			}
			if response, err := s.NodeGetVolumeHealth(context.Background(), &csi.NodeGetVolumeHealthRequest{VolumeId: "v", StagingTargetPath: path}); err == nil {
				t.Fatalf("accepted %s: %+v", change, response)
			}
		})
	}
}

func TestHealthAndStatsHonorCancellationAndUnavailableProbe(t *testing.T) {
	s, path := xfsHealthFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.NodeGetVolumeHealth(ctx, &csi.NodeGetVolumeHealthRequest{VolumeId: "v", StagingTargetPath: path}); status.Code(err) != codes.Canceled {
		t.Fatal(err)
	}
	s.healthProbeFn = func(context.Context, string, volumeprobe.Request) (volumeprobe.Result, error) {
		return volumeprobe.Result{}, volumeprobe.ErrBusy
	}
	if _, err := s.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: "v", VolumePath: path}); status.Code(err) != codes.Unavailable {
		t.Fatal(err)
	}
	s.healthMountFn = func(context.Context, string) (mount.Entry, bool, error) {
		return mount.Entry{}, false, errors.New("unreadable mount table")
	}
	if _, err := s.NodeGetVolumeHealth(context.Background(), &csi.NodeGetVolumeHealthRequest{VolumeId: "v", StagingTargetPath: path}); status.Code(err) != codes.Unavailable {
		t.Fatal(err)
	}
}

func TestBoundedStatsUseObservedCountersAndRejectBadEvidence(t *testing.T) {
	s, path := xfsHealthFixture(t)
	response, err := s.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: "v", VolumePath: path, StagingTargetPath: path})
	if err != nil || len(response.GetUsage()) != 2 || response.Usage[0].Total != 1024 || response.Usage[0].Used != 512 || response.Usage[0].Available != 256 || response.Usage[1].Used != 20 {
		t.Fatalf("stats=%+v %v", response, err)
	}
	for _, result := range []volumeprobe.Result{{Code: volumeprobe.IOFailure}, {Code: volumeprobe.Unsupported}, {Code: volumeprobe.OK, TotalBytes: 1, FreeBytes: 2}} {
		result.StagingNQN = nqnA
		s.healthProbeFn = func(context.Context, string, volumeprobe.Request) (volumeprobe.Result, error) { return result, nil }
		if response, err := s.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: "v", VolumePath: path}); err == nil || response != nil {
			t.Fatalf("fabricated stats: %+v %v", response, err)
		}
	}
}

func TestAlreadyMountedNVMeStageRequiresLivenessWithoutReset(t *testing.T) {
	for _, code := range []string{volumeprobe.OK, volumeprobe.IOFailure, volumeprobe.Unsupported} {
		t.Run(code, func(t *testing.T) {
			s, path := xfsHealthFixture(t)
			s.healthProbeFn = func(context.Context, string, volumeprobe.Request) (volumeprobe.Result, error) {
				return volumeprobe.Result{Code: code, StagingNQN: nqnA}, nil
			}
			capability := &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: fsTypeXFS}}}
			s.disconnectNVMeOFFn = func(context.Context, string) error { t.Fatal("health reset a connection"); return nil }
			s.formatDeviceFn = func(context.Context, string, string, string) error {
				t.Fatal("health formatted a mounted volume")
				return nil
			}
			response, err := s.stageNVMeDevice(context.Background(), "v", "/dev/nvme0n1", path, capability, false, map[string]string{"server": "192.0.2.10", "nqn": nqnA})
			if (err == nil) != (code == volumeprobe.OK) {
				t.Fatalf("stage=%+v %v", response, err)
			}
		})
	}
}

func TestHealthUsesDurableBindingAndRechecksItsEndpoint(t *testing.T) {
	for _, change := range []string{"none", "endpoint", "record-deleted"} {
		t.Run(change, func(t *testing.T) {
			s, path := xfsHealthFixture(t)
			params, err := s.validateNVMeOFParams(map[string]string{"server": "192.0.2.10", "nqn": nqnA, nvmeMaxSectorsKey: "1024"})
			if err != nil {
				t.Fatal(err)
			}
			if prepareErr := s.prepareNVMePolicy(context.Background(), "v", "/dev/nvme0n1", path, params); prepareErr != nil {
				t.Fatal(prepareErr)
			}
			s.healthProbeFn = func(context.Context, string, volumeprobe.Request) (volumeprobe.Result, error) {
				if change == "endpoint" {
					writeNVMeFixtureValue(t, filepath.Join(filepath.Dir(sysBlockPath), "owners", "nvme7", "address"), "traddr=192.0.2.99,trsvcid=4420")
				}
				if change == "record-deleted" {
					if removeErr := os.Remove(filepath.Join(s.nvmePolicyDirectory, nvmeRecordName("v"))); removeErr != nil {
						t.Fatal(removeErr)
					}
				}
				return volumeprobe.Result{Code: volumeprobe.OK, PolicyRecord: nvmeRecordName("v")}, nil
			}
			response, err := s.NodeGetVolumeHealth(context.Background(), &csi.NodeGetVolumeHealthRequest{VolumeId: "v", VolumePublishPath: path})
			if (err == nil) != (change == "none") {
				t.Fatalf("health=%+v %v", response, err)
			}
		})
	}
}

func TestRawBlockStatsOmitFilesystemUsage(t *testing.T) {
	s, path := xfsHealthFixture(t)
	writeNVMeFixtureValue(t, filepath.Join(sysBlockPath, "nvme4n1", "dev"), "259:1")
	s.healthMountFn = func(context.Context, string) (mount.Entry, bool, error) {
		return mount.Entry{Source: "devtmpfs", FSType: "devtmpfs", Device: "0:5", MountID: 42}, true, nil
	}
	s.healthProbeFn = func(context.Context, string, volumeprobe.Request) (volumeprobe.Result, error) {
		return volumeprobe.Result{Code: volumeprobe.OK, RawDevice: "259:0", StagingNQN: nqnA, TotalBytes: 4096}, nil
	}
	response, err := s.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: "v", VolumePath: path, StagingTargetPath: path})
	if err != nil || len(response.GetUsage()) != 1 || response.Usage[0].Total != 4096 || response.Usage[0].Used != 0 || response.Usage[0].Available != 0 {
		t.Fatalf("raw stats: %+v %v", response, err)
	}
}

func TestHealthResolvesParentSymlinksInsideProbe(t *testing.T) {
	s, path := xfsHealthFixture(t)
	canonical := filepath.Join(t.TempDir(), "canonical")
	entry := mount.Entry{Source: "/dev/nvme0n1", FSType: fsTypeXFS, Device: "259:0", MountID: 42}
	s.healthMountFn = func(_ context.Context, p string) (mount.Entry, bool, error) { return entry, p == canonical, nil }
	calls := 0
	s.healthProbeFn = func(_ context.Context, _ string, request volumeprobe.Request) (volumeprobe.Result, error) {
		calls++
		if request.Path != path {
			t.Fatal("observation switched away from the caller's path")
		}
		if request.ResolveMount {
			return volumeprobe.Result{Code: volumeprobe.OK, Mount: &volumeprobe.ResolvedMount{Path: canonical, Source: entry.Source, Device: entry.Device, Filesystem: entry.FSType, MountID: entry.MountID}}, nil
		}
		return volumeprobe.Result{Code: volumeprobe.OK, StagingNQN: nqnA}, nil
	}
	if _, err := s.NodeGetVolumeHealth(context.Background(), &csi.NodeGetVolumeHealthRequest{VolumeId: "v", StagingTargetPath: path}); err != nil || calls != 2 {
		t.Fatalf("bounded resolution: %d %v", calls, err)
	}
}

func TestMissingMountAndUncertainResolutionAreDistinct(t *testing.T) {
	s, path := xfsHealthFixture(t)
	s.healthMountFn = func(context.Context, string) (mount.Entry, bool, error) { return mount.Entry{}, false, nil }
	for _, code := range []string{volumeprobe.Missing, volumeprobe.Unknown} {
		s.healthProbeFn = func(context.Context, string, volumeprobe.Request) (volumeprobe.Result, error) {
			return volumeprobe.Result{Code: code}, nil
		}
		response, err := s.NodeGetVolumeHealth(context.Background(), &csi.NodeGetVolumeHealthRequest{VolumeId: "v", StagingTargetPath: path})
		if code == volumeprobe.Unknown {
			if status.Code(err) != codes.Unavailable {
				t.Fatal(err)
			}
			continue
		}
		if err != nil || response.GetVolumeHealth().GetHealthStatuses()[0].GetStatus() != csi.VolumeHealthErrorType_INACCESSIBLE {
			t.Fatalf("missing mount: %+v %v", response, err)
		}
	}
}

func TestAlreadyMountedStageRefusesWrongFilesystemOrSource(t *testing.T) {
	for _, source := range []string{"/dev/nvme4n1", "server:/export"} {
		s, path := xfsHealthFixture(t)
		s.healthMountFn = func(context.Context, string) (mount.Entry, bool, error) {
			return mount.Entry{Source: source, FSType: fsTypeXFS, Device: "259:0", MountID: 42}, true, nil
		}
		capability := &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: fsTypeXFS}}}
		if _, err := s.checkMountedNVMeStage(context.Background(), "v", path, nqnA, capability); err == nil {
			t.Fatalf("accepted wrong source %s", source)
		}
		capability.GetMount().FsType = fsTypeExt4
		if _, err := s.checkMountedNVMeStage(context.Background(), "v", path, nqnA, capability); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("accepted wrong filesystem: %v", err)
		}
	}
}

func TestMultipathHealthChecksSelectedControllerNQN(t *testing.T) {
	s, _, _ := nvmeLimitFixture(t, true)
	writeNVMeFixtureValue(t, filepath.Join(filepath.Dir(sysBlockPath), "owners", "nvme7", "state"), "live")
	identity, err := nvmeNamespaceIdentityAt(filepath.Join(sysBlockPath, "nvme0n1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.healthNVMeController("/dev/nvme0n1", identity, nil); err != nil {
		t.Fatal(err)
	}
	writeNVMeFixtureValue(t, filepath.Join(filepath.Dir(sysBlockPath), "owners", "nvme7", "subsysnqn"), nqnB)
	if err := s.healthNVMeController("/dev/nvme0n1", identity, nil); err == nil {
		t.Fatal("accepted mismatched live controller")
	}
}

func TestOtherFilesystemHealthUsesBoundedDirectoryProbe(t *testing.T) {
	for _, fs := range []string{"nfs4", "cifs", "ext4"} {
		t.Run(fs, func(t *testing.T) {
			s := NewNodeService("test", nil, false, nil, false, 1)
			s.nvmePolicyDirectory = t.TempDir()
			s.healthMountFn = func(context.Context, string) (mount.Entry, bool, error) {
				return mount.Entry{Source: "server:/share", FSType: fs, Device: "0:5", MountID: 12}, true, nil
			}
			s.healthProbeFn = func(_ context.Context, _ string, request volumeprobe.Request) (volumeprobe.Result, error) {
				if request.Filesystem != fs {
					t.Fatal(request)
				}
				return volumeprobe.Result{Code: volumeprobe.OK}, nil
			}
			response, err := s.NodeGetVolumeHealth(context.Background(), &csi.NodeGetVolumeHealthRequest{VolumeId: "v", VolumePublishPath: "/mount"})
			if err != nil || len(response.GetVolumeHealth().GetHealthStatuses()) != 0 {
				t.Fatalf("health: %+v %v", response, err)
			}
		})
	}
}

func TestNonNVMeHealthSourceDoesNotReadUnrelatedNamespaces(t *testing.T) {
	useFakeSysBlock(t, map[string]string{"nvme0": nqnA}, map[string]string{"nvme0n1": "nvme0"})
	// The unrelated namespace deliberately has no readable dev metadata.
	source, err := healthNVMeSource("/dev/null")
	if err != nil || source != "" {
		t.Fatalf("non-NVMe source depended on unrelated namespace metadata: %q %v", source, err)
	}
}
