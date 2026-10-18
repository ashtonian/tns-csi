package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/klog/v2"

	"github.com/fenio/tns-csi/pkg/cmdexec"
)

// getSubsystemState keeps failed observations distinct from a missing subsystem.
func getSubsystemState(ctx context.Context, nqn string) (string, error) {
	output, err := nvmeListSubsysCmd(ctx)
	if err != nil {
		return "", err
	}
	subsystem, err := findNVMeSubsystem(output, nqn)
	if err != nil {
		return "", err
	}
	if subsystem == nil {
		return "", ErrNVMeDeviceNotFound
	}
	for _, path := range subsystem.Paths {
		if path.State == nvmeSubsystemStateLive {
			return path.State, nil
		}
	}
	if len(subsystem.Paths) == 0 || subsystem.Paths[0].State == "" {
		return "", ErrNVMeDiscoveryUnknown
	}
	return subsystem.Paths[0].State, nil
}

type nvmeListSubsystemsOutput struct {
	Subsystems []nvmeListSubsystem `json:"Subsystems"`
}

type nvmeListSubsystem struct {
	NQN          string         `json:"NQN"`
	SubsystemNQN string         `json:"SubsystemNQN"`
	Paths        []nvmeListPath `json:"Paths"`
}

type nvmeListPath struct {
	Name  string `json:"Name"`
	State string `json:"State"`
}

func (s nvmeListSubsystem) nqn() string {
	if s.SubsystemNQN != "" {
		return s.SubsystemNQN
	}
	return s.NQN
}

var errInvalidNVMeListSubsystemsOutput = errors.New("invalid nvme list-subsys output")

func findNVMeSubsystem(output []byte, nqn string) (*nvmeListSubsystem, error) {
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil, errInvalidNVMeListSubsystemsOutput
	}

	var groups []nvmeListSubsystemsOutput
	switch trimmed[0] {
	case '[':
		// nvme-cli 2.x groups subsystems by host in a top-level array.
		if err := json.Unmarshal([]byte(trimmed), &groups); err != nil {
			return nil, err
		}
	case '{':
		// Older nvme-cli versions return the subsystem collection directly.
		var parsed nvmeListSubsystemsOutput
		if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
			return nil, err
		}
		groups = append(groups, parsed)
	default:
		return nil, fmt.Errorf("%w: unexpected JSON prefix %q", errInvalidNVMeListSubsystemsOutput, trimmed[0])
	}

	for groupIndex := range groups {
		if groups[groupIndex].Subsystems == nil {
			return nil, errInvalidNVMeListSubsystemsOutput
		}
		for subsystemIndex := range groups[groupIndex].Subsystems {
			subsystem := &groups[groupIndex].Subsystems[subsystemIndex]
			if subsystem.nqn() == nqn {
				return subsystem, nil
			}
		}
	}
	return nil, nil //nolint:nilnil // nil means the exact NQN was not found
}

// waitForSubsystemLive polls transport readiness without resetting the subsystem.
func waitForSubsystemLive(ctx context.Context, nqn string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for attempt := 1; attempt <= 30; attempt++ {
		if err := waitCtx.Err(); err != nil {
			return errors.Join(ErrNVMeSubsystemTimeout, err, lastErr)
		}
		state, err := getSubsystemState(waitCtx, nqn)
		if err == nil && state == nvmeSubsystemStateLive {
			return nil
		}
		lastErr = err
		if attempt%5 == 0 {
			triggerUdevForNVMeSubsystem(waitCtx)
		}
		if err := waitNVMeContext(waitCtx, 2*time.Second); err != nil {
			return errors.Join(ErrNVMeSubsystemTimeout, err, lastErr)
		}
	}
	return errors.Join(ErrNVMeSubsystemTimeout, lastErr)
}

// triggerUdevForNVMeSubsystem triggers udev to process new NVMe devices after a connection.
// This helps ensure the kernel and udev properly enumerate newly connected NVMe-oF devices.
func triggerUdevForNVMeSubsystem(ctx context.Context) {
	klog.V(4).Infof("Triggering udev to process new NVMe devices")

	// Trigger udev to process any new NVMe devices
	triggerCtx, triggerCancel := context.WithTimeout(ctx, 5*time.Second)
	defer triggerCancel()
	triggerCmd := cmdexec.CommandContext(triggerCtx, "udevadm", "trigger", "--action=add", "--subsystem-match=nvme")
	if output, err := triggerCmd.CombinedOutput(); err != nil {
		klog.V(4).Infof("udevadm trigger for NVMe subsystem failed: %v, output: %s (continuing anyway)", err, string(output))
	} else {
		klog.V(4).Infof("Triggered udev add events for NVMe subsystem")
	}

	// Also trigger block subsystem in case block devices need processing
	blockTriggerCtx, blockTriggerCancel := context.WithTimeout(ctx, 5*time.Second)
	defer blockTriggerCancel()
	blockTriggerCmd := cmdexec.CommandContext(blockTriggerCtx, "udevadm", "trigger", "--action=add", "--subsystem-match=block")
	if output, err := blockTriggerCmd.CombinedOutput(); err != nil {
		klog.V(4).Infof("udevadm trigger for block subsystem failed: %v, output: %s (continuing anyway)", err, string(output))
	} else {
		klog.V(4).Infof("Triggered udev add events for block subsystem")
	}

	// Wait for udev to settle (process the events)
	settleCtx, settleCancel := context.WithTimeout(ctx, 15*time.Second)
	defer settleCancel()
	settleCmd := cmdexec.CommandContext(settleCtx, "udevadm", "settle", "--timeout=10")
	if output, err := settleCmd.CombinedOutput(); err != nil {
		klog.V(4).Infof("udevadm settle failed: %v, output: %s (continuing anyway)", err, string(output))
	} else {
		klog.V(4).Infof("udevadm settle completed after NVMe connection")
	}
}

// findNVMeDeviceByNQN finds the device path for a given NQN.
// With independent subsystems, NSID is always 1, so we just need to find the controller
// and return the n1 device.
func (s *NodeService) findNVMeDeviceByNQN(ctx context.Context, nqn string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if nqn == "" {
		return "", ErrNVMeEmptyNQN
	}
	output, listErr := s.runNVMeListSubsys(ctx)
	if listErr == nil {
		subsystem, parseErr := findNVMeSubsystem(output, nqn)
		if parseErr == nil && subsystem != nil {
			devicePath, err := nvmeNamespaceDeviceForNQN(sysBlockPath, nqn)
			if errors.Is(err, ErrNVMeDeviceNotFound) {
				return "", fmt.Errorf("%w: subsystem %s has no visible namespace", ErrNVMeDeviceUnhealthy, nqn)
			}
			return devicePath, err
		}
	}
	return s.findNVMeDeviceByNQNFromSys(ctx, nqn)
}

// nvmeListSubsysCmd runs "nvme list-subsys -o json". Tests replace it.
var nvmeListSubsysCmd = func(ctx context.Context) ([]byte, error) {
	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return cmdexec.CommandContext(listCtx, "nvme", "list-subsys", "-o", "json").CombinedOutput()
}

// runNVMeListSubsys executes nvme list-subsys and returns the output.
func (s *NodeService) runNVMeListSubsys(ctx context.Context) ([]byte, error) {
	return nvmeListSubsysCmd(ctx)
}

// parseNVMeListSubsysOutputForNQN parses nvme list-subsys JSON output to find device path.
// With independent subsystems, NSID is always 1.
func (s *NodeService) parseNVMeListSubsysOutputForNQN(output []byte, nqn string) string {
	subsystem, err := findNVMeSubsystem(output, nqn)
	if err != nil || subsystem == nil {
		return ""
	}
	// The subsystem is connected; resolve its namespace device from sysfs rather than
	// assuming /dev/<controller>n1, which is wrong under native NVMe multipath.
	devicePath, err := nvmeNamespaceDeviceForNQN(sysBlockPath, nqn)
	if err != nil {
		klog.V(4).Infof("Subsystem %s is listed but its namespace device is not resolvable yet: %v", nqn, err)
		return ""
	}
	klog.V(4).Infof("Found NVMe device from list-subsys + sysfs: %s (NQN: %s)", devicePath, nqn)
	return devicePath
}

// findNVMeDeviceByNQNFromSys finds NVMe device by checking /sys/class/nvme.
// With independent subsystems, NSID is always 1.
func (s *NodeService) findNVMeDeviceByNQNFromSys(ctx context.Context, nqn string) (string, error) {
	device, _, err := s.findNVMeDeviceFromSys(ctx, nqn)
	return device, err
}

func (s *NodeService) findNVMeDeviceFromSys(ctx context.Context, nqn string) (devicePath, controllerName string, resultErr error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	entries, err := os.ReadDir(sysClassNVMePath)
	if err != nil {
		return "", "", fmt.Errorf("%w: controller registry: %w", ErrNVMeDiscoveryUnknown, err)
	}
	controller := ""
	for _, entry := range entries {
		if !isNVMeControllerName(entry.Name()) {
			continue
		}
		identity, readErr := readNVMeControllerNQN(sysClassNVMePath, entry.Name())
		if readErr != nil {
			return "", "", readErr
		}
		if identity == nqn && controller == "" {
			controller = entry.Name()
		}
	}
	devicePath, err = nvmeNamespaceDeviceForNQN(sysBlockPath, nqn)
	if err != nil && !errors.Is(err, ErrNVMeDeviceNotFound) {
		return "", controller, err
	}
	if controller == "" {
		if devicePath != "" {
			return "", "", fmt.Errorf("%w: namespace exists without a readable matching controller", ErrNVMeDiscoveryUnknown)
		}
		return "", "", fmt.Errorf("%w for NQN: %s", ErrNVMeDeviceNotFound, nqn)
	}
	if devicePath == "" {
		return "", controller, fmt.Errorf("%w: no namespace for NQN %s", ErrNVMeDeviceUnhealthy, nqn)
	}
	return devicePath, controller, nil
}

// forceNamespaceRescan forces the kernel to rescan namespaces on an NVMe controller.
// This is a lightweight version that just does ns-rescan without full udev processing.
func (s *NodeService) forceNamespaceRescan(ctx context.Context, controllerPath string) {
	nvmeNSRescanCmd(ctx, controllerPath)
}

// nvmeNSRescanCmd runs "nvme ns-rescan" on a controller, best effort. Tests replace it.
var nvmeNSRescanCmd = func(ctx context.Context, controllerPath string) {
	rescanCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	klog.V(4).Infof("Forcing namespace rescan on controller %s", controllerPath)

	cmd := cmdexec.CommandContext(rescanCtx, "nvme", "ns-rescan", controllerPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		klog.V(4).Infof("nvme ns-rescan failed for %s: %v, output: %s (continuing anyway)", controllerPath, err, string(output))
	} else {
		klog.V(4).Infof("nvme ns-rescan completed for %s", controllerPath)
	}
	// Note: Don't call triggerUdevForNVMeSubsystem here - it's too slow (10s+ settle)
	// udev trigger is done periodically in waitForNVMeDevice instead
}

// waitForNVMeDevice waits for the NVMe device to appear after connection.
// With independent subsystems, NSID is always 1.
// Note: This should be called AFTER waitForSubsystemLive() has confirmed the subsystem is "live".
func (s *NodeService) waitForNVMeDevice(ctx context.Context, nqn string, timeout time.Duration) (string, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	lastController := ""
	var lastErr error
	for attempt := 1; ; attempt++ {
		if err := waitCtx.Err(); err != nil {
			return "", errors.Join(ErrNVMeDeviceTimeout, err, lastErr)
		}
		devicePath, controller, err := s.findNVMeDeviceByNQNWithController(waitCtx, nqn)
		lastErr = err
		if err != nil && !errors.Is(err, ErrNVMeDeviceNotFound) && !errors.Is(err, ErrNVMeDeviceUnhealthy) {
			return "", err
		}
		if err == nil && devicePath != "" {
			if _, statErr := os.Stat(devicePath); statErr == nil {
				size, sizeErr := s.nvmeOperations().readSize(waitCtx, devicePath)
				if sizeErr == nil && size > 0 {
					return devicePath, nil
				}
				lastErr = sizeErr
			} else {
				lastErr = statErr
			}
		}
		if controller != "" && (controller != lastController || attempt%5 == 0) {
			s.forceNamespaceRescan(waitCtx, "/dev/"+controller)
			lastController = controller
		}
		if attempt%10 == 0 {
			s.logNVMeDiscoveryDiagnostics(waitCtx, nqn)
			triggerUdevForNVMeSubsystem(waitCtx)
		}
		if err := waitNVMeContext(waitCtx, 2*time.Second); err != nil {
			return "", errors.Join(ErrNVMeDeviceTimeout, err, lastErr)
		}
	}
}

// findNVMeDeviceByNQNWithController finds NVMe device and returns both device path and controller name.
func (s *NodeService) findNVMeDeviceByNQNWithController(ctx context.Context, nqn string) (devicePath, controllerName string, err error) {
	// Use nvme list-subsys which shows NQN and controller mapping
	subsysOutput, listErr := s.runNVMeListSubsys(ctx)
	if listErr != nil {
		klog.V(4).Infof("nvme list-subsys failed: %v, falling back to sysfs", listErr)
		return s.findNVMeDeviceFromSys(ctx, nqn)
	}

	// Parse the output to find controller name for this NQN
	controllerName = s.findControllerForNQN(string(subsysOutput), nqn)
	if controllerName != "" {
		// Namespace device from sysfs, not /dev/<controller>n1 (wrong under multipath).
		if devicePath, err = nvmeNamespaceDeviceForNQN(sysBlockPath, nqn); err == nil {
			return devicePath, controllerName, nil
		}
		// The controller is known; the caller decides when to rescan it. Falling through to
		// the sysfs scan here would rescan on every poll and drop the controller name.
		klog.V(4).Infof("Controller %s found for %s but namespace device not resolvable yet: %v", controllerName, nqn, err)
		return "", controllerName, err
	}

	// Fall back to sysfs
	return s.findNVMeDeviceFromSys(ctx, nqn)
}

// findControllerForNQN parses nvme list-subsys output to find the controller name for a given NQN.
func (s *NodeService) findControllerForNQN(output, nqn string) string {
	subsystem, err := findNVMeSubsystem([]byte(output), nqn)
	if err != nil || subsystem == nil {
		return ""
	}
	for _, path := range subsystem.Paths {
		if isNVMeControllerName(path.Name) {
			return path.Name
		}
	}
	return ""
}

// logNVMeDiscoveryDiagnostics logs diagnostic information to help debug device discovery issues.
func (s *NodeService) logNVMeDiscoveryDiagnostics(ctx context.Context, nqn string) {
	klog.V(2).Infof("=== NVMe Device Discovery Diagnostics for NQN: %s ===", nqn)

	// Run nvme list-subsys
	subsysCtx, subsysCancel := context.WithTimeout(ctx, 5*time.Second)
	defer subsysCancel()
	subsysCmd := cmdexec.CommandContext(subsysCtx, "nvme", "list-subsys")
	if output, err := subsysCmd.CombinedOutput(); err == nil {
		klog.V(2).Infof("nvme list-subsys output:\n%s", string(output))
	} else {
		klog.V(2).Infof("nvme list-subsys failed: %v", err)
	}

	// Run nvme list to show actual namespace devices
	listCtx, listCancel := context.WithTimeout(ctx, 5*time.Second)
	defer listCancel()
	listCmd := cmdexec.CommandContext(listCtx, "nvme", "list")
	if output, err := listCmd.CombinedOutput(); err == nil {
		klog.V(2).Infof("nvme list output:\n%s", string(output))
	} else {
		klog.V(2).Infof("nvme list failed: %v", err)
	}

	// List /sys/class/nvme contents and their NQNs
	if entries, err := os.ReadDir("/sys/class/nvme"); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		klog.V(2).Infof("/sys/class/nvme contents: %v", names)

		// Read subsysnqn for each controller
		nvmeSysDir := "/sys/class/nvme"
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), "nvme") || strings.Contains(e.Name(), "-") {
				continue
			}
			if len(e.Name()) > 4 && strings.Contains(e.Name()[4:], "n") {
				continue // Skip namespace entries
			}
			nqnPath := nvmeSysDir + "/" + e.Name() + "/subsysnqn"
			//nolint:gosec // Reading NVMe subsystem info from standard sysfs path for diagnostics
			if data, readErr := os.ReadFile(nqnPath); readErr == nil {
				klog.V(2).Infof("  %s/subsysnqn = %q", e.Name(), strings.TrimSpace(string(data)))
			} else {
				klog.V(2).Infof("  %s/subsysnqn: error reading: %v", e.Name(), readErr)
			}
		}
	}

	// List /dev/nvme* devices. (This used to exec "ls /dev/nvme*" without a shell, so the
	// glob was never expanded and the diagnostic always failed.)
	if devices, err := filepath.Glob("/dev/nvme*"); err == nil {
		klog.V(2).Infof("/dev/nvme* devices: %v", devices)
	}

	klog.V(2).Infof("=== End NVMe Diagnostics ===")
}
