package driver

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/fenio/tns-csi/pkg/volumeprobe"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sysClassNVMePath is injectable so tests never inspect host controllers.
var sysClassNVMePath = "/sys/class/nvme"

// VolumeHealth describes observed usability; it does not establish ownership.
type VolumeHealth struct {
	Message  string
	Abnormal bool
	Degraded bool
}

// Healthy means the requested read-only observations completed.
func Healthy() VolumeHealth { return VolumeHealth{} }

// Unhealthy reports an observed accessibility failure.
func Unhealthy(message string) VolumeHealth { return VolumeHealth{Abnormal: true, Message: message} }

// ToCSI converts observations to CSI 1.13 health entries.
func (h VolumeHealth) ToCSI(volumeID string) *csi.VolumeHealth {
	health := &csi.VolumeHealth{VolumeId: volumeID}
	if h.Abnormal || h.Degraded {
		entry := &csi.VolumeHealth_VolumeHealthEntry{Status: csi.VolumeHealthErrorType_INACCESSIBLE, Reason: "VolumeUnavailable", Message: h.Message}
		if h.Degraded && !h.Abnormal {
			entry.Status = csi.VolumeHealthErrorType_DEGRADED
			entry.Reason = "XFSRootMetadataSick"
		}
		health.HealthStatuses = []*csi.VolumeHealth_VolumeHealthEntry{entry}
	}
	return health
}

func (s *NodeService) checkVolumeHealth(ctx context.Context, volumeID, path, stagingPath string) (VolumeHealth, error) {
	result, err := s.probeMountedVolume(ctx, volumeID, path, stagingPath, "")
	if err != nil {
		return VolumeHealth{}, err
	}
	switch result.Code {
	case volumeprobe.OK:
		if result.RootSick != 0 {
			return VolumeHealth{Degraded: true, Message: "XFS root-inode metadata is marked sick; manual investigation is required"}, nil
		}
		return Healthy(), nil
	case volumeprobe.TransportUnavailable:
		return Unhealthy("block-device transport is not running"), nil
	case volumeprobe.IOFailure:
		return Unhealthy("read-only filesystem observation returned an I/O error"), nil
	case volumeprobe.Missing:
		return Unhealthy("volume is not mounted at the requested path"), nil
	default:
		return VolumeHealth{}, status.Error(codes.Unavailable, "filesystem liveness could not be established")
	}
}

// getNVMeControllerState reads the NVMe controller state from sysfs.
func getNVMeControllerState(devicePath string) (string, error) {
	// Device path is like /dev/nvme0n1 or /dev/nvme0n1p1
	// We need to extract the controller name (nvme0)
	base := filepath.Base(devicePath)
	if !strings.HasPrefix(base, "nvme") {
		return "", fmt.Errorf("%w: %s", ErrNVMeNotNVMeDevice, devicePath)
	}

	// The controller comes from the namespace's sysfs owner, not from its name: under
	// native multipath /dev/nvme1n1 may be served by nvme0/nvme2, and nvme1 may belong to
	// another volume. With several paths, a live one is reported if there is any.
	_, state, err := nvmeControllerForNamespace(sysBlockPath, devicePath)
	if err != nil {
		return "", fmt.Errorf("failed to read NVMe state: %w", err)
	}
	return state, nil
}
