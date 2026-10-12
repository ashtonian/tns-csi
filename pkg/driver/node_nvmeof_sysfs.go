package driver

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"k8s.io/klog/v2"
)

// sysBlockPath is the sysfs directory listing block devices. Tests point it elsewhere.
var sysBlockPath = "/sys/block"

// errNVMeAmbiguousNamespace means several namespace devices match one NQN and none is NSID 1.
var errNVMeAmbiguousNamespace = errors.New("ambiguous NVMe namespace devices for NQN")

// nvmeNamespaceName matches visible namespace block devices (nvme0n1). It excludes
// partitions (nvme0n1p1) and the hidden per-path devices of native multipath (nvme0c1n1).
var nvmeNamespaceName = regexp.MustCompile(`^nvme\d+n\d+$`)

// nvmeNamespacePartition matches a partition of a namespace device (nvme0n1p1).
var nvmeNamespacePartition = regexp.MustCompile(`^(nvme\d+n\d+)p\d+$`)

// nvmeControllerDirName matches controller entries (nvme0) in sysfs.
var nvmeControllerDirName = regexp.MustCompile(`^nvme\d+$`)

// errNVMeNoController means a namespace's sysfs owner exposes no controller.
var errNVMeNoController = errors.New("no NVMe controller for namespace")

// nvmeNamespaceDeviceForNQN returns the namespace block device (/dev/nvmeXnY) that belongs
// to the subsystem with the given NQN.
//
// Each namespace device's "device" link points at its owner: the controller without
// multipath, or the nvme-subsysN subsystem with native multipath. Both expose subsysnqn.
// Reading it is the only reliable mapping: with multipath the block device is named
// after the subsystem instance, so "/dev/<controller>n1" can be another volume's disk.
// Volumes use independent subsystems with NSID 1; if several namespaces match, NSID 1
// wins and anything else ambiguous is an error rather than a guess.
func nvmeNamespaceDeviceForNQN(sysBlock, nqn string) (string, error) {
	entries, err := os.ReadDir(sysBlock)
	if err != nil {
		return "", fmt.Errorf("failed to read %s: %w", sysBlock, err)
	}
	var matches []string
	for _, e := range entries {
		name := e.Name()
		if !nvmeNamespaceName.MatchString(name) {
			continue
		}
		owner, readErr := readSysfsValue(filepath.Join(sysBlock, name, "device", "subsysnqn"))
		if readErr != nil || owner != nqn {
			continue
		}
		matches = append(matches, name)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w for NQN: %s", ErrNVMeDeviceNotFound, nqn)
	case 1:
		return "/dev/" + matches[0], nil
	}
	for _, name := range matches {
		if nsid, readErr := readSysfsValue(filepath.Join(sysBlock, name, "nsid")); readErr == nil && nsid == "1" {
			return "/dev/" + name, nil
		}
	}
	return "", fmt.Errorf("%w %s: %v", errNVMeAmbiguousNamespace, nqn, matches)
}

// nvmeNQNForDevice returns the subsystem NQN that owns an NVMe namespace block device.
func nvmeNQNForDevice(sysBlock, devicePath string) (string, error) {
	name := filepath.Base(devicePath)
	if !nvmeNamespaceName.MatchString(name) {
		return "", fmt.Errorf("%s: %w", devicePath, ErrNVMeNotNVMeDevice)
	}
	p := filepath.Join(sysBlock, name, "device", "subsysnqn")
	nqn, err := readSysfsValue(p)
	if err != nil {
		return "", fmt.Errorf("failed to read NQN from %s: %w", p, err)
	}
	if nqn == "" {
		return "", fmt.Errorf("%s: %w", p, ErrNVMeEmptyNQN)
	}
	return nqn, nil
}

// nvmeNamespaceDeviceOrEmpty resolves nqn's namespace device, or "" if it cannot be
// resolved yet (the caller polls, rescans, or reports not-found).
func nvmeNamespaceDeviceOrEmpty(nqn string) string {
	devicePath, err := nvmeNamespaceDeviceForNQN(sysBlockPath, nqn)
	if err != nil {
		klog.V(5).Infof("NVMe namespace device for %s not resolvable yet: %v", nqn, err)
		return ""
	}
	return devicePath
}

func readSysfsValue(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // fixed sysfs layout under a constant root
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// nvmeControllerForNamespace returns the controller (/dev/nvmeN) serving an NVMe namespace
// device, and that controller's state ("live", "connecting", ...).
//
// The controller cannot be derived from the device name: with native multipath,
// /dev/nvme1n1 is named after subsystem instance 1, and "nvme1" may be another volume's
// controller. The namespace's "device" link points at its owner instead: a controller
// without multipath, or the nvme-subsysN subsystem with multipath, whose directory lists
// its controllers (paths). Of those, a live one is preferred, then the lowest-numbered.
func nvmeControllerForNamespace(sysBlock, devicePath string) (controller, state string, err error) {
	name := filepath.Base(devicePath)
	if m := nvmeNamespacePartition.FindStringSubmatch(name); m != nil {
		name = m[1]
	}
	if !nvmeNamespaceName.MatchString(name) {
		return "", "", fmt.Errorf("%s: %w", devicePath, ErrNVMeNotNVMeDevice)
	}
	owner, err := filepath.EvalSymlinks(filepath.Join(sysBlock, name, "device"))
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve sysfs owner of %s: %w", devicePath, err)
	}

	ownerName := filepath.Base(owner)
	if nvmeControllerDirName.MatchString(ownerName) {
		ctrlState, readErr := readSysfsValue(filepath.Join(owner, "state"))
		if readErr != nil {
			return "", "", fmt.Errorf("failed to read state of %s: %w", ownerName, readErr)
		}
		return "/dev/" + ownerName, ctrlState, nil
	}

	entries, err := os.ReadDir(owner)
	if err != nil {
		return "", "", fmt.Errorf("failed to list controllers of %s: %w", ownerName, err)
	}
	var ctrls []string
	for _, e := range entries {
		if nvmeControllerDirName.MatchString(e.Name()) {
			ctrls = append(ctrls, e.Name())
		}
	}
	if len(ctrls) == 0 {
		return "", "", fmt.Errorf("%w: %s (owner %s)", errNVMeNoController, devicePath, ownerName)
	}
	sort.Slice(ctrls, func(i, j int) bool { return nvmeIndex(ctrls[i]) < nvmeIndex(ctrls[j]) })
	for _, c := range ctrls {
		if st, readErr := readSysfsValue(filepath.Join(owner, c, "state")); readErr == nil && st == "live" {
			return "/dev/" + c, st, nil
		}
	}
	// No live path: report the first controller with its own state (empty if unreadable).
	firstState, err := readSysfsValue(filepath.Join(owner, ctrls[0], "state"))
	if err != nil {
		firstState = ""
	}
	return "/dev/" + ctrls[0], firstState, nil
}

// nvmeIndex returns N for a controller name "nvmeN" (names come from nvmeControllerDirName).
func nvmeIndex(name string) int {
	n := 0
	for _, c := range strings.TrimPrefix(name, "nvme") {
		n = n*10 + int(c-'0')
	}
	return n
}
