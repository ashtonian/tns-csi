package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *NodeService) verifyNVMeStagingSource(ctx context.Context, path, nqn string, block bool) error {
	entry, mounted, err := lookupMountedEntry(ctx, path)
	if err != nil {
		return err
	}
	if mounted {
		return verifyNVMeNQN(entry.Source, nqn)
	}
	if !block {
		return nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		// kubelet creates the raw-block staging path as an empty directory before NodeStageVolume.
		entries, readErr := os.ReadDir(path)
		if readErr != nil {
			return readErr
		}
		if len(entries) > 0 {
			return ErrNVMeIdentityConflict
		}
		return nil
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return ErrNVMeIdentityConflict
	}
	device, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	return verifyNVMeNQN(device, nqn)
}

func (s *NodeService) prepareNVMePublish(ctx context.Context, req *csi.NodePublishVolumeRequest) error {
	parameters := req.GetVolumeContext()
	var params *nvmeOFConnectionParams
	var err error
	if parameters["nqn"] == "" && parameters["server"] == "" {
		params, err = s.restoreNVMeNodeParams(ctx, req.GetVolumeId(), req.GetStagingTargetPath())
	} else {
		params, err = s.validateNVMeOFParams(parameters)
	}
	if errors.Is(err, errNVMePolicyRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if params == nil {
		return nil
	}
	device, err := s.getStagedNVMeDevicePath(ctx, req.GetStagingTargetPath())
	if err != nil {
		return nvmeLifecycleError(ctx, "cannot identify publication namespace", err)
	}
	if err := verifyNVMeNQN(device, params.nqn); err != nil {
		return nvmeLifecycleError(ctx, "publication namespace differs from volume", err)
	}
	if err := s.prepareNVMePolicy(ctx, req.GetVolumeId(), device, req.GetStagingTargetPath(), params); err != nil {
		return nvmeLifecycleError(ctx, "cannot enforce policy before publication", err)
	}
	return nil
}

func (s *NodeService) prepareNVMeExpand(ctx context.Context, req *csi.NodeExpandVolumeRequest, device string) (func() error, error) {
	params, err := s.restoreNVMeNodeParams(ctx, req.GetVolumeId(), req.GetStagingTargetPath())
	if errors.Is(err, errNVMePolicyRecordNotFound) {
		return func() error { return ctx.Err() }, nil
	}
	if err != nil {
		return nil, err
	}
	if device == "" {
		return nil, status.Error(codes.FailedPrecondition, "expansion source is not a verified NVMe namespace")
	}
	apply := func() error {
		if err := verifyNVMeNQN(device, params.nqn); err != nil {
			return nvmeLifecycleError(ctx, "expansion namespace differs from volume", err)
		}
		if err := s.prepareNVMePolicy(ctx, req.GetVolumeId(), device, req.GetStagingTargetPath(), params); err != nil {
			return nvmeLifecycleError(ctx, "cannot enforce policy during expansion", err)
		}
		return nil
	}
	if err := apply(); err != nil {
		return nil, err
	}
	return apply, nil
}

func nvmeExpansionSource(device string) string {
	if resolved, err := filepath.EvalSymlinks(device); err == nil {
		device = resolved
	}
	if filepath.Dir(device) == "/dev" && nvmeNamespaceName.MatchString(filepath.Base(device)) {
		return device
	}
	return ""
}

func (s *NodeService) expandBlockVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	resolve := s.nvmeExpandDeviceFn
	if resolve == nil {
		resolve = nvmeBlockDeviceForPath
	}
	device, deviceErr := resolve(req.GetVolumePath())
	if deviceErr != nil {
		return nil, nvmeLifecycleError(ctx, "cannot identify block expansion source", deviceErr)
	}
	record, recordErr := s.readNVMeNodeRecord(req.GetVolumeId(), req.GetStagingTargetPath())
	if (recordErr != nil && !errors.Is(recordErr, errNVMePolicyRecordNotFound)) || (record != nil && device == "") {
		return nil, status.Error(codes.FailedPrecondition, "block expansion source cannot be bound to its retained NVMe policy")
	}
	if device != "" {
		if _, err := s.prepareNVMeExpand(ctx, req, device); err != nil {
			return nil, err
		}
	}
	return &csi.NodeExpandVolumeResponse{CapacityBytes: req.GetCapacityRange().GetRequiredBytes()}, nil
}

func (s *NodeService) prepareNVMeFilesystemExpand(ctx context.Context, req *csi.NodeExpandVolumeRequest, source string) (func() error, error) {
	record, recordErr := s.readNVMeNodeRecord(req.GetVolumeId(), req.GetStagingTargetPath())
	device := nvmeExpansionSource(source)
	if (recordErr != nil && !errors.Is(recordErr, errNVMePolicyRecordNotFound)) || (record != nil && device == "") {
		return nil, status.Error(codes.FailedPrecondition, "expansion source cannot be bound to its retained NVMe policy")
	}
	if device != "" {
		return s.prepareNVMeExpand(ctx, req, device)
	}
	return func() error { return ctx.Err() }, nil
}

// A raw bind mount's mountinfo source names devtmpfs, not the block device.
// Match its device number to sysfs instead of guessing from the publish path.
func nvmeBlockDeviceNumber(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return "", nil
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", ErrNVMeDiscoveryUnknown
	}
	rdev, parseErr := nvmeUnsignedRdev(stat.Rdev)
	if parseErr != nil {
		return "", ErrNVMeDiscoveryUnknown
	}
	return fmt.Sprintf("%d:%d", unix.Major(rdev), unix.Minor(rdev)), nil
}

// Stat_t uses signed device numbers on Darwin and unsigned ones on Linux.
func nvmeUnsignedRdev[T ~int32 | ~uint32 | ~int64 | ~uint64](value T) (uint64, error) {
	if value < 0 {
		return 0, ErrNVMeDiscoveryUnknown
	}
	return uint64(value), nil
}

func nvmeBlockDeviceForPath(path string) (string, error) {
	deviceNumber, err := nvmeBlockDeviceNumber(path)
	if err != nil || deviceNumber == "" {
		return "", err
	}
	entries, err := os.ReadDir(sysBlockPath)
	if err != nil {
		return "", err
	}
	matched := ""
	for _, entry := range entries {
		if !nvmeNamespaceName.MatchString(entry.Name()) {
			continue
		}
		number, err := readSysfsValue(filepath.Join(sysBlockPath, entry.Name(), "dev"))
		if err != nil {
			return "", err
		}
		if number == deviceNumber {
			if matched != "" {
				return "", ErrNVMeIdentityConflict
			}
			matched = "/dev/" + entry.Name()
		}
	}
	return matched, nil
}
