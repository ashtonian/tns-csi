package driver

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/fenio/tns-csi/pkg/cmdexec"
	"github.com/fenio/tns-csi/pkg/mount"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	// ErrNVMeDiscoveryUnknown means the available evidence cannot establish absence.
	ErrNVMeDiscoveryUnknown = errors.New("NVMe discovery evidence is incomplete")
	// ErrNVMeIdentityConflict means the requested transition affects a different identity or user.
	ErrNVMeIdentityConflict = errors.New("NVMe identity conflict")

	errNVMeControllerRemoved = errors.New("NVMe controller was removed during inspection")
)

type nvmeLifecycleOperations struct {
	checkCLI   func(context.Context) error
	connect    func(context.Context, *nvmeOFConnectionParams) error
	waitLive   func(context.Context, string, time.Duration) error
	waitDevice func(context.Context, string, time.Duration) (string, error)
	stage      func(context.Context, string, string, string, *csi.VolumeCapability, bool, map[string]string) (*csi.NodeStageVolumeResponse, error)
	readSize   func(context.Context, string) (int64, error)
	rescan     func(context.Context, string) error
	unmount    func(context.Context, string) error
}

func (s *NodeService) nvmeOperations() nvmeLifecycleOperations {
	ops := s.nvmeOps
	if ops.checkCLI == nil {
		ops.checkCLI = s.checkNVMeCLI
	}
	if ops.connect == nil {
		ops.connect = s.connectNVMeOFTarget
	}
	if ops.waitLive == nil {
		ops.waitLive = waitForSubsystemLive
	}
	if ops.waitDevice == nil {
		ops.waitDevice = s.waitForNVMeDevice
	}
	if ops.stage == nil {
		ops.stage = s.stageNVMeDevice
	}
	if ops.readSize == nil {
		ops.readSize = readNVMeDeviceSize
	}
	if ops.rescan == nil {
		ops.rescan = s.rescanNVMeNamespace
	}
	if ops.unmount == nil {
		ops.unmount = mount.Unmount
	}
	return ops
}

func readNVMeDeviceSize(ctx context.Context, devicePath string) (int64, error) {
	output, err := cmdexec.CombinedOutput(ctx, 3*time.Second, "blockdev", "--getsize64", devicePath)
	if err != nil {
		return 0, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid capacity for %s: %w", devicePath, err)
	}
	if size <= 0 {
		return 0, fmt.Errorf("%w: %s reports %d bytes", ErrNVMeDeviceUnhealthy, devicePath, size)
	}
	return size, nil
}

func verifyNVMeNQN(devicePath, nqn string) error {
	identity, err := nvmeNQNForDevice(sysBlockPath, devicePath)
	if err != nil {
		return err
	}
	if identity != nqn {
		return ErrNVMeIdentityConflict
	}
	return nil
}

func nvmeLifecycleError(ctx context.Context, operation string, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	if status.Code(err) != codes.Unknown {
		return err
	}
	code := codes.Unavailable
	if errors.Is(err, ErrNVMeIdentityConflict) || errors.Is(err, errNVMeAmbiguousNamespace) || errors.Is(err, ErrNVMeEmptyNQN) || errors.Is(err, fs.ErrPermission) {
		code = codes.FailedPrecondition
	}
	return status.Errorf(code, "%s; retaining NVMe connection: %v", operation, err)
}

func waitNVMeContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

// readNVMeControllerNQN never treats an unreadable identity as a negative match.
// The kernel removes a controller's class link before its attributes, so a listed
// controller whose entry has since vanished is reported as removed, not unknown.
func readNVMeControllerNQN(root, controller string) (string, error) {
	nqn, err := readSysfsValue(filepath.Join(root, controller, "subsysnqn"))
	if errors.Is(err, fs.ErrNotExist) {
		if _, statErr := os.Stat(filepath.Join(root, controller)); errors.Is(statErr, fs.ErrNotExist) {
			return "", errNVMeControllerRemoved
		}
	}
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrNVMeDiscoveryUnknown, controller, err)
	}
	if nqn == "" {
		return "", fmt.Errorf("%w: %s: %w", ErrNVMeDiscoveryUnknown, controller, ErrNVMeEmptyNQN)
	}
	return nqn, nil
}

// Negative mount evidence supplements CSI unpublish ordering; it does not fence
// arbitrary external writers or prove that no process holds an open raw device.
func (s *NodeService) checkNVMeTeardownUsers(ctx context.Context, nqn string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	namespaces, err := nvmeNamespaceDevicesForNQN(sysBlockPath, nqn)
	if errors.Is(err, ErrNVMeDeviceNotFound) {
		present, presenceErr := nvmeNQNPresent(sysClassNVMePath, nqn)
		if presenceErr != nil {
			return presenceErr
		}
		if present {
			return fmt.Errorf("%w: connected subsystem exposes no verifiable namespace", ErrNVMeDiscoveryUnknown)
		}
		return nil
	}
	if err != nil {
		return err
	}
	probe := s.isSourceMountedFn
	if probe == nil {
		probe = mount.IsSourceMounted
	}
	for _, namespace := range namespaces {
		nsid, err := readSysfsValue(filepath.Join(sysBlockPath, namespace, "nsid"))
		if err != nil {
			return err
		}
		if nsid != "1" {
			return fmt.Errorf("%w: unexpected namespace %s (NSID %s)", ErrNVMeIdentityConflict, namespace, nsid)
		}
		inUse, err := probe(ctx, "/dev/"+namespace)
		if err != nil {
			return err
		}
		if inUse {
			return fmt.Errorf("%w: namespace %s still has a mounted consumer", ErrNVMeIdentityConflict, namespace)
		}
	}
	return ctx.Err()
}

func createNVMeMetadataFile(path string) (*os.File, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	file, openErr := root.OpenFile(filepath.Base(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	closeErr := root.Close()
	if closeErr != nil && file != nil {
		return nil, errors.Join(closeErr, file.Close())
	}
	return file, errors.Join(openErr, closeErr)
}

func syncNVMeMetadataDirectory(path string) error {
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	directory, openErr := root.Open(".")
	closeRootErr := root.Close()
	if openErr != nil {
		return errors.Join(openErr, closeRootErr)
	}
	return errors.Join(directory.Sync(), directory.Close(), closeRootErr)
}
