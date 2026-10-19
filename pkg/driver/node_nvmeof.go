package driver

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/fenio/tns-csi/pkg/metrics"
	"github.com/fenio/tns-csi/pkg/mount"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

// Static errors for NVMe-oF operations.
var (
	ErrNVMeCLINotFound             = errors.New("nvme command not found - please install nvme-cli")
	ErrNVMeDeviceNotFound          = errors.New("NVMe device not found")
	ErrNVMeDeviceUnhealthy         = errors.New("NVMe device exists but is unhealthy (zero size)")
	ErrNVMeDeviceTimeout           = errors.New("timeout waiting for NVMe device to appear")
	ErrNVMeSubsystemTimeout        = errors.New("timeout waiting for NVMe subsystem to become live")
	ErrDeviceInitializationTimeout = errors.New("device failed to initialize - size remained zero or unreadable")
	ErrNVMeControllerNotFound      = errors.New("could not extract NVMe controller path from device path")
	ErrDeviceSizeMismatch          = errors.New("device size does not match expected capacity")
	ErrNVMeEmptyNQN                = errors.New("empty NQN in sysfs")
	ErrNVMeNotNVMeDevice           = errors.New("not an NVMe device")
	ErrNVMeNonNVMeStagingDevice    = errors.New("staging path resolved to non-NVMe device")
	ErrNVMeCleanupTimeout          = errors.New("timeout waiting for NVMe-oF controller cleanup")
)

// NVMe subsystem states.
const (
	nvmeSubsystemStateLive = "live"
)

// defaultNVMeOFMountOptions are sensible defaults for NVMe-oF filesystem mounts.
// These are merged with user-specified mount options from StorageClass.
var defaultNVMeOFMountOptions = []string{zfsNoatime}

const nvmeStagingMetadataSuffix = ".tns-csi-nvmeof"

// nvmeOFConnectionParams holds validated NVMe-oF connection parameters.
// With independent subsystems per volume, NSID is always 1.
type nvmeOFConnectionParams struct {
	policy         nvmePolicy
	explicitPolicy nvmePolicy
	connectServer  string
	nqn            string
	server         string
	transport      string
	port           string
	nrIOQueues     string // optional: --nr-io-queues flag value
	queueSize      string // optional: --queue-size flag value
	addressPins    []netip.Addr
}

// stageNVMeOFVolume stages an NVMe-oF volume by connecting to the target.
// With independent subsystems, each volume has its own NQN and NSID is always 1.
func (s *NodeService) stageNVMeOFVolume(ctx context.Context, req *csi.NodeStageVolumeRequest, volumeContext map[string]string) (*csi.NodeStageVolumeResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	volumeID := req.GetVolumeId()
	stagingTargetPath := req.GetStagingTargetPath()
	volumeCapability := req.GetVolumeCapability()

	// Validate and extract connection parameters
	params, err := s.validateNVMeOFParams(volumeContext)
	if err != nil {
		return nil, err
	}
	isBlockVolume := volumeCapability.GetBlock() != nil
	if err := s.verifyNVMeStagingSource(ctx, stagingTargetPath, params.nqn, isBlockVolume); err != nil {
		return nil, nvmeLifecycleError(ctx, "staging path belongs to another or unknown namespace", err)
	}
	if err := writeNVMeStagingNQN(stagingTargetPath, params.nqn); err != nil {
		return nil, nvmeLifecycleError(ctx, "failed to persist NVMe-oF staging metadata", err)
	}

	datasetName := volumeContext["datasetName"]
	klog.V(4).Infof("Staging NVMe-oF volume %s (block mode: %v): server=%s:%s, NQN=%s, dataset=%s",
		volumeID, isBlockVolume, params.server, params.port, params.nqn, datasetName)

	// Try to reuse existing connection (idempotent staging)
	if resp, reused, reuseErr := s.tryReuseExistingConnection(ctx, params, volumeID, stagingTargetPath, volumeCapability, isBlockVolume, volumeContext); reuseErr != nil {
		return nil, reuseErr
	} else if reused {
		return resp, nil
	}

	// Check if nvme-cli is installed
	if checkErr := s.nvmeOperations().checkCLI(ctx); checkErr != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "nvme-cli not available: %v", checkErr)
	}

	// Acquire semaphore to limit concurrent NVMe-oF connect operations.
	// This prevents overwhelming the kernel's NVMe subsystem registration lock
	// when many volumes are being staged simultaneously.
	klog.V(4).Infof("Waiting for NVMe-oF connect semaphore (capacity: %d) for NQN: %s", cap(s.nvmeConnectSem), params.nqn)
	metrics.NVMeConnectWaiting()
	select {
	case s.nvmeConnectSem <- struct{}{}:
		metrics.NVMeConnectDoneWaiting()
		metrics.NVMeConnectStart()
		defer func() {
			<-s.nvmeConnectSem
			metrics.NVMeConnectDone()
		}()
	case <-ctx.Done():
		metrics.NVMeConnectDoneWaiting()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	klog.V(4).Infof("Acquired NVMe-oF connect semaphore for NQN: %s", params.nqn)

	// Connect to NVMe-oF target and stage device
	return s.connectAndStageDevice(ctx, params, volumeID, stagingTargetPath, volumeCapability, isBlockVolume, volumeContext, datasetName)
}

// tryReuseExistingConnection attempts to reuse an existing NVMe-oF connection.
// With independent subsystems, we simply check if the device for this NQN exists.
func (s *NodeService) tryReuseExistingConnection(ctx context.Context, params *nvmeOFConnectionParams, volumeID, stagingTargetPath string, volumeCapability *csi.VolumeCapability, isBlockVolume bool, volumeContext map[string]string) (*csi.NodeStageVolumeResponse, bool, error) {
	devicePath, findErr := s.findNVMeDeviceByNQN(ctx, params.nqn)
	if errors.Is(findErr, ErrNVMeDeviceNotFound) {
		return nil, false, nil
	}
	if findErr != nil {
		return nil, false, nvmeLifecycleError(ctx, "cannot reuse NVMe namespace", findErr)
	}
	if devicePath == "" {
		return nil, false, nvmeLifecycleError(ctx, "cannot establish NVMe identity", ErrNVMeDiscoveryUnknown)
	}
	ops := s.nvmeOperations()
	if err := verifyNVMeNQN(devicePath, params.nqn); err != nil {
		return nil, false, nvmeLifecycleError(ctx, "namespace identity cannot be verified", err)
	}
	if err := ops.rescan(ctx, devicePath); err != nil {
		return nil, false, nvmeLifecycleError(ctx, "namespace rescan failed", err)
	}
	if err := verifyNVMeNQN(devicePath, params.nqn); err != nil {
		return nil, false, nvmeLifecycleError(ctx, "namespace identity changed after rescan", err)
	}
	if err := s.prepareNVMePolicy(ctx, volumeID, devicePath, stagingTargetPath, params); err != nil {
		return nil, false, nvmeLifecycleError(ctx, "NVMe policy could not be enforced after rescan", err)
	}
	if len(params.policy.parameters()) > 0 {
		klog.V(4).Info("Reusing NVMe connection; connection-only policy takes effect on the next normal connection creation")
	}
	if err := s.verifyDeviceReady(ctx, devicePath); err != nil {
		return nil, false, nvmeLifecycleError(ctx, "namespace capacity unavailable", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, false, status.FromContextError(err).Err()
	}
	resp, err := ops.stage(ctx, volumeID, devicePath, stagingTargetPath, volumeCapability, isBlockVolume, volumeContext)
	return resp, true, err
}

func (s *NodeService) verifyDeviceReady(ctx context.Context, devicePath string) error {
	const maxAttempts = 5
	ops := s.nvmeOperations()
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		size, err := ops.readSize(ctx, devicePath)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil && size > 0 {
			return nil
		}
		lastErr = err
		if lastErr == nil {
			lastErr = ErrNVMeDeviceUnhealthy
		}
		if attempt < maxAttempts {
			if err := waitNVMeContext(ctx, 500*time.Millisecond); err != nil {
				return err
			}
		}
	}
	return lastErr
}

// connectAndStageDevice retains uncertain connections for a later CSI retry.
func (s *NodeService) connectAndStageDevice(ctx context.Context, params *nvmeOFConnectionParams, volumeID, stagingTargetPath string, volumeCapability *csi.VolumeCapability, isBlockVolume bool, volumeContext map[string]string, _ string) (*csi.NodeStageVolumeResponse, error) {
	ops := s.nvmeOperations()
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if err := ops.connect(ctx, params); err != nil {
		return nil, nvmeLifecycleError(ctx, "NVMe connect failed", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if err := ops.waitLive(ctx, params.nqn, 60*time.Second); err != nil {
		return nil, nvmeLifecycleError(ctx, "NVMe subsystem not ready", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	devicePath, err := ops.waitDevice(ctx, params.nqn, 60*time.Second)
	if err != nil {
		return nil, nvmeLifecycleError(ctx, "NVMe namespace not ready", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	return ops.stage(ctx, volumeID, devicePath, stagingTargetPath, volumeCapability, isBlockVolume, volumeContext)
}

// validateNVMeOFParams validates and extracts NVMe-oF connection parameters from volume context.
// With independent subsystems, nsid is not required (always 1).
func (s *NodeService) validateNVMeOFParams(volumeContext map[string]string) (*nvmeOFConnectionParams, error) {
	params := &nvmeOFConnectionParams{
		nqn:        volumeContext["nqn"],
		server:     volumeContext["server"],
		transport:  volumeContext["transport"],
		port:       volumeContext["port"],
		nrIOQueues: volumeContext["nvmeof.nr-io-queues"],
		queueSize:  volumeContext["nvmeof.queue-size"],
	}

	if params.nqn == "" || params.server == "" {
		return nil, status.Error(codes.InvalidArgument, "nqn and server must be provided in volume context for NVMe-oF volumes")
	}

	// Default values
	if params.transport == "" {
		params.transport = nvmeTransportTCP
	}
	if params.port == "" {
		params.port = "4420"
	}

	explicit, policyErr := parseNVMePolicy(volumeContext, false)
	if policyErr != nil {
		return nil, status.Error(codes.InvalidArgument, policyErr.Error())
	}
	profile, pins, profileErr := s.nvmeNodePolicy.resolve(params)
	if profileErr != nil {
		return nil, status.Error(codes.FailedPrecondition, profileErr.Error())
	}
	params.explicitPolicy = explicit
	params.policy = mergeNVMePolicies(explicit, profile)
	params.nrIOQueues = params.policy.legacy[nvmeIOQueuesKey]
	params.queueSize = params.policy.legacy[nvmeQueueSizeKey]
	if len(params.policy.values) > 0 || len(pins) > 0 {
		if params.transport != nvmeTransportTCP {
			return nil, status.Error(codes.FailedPrecondition, "scoped NVMe policy requires TCP")
		}
		if len(pins) == 0 {
			server, canonicalErr := canonicalNVMeServer(params.server)
			address, parseErr := netip.ParseAddr(server)
			if canonicalErr != nil || parseErr != nil {
				return nil, status.Error(codes.FailedPrecondition, "hostname policy requires a node profile with pinned numeric addresses")
			}
			pins = []netip.Addr{address.Unmap()}
		}
		port, portErr := strconv.ParseUint(params.port, 10, 16)
		if portErr != nil || port == 0 || strconv.FormatUint(port, 10) != params.port {
			return nil, status.Error(codes.InvalidArgument, "scoped NVMe policy requires a canonical decimal TCP port")
		}
		params.addressPins = pins
		params.connectServer = pins[0].String()
	}

	return params, nil
}

// stageNVMeDevice stages an NVMe device as either block or filesystem volume.
func (s *NodeService) stageNVMeDevice(ctx context.Context, volumeID, devicePath, stagingTargetPath string, volumeCapability *csi.VolumeCapability, isBlockVolume bool, volumeContext map[string]string) (*csi.NodeStageVolumeResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	params, err := s.validateNVMeOFParams(volumeContext)
	if err != nil {
		return nil, err
	}
	if err := verifyNVMeNQN(devicePath, params.nqn); err != nil {
		return nil, nvmeLifecycleError(ctx, "cannot verify staging namespace", err)
	}
	if err := s.prepareNVMePolicy(ctx, volumeID, devicePath, stagingTargetPath, params); err != nil {
		return nil, nvmeLifecycleError(ctx, "NVMe policy could not be enforced before staging", err)
	}
	if !isBlockVolume {
		mounted, mountErr := s.checkMountedNVMeStage(ctx, volumeID, stagingTargetPath, params.nqn, volumeCapability)
		if mountErr != nil {
			return nil, mountErr
		}
		if mounted {
			return &csi.NodeStageVolumeResponse{}, nil
		}
	}
	// For filesystem volumes, wait for device to be fully initialized.
	if !isBlockVolume {
		// First, wait for device to report non-zero size (indicates device is initialized)
		if err := waitForDeviceInitialization(ctx, devicePath); err != nil {
			return nil, status.Errorf(codes.Internal, "Device initialization timeout: %v", err)
		}

		// Force the kernel to completely re-read the device identity
		if err := forceDeviceRescan(ctx, devicePath); err != nil {
			klog.Warningf("Device rescan warning for %s: %v (continuing anyway)", devicePath, err)
		}
		if err := verifyNVMeNQN(devicePath, params.nqn); err != nil {
			return nil, nvmeLifecycleError(ctx, "namespace changed during staging rescan", err)
		}
		if err := s.prepareNVMePolicy(ctx, volumeID, devicePath, stagingTargetPath, params); err != nil {
			return nil, nvmeLifecycleError(ctx, "NVMe policy could not be enforced after staging rescan", err)
		}

		// Additional stabilization delay to ensure metadata is readable after rescan
		const deviceMetadataDelay = 2 * time.Second
		klog.V(4).Infof("Waiting %v for device %s metadata to stabilize after rescan", deviceMetadataDelay, devicePath)
		if err := waitNVMeContext(ctx, deviceMetadataDelay); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		klog.V(4).Infof("Device metadata stabilization delay complete for %s", devicePath)
	}

	if isBlockVolume {
		if err := s.verifyNVMeStagingSource(ctx, stagingTargetPath, params.nqn, true); err != nil {
			return nil, nvmeLifecycleError(ctx, "block staging source cannot be verified", err)
		}
		return s.stageBlockDevice(devicePath, stagingTargetPath)
	}
	return s.formatAndMountNVMeDevice(ctx, volumeID, devicePath, stagingTargetPath, volumeCapability, volumeContext)
}

// unstageNVMeOFVolume unstages an NVMe-oF volume by disconnecting from the target.
// CSI unpublish ordering is required; observed users or uncertain identity refuse teardown.
func (s *NodeService) unstageNVMeOFVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest, volumeContext map[string]string) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	stagingTargetPath := req.GetStagingTargetPath()

	klog.V(4).Infof("Unstaging NVMe-oF volume %s from %s", volumeID, stagingTargetPath)

	// Get NQN from volume context
	nqn := volumeContext["nqn"]
	if nqn == "" {
		derivedNQN, deriveErr := s.deriveNQNFromStagingPath(ctx, stagingTargetPath)
		if deriveErr != nil {
			klog.Warningf("Failed to derive NVMe-oF NQN from staging path %s: %v", stagingTargetPath, deriveErr)
		} else {
			nqn = derivedNQN
			klog.V(4).Infof("Derived NVMe-oF NQN from staging path %s: %s", stagingTargetPath, nqn)
		}
	}
	if nqn == "" {
		// A missing path after a completed unstage is an idempotent success. If the
		// path still exists, losing its NQN is unsafe because a live session may leak.
		if _, statErr := os.Lstat(stagingTargetPath); os.IsNotExist(statErr) {
			klog.V(4).Infof("Staging path %s no longer exists; volume %s is already unstaged", stagingTargetPath, volumeID)
			return &csi.NodeUnstageVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal,
			"cannot determine NQN for NVMe-oF volume %s at staging path %s; refusing to report successful unstage",
			volumeID, stagingTargetPath)
	}

	// CSI does not include VolumeContext in NodeUnstageVolume retries. Persist the
	// NQN before unmounting so a failed disconnect cannot be retried as NFS.
	if err := writeNVMeStagingNQN(stagingTargetPath, nqn); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to persist NVMe-oF staging metadata before unmount: %v", err)
	}

	entry, mounted, err := lookupMountedEntry(ctx, stagingTargetPath)
	if err != nil {
		return nil, nvmeLifecycleError(ctx, "cannot verify staging mount", err)
	}
	if mounted {
		if err := verifyNVMeNQN(entry.Source, nqn); err != nil {
			return nil, nvmeLifecycleError(ctx, "staging source identity conflict", err)
		}
	} else if info, statErr := os.Lstat(stagingTargetPath); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		device, resolveErr := filepath.EvalSymlinks(stagingTargetPath)
		if resolveErr != nil {
			present, presenceErr := nvmeNQNPresent(sysClassNVMePath, nqn)
			if presenceErr != nil || present {
				return nil, nvmeLifecycleError(ctx, "cannot verify raw staging source", errors.Join(resolveErr, presenceErr))
			}
		} else if err := verifyNVMeNQN(device, nqn); err != nil {
			return nil, nvmeLifecycleError(ctx, "raw staging source identity conflict", err)
		}
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return nil, nvmeLifecycleError(ctx, "cannot inspect staging path", statErr)
	}

	if mounted {
		klog.V(4).Infof("Unmounting staging path: %s", stagingTargetPath)
		if err := s.nvmeOperations().unmount(ctx, stagingTargetPath); err != nil {
			return nil, status.Errorf(codes.Internal, "Failed to unmount staging path: %v", err)
		}
	}

	if err := s.checkNVMeTeardownUsers(ctx, nqn); err != nil {
		return nil, nvmeLifecycleError(ctx, "cannot safely unstage NVMe subsystem", err)
	}
	klog.V(4).Infof("Disconnecting NVMe-oF subsystem for volume %s: NQN=%s", volumeID, nqn)
	if err := s.disconnectNVMeOF(ctx, nqn); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to disconnect NVMe-oF volume %s (NQN %s): %v", volumeID, nqn, err)
	}
	// Remove the marker while the NQN sidecar still routes a retry through NVMe unstage.
	if err := removeNVMePolicyMarker(stagingTargetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "disconnected NVMe-oF volume %s but failed to remove staging policy marker: %v", volumeID, err)
	}
	if err := removeNVMeStagingNQN(stagingTargetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "disconnected NVMe-oF volume %s but failed to remove staging metadata: %v", volumeID, err)
	}
	klog.Infof("Disconnected NVMe-oF volume %s from target %s", volumeID, nqn)

	return &csi.NodeUnstageVolumeResponse{}, nil
}

// deriveNQNFromStagingPath derives the NVMe-oF NQN from Linux mount/device metadata.
func (s *NodeService) deriveNQNFromStagingPath(ctx context.Context, stagingTargetPath string) (string, error) {
	if nqn, err := readNVMeStagingNQN(stagingTargetPath); err == nil && nqn != "" {
		return nqn, nil
	} else if err != nil {
		klog.Warningf("Failed to read NVMe-oF staging metadata for %s: %v; falling back to device metadata", stagingTargetPath, err)
	}

	devicePath, err := s.getStagedNVMeDevicePath(ctx, stagingTargetPath)
	if err != nil {
		return "", err
	}

	// The device's own owner (controller or multipath subsystem) carries its NQN; a
	// controller name derived from the device name is wrong under native multipath.
	nqn, err := nvmeNQNForDevice(sysBlockPath, devicePath)
	if err != nil {
		return "", err
	}
	return nqn, nil
}

func nvmeStagingMetadataPath(stagingTargetPath string) string {
	return stagingTargetPath + nvmeStagingMetadataSuffix
}

func writeNVMeStagingNQN(stagingTargetPath, nqn string) error {
	if strings.TrimSpace(nqn) == "" {
		return ErrNVMeEmptyNQN
	}
	if err := os.MkdirAll(filepath.Dir(stagingTargetPath), 0o750); err != nil {
		return err
	}
	path := nvmeStagingMetadataPath(stagingTargetPath)
	file, err := createNVMeMetadataFile(path)
	if errors.Is(err, os.ErrExist) {
		retained, readErr := readNVMeStagingNQN(stagingTargetPath)
		if readErr != nil {
			return readErr
		}
		if retained != nqn {
			return fmt.Errorf("%w: retained NQN differs from requested NQN", ErrNVMeIdentityConflict)
		}
		return nil
	}
	if err != nil {
		return err
	}
	_, writeErr := file.WriteString(nqn + "\n")
	syncErr := file.Sync()
	return errors.Join(writeErr, syncErr, file.Close(), syncNVMeMetadataDirectory(filepath.Dir(path)))
}

func readNVMeStagingNQN(stagingTargetPath string) (string, error) {
	data, err := os.ReadFile(nvmeStagingMetadataPath(stagingTargetPath))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	nqn := strings.TrimSpace(string(data))
	if nqn == "" {
		return "", ErrNVMeEmptyNQN
	}
	return nqn, nil
}

func removeNVMeStagingNQN(stagingTargetPath string) error {
	err := os.Remove(nvmeStagingMetadataPath(stagingTargetPath))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// getStagedNVMeDevicePath resolves the NVMe device backing a staging path.
func (s *NodeService) getStagedNVMeDevicePath(ctx context.Context, stagingTargetPath string) (string, error) {
	// Filesystem mode: the device mounted at the staging path, from the mount table.
	entry, mounted, err := lookupMountedEntry(ctx, stagingTargetPath)
	if err != nil {
		// Fail closed: guessing a device here could disconnect another volume.
		return "", fmt.Errorf("mount table lookup failed for %s: %w", stagingTargetPath, err)
	}
	if mounted && strings.HasPrefix(filepath.Base(entry.Source), "nvme") {
		return entry.Source, nil
	}

	// Block mode: staging path is a symlink to /dev/nvmeXnY.
	resolved, err := filepath.EvalSymlinks(stagingTargetPath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve staging path %s: %w", stagingTargetPath, err)
	}
	if !strings.HasPrefix(filepath.Base(resolved), "nvme") {
		return "", fmt.Errorf("staging path %s resolved to %s: %w", stagingTargetPath, resolved, ErrNVMeNonNVMeStagingDevice)
	}
	return resolved, nil
}

// formatAndMountNVMeDevice formats (if needed) and mounts an NVMe device.
func (s *NodeService) formatAndMountNVMeDevice(ctx context.Context, volumeID, devicePath, stagingTargetPath string, volumeCapability *csi.VolumeCapability, volumeContext map[string]string) (*csi.NodeStageVolumeResponse, error) {
	params, parameterErr := s.validateNVMeOFParams(volumeContext)
	if parameterErr != nil {
		return nil, parameterErr
	}
	if err := verifyNVMeNQN(devicePath, params.nqn); err != nil {
		return nil, nvmeLifecycleError(ctx, "cannot verify namespace before filesystem use", err)
	}
	if err := s.prepareNVMePolicy(ctx, volumeID, devicePath, stagingTargetPath, params); err != nil {
		return nil, nvmeLifecycleError(ctx, "cannot enforce policy before filesystem use", err)
	}
	mounted, mountErr := s.checkMountedNVMeStage(ctx, volumeID, stagingTargetPath, params.nqn, volumeCapability)
	if mountErr != nil {
		return nil, mountErr
	}
	if mounted {
		return &csi.NodeStageVolumeResponse{}, nil
	}
	datasetName := volumeContext["datasetName"]
	nqn := volumeContext["nqn"]
	klog.V(4).Infof("Formatting and mounting NVMe device: device=%s, path=%s, volume=%s, dataset=%s, NQN=%s",
		devicePath, stagingTargetPath, volumeID, datasetName, nqn)

	mounted, mountErr = ensureStagingTarget(ctx, stagingTargetPath)
	if mountErr != nil {
		return nil, mountErr
	}
	if mounted {
		if err := s.verifyNVMeStagingSource(ctx, stagingTargetPath, params.nqn, false); err != nil {
			return nil, nvmeLifecycleError(ctx, "mounted staging source cannot be verified", err)
		}
		if healthErr := s.verifyMountedNVMeHealth(ctx, volumeID, stagingTargetPath, params.nqn); healthErr != nil {
			return nil, healthErr
		}
		klog.V(4).Infof("Staging path %s is already mounted", stagingTargetPath)
		return &csi.NodeStageVolumeResponse{}, nil
	}

	// Verify device still exists before proceeding (it may have disappeared due to race conditions
	// with previous volume cleanup or controller reconnection)
	if _, err := os.Stat(devicePath); err != nil {
		klog.Warningf("NVMe device %s disappeared before staging could complete: %v", devicePath, err)
		return nil, status.Errorf(codes.Unavailable,
			"NVMe device %s became unavailable: %v", devicePath, err)
	}

	// Log device information for troubleshooting
	s.logDeviceInfo(ctx, devicePath)

	// SAFETY CHECK: Verify device size matches expected capacity
	if err := s.verifyDeviceSize(ctx, devicePath, volumeContext); err != nil {
		klog.Errorf("Device size verification FAILED for %s: %v", devicePath, err)
		return nil, status.Errorf(codes.FailedPrecondition,
			"Device size mismatch detected - refusing to mount to prevent data corruption: %v", err)
	}

	// Determine filesystem type from volume capability
	fsType := "ext4" // default
	if mnt := volumeCapability.GetMount(); mnt != nil && mnt.FsType != "" {
		fsType = mnt.FsType
	}

	// Check if this volume was cloned from a snapshot
	isClone := false
	if cloned, exists := volumeContext[VolumeContextKeyClonedFromSnap]; exists && cloned == VolumeContextValueTrue {
		isClone = true
		klog.V(4).Infof("Volume %s was cloned from snapshot - adding extra stabilization delay before filesystem check", volumeID)
		// Reduced delay with independent subsystems (no NSID cache pollution)
		const cloneStabilizationDelay = 5 * time.Second
		klog.V(4).Infof("Waiting %v for cloned volume %s filesystem metadata to stabilize", cloneStabilizationDelay, devicePath)
		if err := waitNVMeContext(ctx, cloneStabilizationDelay); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		klog.V(4).Infof("Clone stabilization delay complete for %s", devicePath)
	}

	// Check if device needs formatting (will detect existing filesystem or format if needed)
	if err := verifyNVMeNQN(devicePath, params.nqn); err != nil {
		return nil, nvmeLifecycleError(ctx, "namespace changed before filesystem probe", err)
	}
	if err := s.prepareNVMePolicy(ctx, volumeID, devicePath, stagingTargetPath, params); err != nil {
		return nil, nvmeLifecycleError(ctx, "cannot enforce policy before filesystem probe", err)
	}
	newlyFormatted, formatErr := s.handleDeviceFormatting(ctx, volumeID, devicePath, fsType, datasetName, nqn, isClone)
	if formatErr != nil {
		return nil, formatErr
	}
	if checkErr := s.checkFilesystemBeforeMount(ctx, devicePath, volumeContext[VolumeContextKeyFilesystemCheck], newlyFormatted); checkErr != nil {
		return nil, checkErr
	}

	// Mount the device
	klog.V(4).Infof("Mounting device %s to %s", devicePath, stagingTargetPath)

	// Get user-specified mount options from StorageClass (passed via VolumeCapability)
	var userMountOptions []string
	if mnt := volumeCapability.GetMount(); mnt != nil {
		userMountOptions = mnt.MountFlags
	}
	mountOptions := getNVMeOFMountOptions(userMountOptions)

	klog.V(4).Infof("NVMe-oF mount options: user=%v, final=%v", userMountOptions, mountOptions)

	args := []string{devicePath, stagingTargetPath}
	if len(mountOptions) > 0 {
		args = []string{"-o", mount.JoinMountOptions(mountOptions), devicePath, stagingTargetPath}
	}

	output, err := runMount(ctx, 30*time.Second, args...)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to mount device: %v, output: %s", err, string(output))
	}

	klog.V(4).Infof("Mounted NVMe device to staging path")
	return &csi.NodeStageVolumeResponse{}, nil
}

// getNVMeOFMountOptions merges user-provided mount options with sensible defaults.
// User options take precedence - if a user specifies an option that conflicts
// with a default, the user's option wins.
// This allows StorageClass mountOptions to fully customize NVMe-oF filesystem mount behavior.
func getNVMeOFMountOptions(userOptions []string) []string {
	if len(userOptions) == 0 {
		return defaultNVMeOFMountOptions
	}

	// Build a map of option keys that the user has specified
	// This handles both key=value options and flags (e.g., "noatime", "ro")
	userOptionKeys := make(map[string]bool)
	for _, opt := range userOptions {
		key := extractNVMeOFOptionKey(opt)
		userOptionKeys[key] = true
	}

	// Start with user options, then add defaults that don't conflict
	result := make([]string, 0, len(userOptions)+len(defaultNVMeOFMountOptions))
	result = append(result, userOptions...)

	for _, defaultOpt := range defaultNVMeOFMountOptions {
		key := extractNVMeOFOptionKey(defaultOpt)
		if !userOptionKeys[key] {
			result = append(result, defaultOpt)
		}
	}

	return result
}

// extractNVMeOFOptionKey extracts the key from a mount option.
// For "key=value" options, returns "key".
// For flag options like "noatime" or "ro", returns the flag itself.
func extractNVMeOFOptionKey(option string) string {
	for i, c := range option {
		if c == '=' {
			return option[:i]
		}
	}
	return option
}
