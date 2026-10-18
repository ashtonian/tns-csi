package driver

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"k8s.io/klog/v2"
)

var nvmePathNamespaceName = regexp.MustCompile(`^nvme\d+c\d+n\d+$`)

type nvmeNamespaceIdentity struct {
	NQN   string `json:"nqn"`
	NSID  string `json:"nsid"`
	UUID  string `json:"uuid,omitempty"`
	NGUID string `json:"nguid,omitempty"`
	EUI   string `json:"eui,omitempty"`
	Dev   string `json:"dev"`
	Owner string `json:"owner"`
}

type nvmeQueueScope struct {
	identities map[string]nvmeNamespaceIdentity
	endpoints  map[string]string
}

func nvmeNamespaceIdentityAt(path string) (nvmeNamespaceIdentity, error) {
	var identity nvmeNamespaceIdentity
	var err error
	identity.NQN, err = readSysfsValue(filepath.Join(path, "device", "subsysnqn"))
	if err != nil || identity.NQN == "" {
		return identity, errors.Join(ErrNVMeDiscoveryUnknown, err)
	}
	identity.NSID, err = readSysfsValue(filepath.Join(path, "nsid"))
	if err != nil || identity.NSID != "1" {
		return identity, errors.Join(ErrNVMeIdentityConflict, err)
	}
	identity.Dev, err = readSysfsValue(filepath.Join(path, "dev"))
	// Hidden native-multipath path disks have sysfs queues but no dev_t.
	hidden := nvmePathNamespaceName.MatchString(filepath.Base(path))
	if hidden && errors.Is(err, os.ErrNotExist) {
		identity.Dev = ""
	} else if err != nil || !validNVMeDeviceNumber(identity.Dev) {
		return identity, errors.Join(ErrNVMeDiscoveryUnknown, err)
	}
	identity.Owner, err = filepath.EvalSymlinks(filepath.Join(path, "device"))
	if err != nil {
		return identity, err
	}
	for key, value := range map[string]*string{"uuid": &identity.UUID, "nguid": &identity.NGUID, "eui": &identity.EUI} {
		text, readErr := readSysfsValue(filepath.Join(path, key))
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return identity, readErr
		}
		if strings.Trim(strings.ReplaceAll(text, "-", ""), "0") != "" {
			*value = strings.ToLower(text)
		}
	}
	return identity, nil
}

func validNVMeDeviceNumber(value string) bool {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if _, err := strconv.ParseUint(part, 10, 32); err != nil || part == "" {
			return false
		}
	}
	return true
}

func sameNVMeStableIdentity(first, second nvmeNamespaceIdentity) bool {
	return first.NQN == second.NQN && first.NSID == second.NSID &&
		first.UUID == second.UUID && first.NGUID == second.NGUID && first.EUI == second.EUI
}

// All controller paths of a multipath head must satisfy the selected scope.
// The namespace number is never used to infer a controller number.
func nvmeScopedEndpoints(owner string, params *nvmeOFConnectionParams) (map[string]string, error) {
	controllers := []string{owner}
	if !nvmeControllerDirName.MatchString(filepath.Base(owner)) {
		controllers = nil
		entries, err := os.ReadDir(owner)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if nvmeControllerDirName.MatchString(entry.Name()) {
				controllers = append(controllers, filepath.Join(owner, entry.Name()))
			}
		}
	}
	if len(controllers) == 0 {
		return nil, errNVMeNoController
	}
	endpoints := make(map[string]string, len(controllers))
	for _, controller := range controllers {
		nqn, err := readSysfsValue(filepath.Join(controller, "subsysnqn"))
		if err != nil || nqn != params.nqn {
			return nil, errors.Join(ErrNVMeIdentityConflict, err)
		}
		transport, err := readSysfsValue(filepath.Join(controller, "transport"))
		if err != nil || transport != nvmeTransportTCP {
			return nil, errors.Join(ErrNVMeIdentityConflict, err)
		}
		address, err := readSysfsValue(filepath.Join(controller, "address"))
		if err != nil {
			return nil, err
		}
		endpoint, err := nvmeVerifiedEndpoint(address, params)
		if err != nil {
			return nil, err
		}
		endpoints[controller] = endpoint
	}
	return endpoints, nil
}

func nvmeVerifiedEndpoint(address string, params *nvmeOFConnectionParams) (string, error) {
	fields := make(map[string]string)
	for _, field := range strings.Split(address, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok || fields[key] != "" {
			return "", ErrNVMeDiscoveryUnknown
		}
		fields[key] = value
	}
	ip, err := netip.ParseAddr(fields["traddr"])
	if err != nil || !slices.Contains(params.addressPins, ip.Unmap()) {
		return "", ErrNVMeIdentityConflict
	}
	port, err := strconv.ParseUint(fields["trsvcid"], 10, 16)
	if err != nil || strconv.FormatUint(port, 10) != params.port {
		return "", ErrNVMeIdentityConflict
	}
	return netip.AddrPortFrom(ip.Unmap(), uint16(port)).String(), nil
}

func captureNVMeQueueScope(devicePath string, params *nvmeOFConnectionParams) (nvmeQueueScope, error) {
	scope := nvmeQueueScope{identities: make(map[string]nvmeNamespaceIdentity), endpoints: make(map[string]string)}
	name := filepath.Base(devicePath)
	if !nvmeNamespaceName.MatchString(name) {
		return scope, ErrNVMeNotNVMeDevice
	}
	head, err := nvmeNamespaceIdentityAt(filepath.Join(sysBlockPath, name))
	if err != nil || head.NQN != params.nqn {
		return scope, errors.Join(ErrNVMeIdentityConflict, err)
	}
	scope.endpoints, err = nvmeScopedEndpoints(head.Owner, params)
	if err != nil {
		return scope, err
	}
	entries, err := os.ReadDir(sysBlockPath)
	if err != nil {
		return scope, err
	}
	for _, entry := range entries {
		if !nvmeNamespaceName.MatchString(entry.Name()) && !nvmePathNamespaceName.MatchString(entry.Name()) {
			continue
		}
		path := filepath.Join(sysBlockPath, entry.Name())
		nqn, readErr := readSysfsValue(filepath.Join(path, "device", "subsysnqn"))
		if readErr != nil || nqn == "" {
			return scope, errors.Join(ErrNVMeDiscoveryUnknown, readErr)
		}
		if nqn != params.nqn {
			continue
		}
		identity, readErr := nvmeNamespaceIdentityAt(path)
		if readErr != nil || !sameNVMeStableIdentity(head, identity) {
			return scope, errors.Join(ErrNVMeIdentityConflict, readErr)
		}
		endpoints, endpointErr := nvmeScopedEndpoints(identity.Owner, params)
		if endpointErr != nil {
			return scope, endpointErr
		}
		for controller, endpoint := range endpoints {
			scope.endpoints[controller] = endpoint
		}
		scope.identities[path] = identity
	}
	if len(scope.identities) == 0 {
		return scope, ErrNVMeDiscoveryUnknown
	}
	return scope, nil
}

func (s *NodeService) enforceNVMeQueuePolicy(ctx context.Context, devicePath string, params *nvmeOFConnectionParams) (nvmeNamespaceIdentity, error) {
	var identity nvmeNamespaceIdentity
	if len(params.addressPins) == 0 {
		return identity, nil
	}
	scope, err := s.captureNVMeDeviceScope(devicePath, params)
	if err != nil {
		return identity, err
	}
	verify := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := s.captureNVMeDeviceScope(devicePath, params)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, scope) {
			return ErrNVMeIdentityConflict
		}
		return nil
	}
	paths := make([]string, 0, len(scope.identities))
	for path := range scope.identities {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		if err := s.applyNVMeQueueLimits(ctx, path, params.policy, verify); err != nil {
			return identity, err
		}
	}
	if err := verify(); err != nil {
		return identity, err
	}
	return scope.identities[filepath.Join(sysBlockPath, filepath.Base(devicePath))], nil
}

func (s *NodeService) captureNVMeDeviceScope(devicePath string, params *nvmeOFConnectionParams) (nvmeQueueScope, error) {
	scope, err := captureNVMeQueueScope(devicePath, params)
	if err != nil {
		return scope, err
	}
	readNumber := s.nvmeDeviceNumberFn
	if readNumber == nil {
		readNumber = nvmeBlockDeviceNumber
	}
	number, err := readNumber(devicePath)
	if err != nil || number == "" || number != scope.identities[filepath.Join(sysBlockPath, filepath.Base(devicePath))].Dev {
		return scope, errors.Join(ErrNVMeIdentityConflict, err)
	}
	return scope, nil
}

func readNVMeQueueNumber(path, key string) (uint64, error) {
	value, err := readSysfsValue(filepath.Join(path, "queue", key))
	if err != nil {
		return 0, err
	}
	number, err := strconv.ParseUint(value, 10, 63)
	if err != nil {
		return 0, errInvalidNVMePolicy
	}
	return number, nil
}

func nvmeAlignedCap(requested, current, hardware, logicalBytes uint64) (uint64, error) {
	if logicalBytes < 512 || logicalBytes&(logicalBytes-1) != 0 {
		return 0, errInvalidNVMePolicy
	}
	bound := min(requested, current, hardware)
	alignmentKiB := max(uint64(1), logicalBytes/1024)
	capKiB := bound / alignmentKiB * alignmentKiB
	if capKiB == 0 {
		return 0, errInvalidNVMePolicy
	}
	return capKiB, nil
}

func (s *NodeService) applyNVMeQueueLimits(ctx context.Context, path string, policy nvmePolicy, verify func() error) error {
	capKiB, hasCap := policy.values[nvmeMaxSectorsKey]
	readAhead, hasReadAhead := policy.values[nvmeReadAheadKey]
	if !hasCap && !hasReadAhead {
		return verify()
	}
	if hasCap {
		current, err := readNVMeQueueNumber(path, "max_sectors_kb")
		if err != nil {
			return err
		}
		hardware, err := readNVMeQueueNumber(path, "max_hw_sectors_kb")
		if err != nil {
			return err
		}
		logical, err := readNVMeQueueNumber(path, "logical_block_size")
		if err != nil {
			return err
		}
		capKiB, err = nvmeAlignedCap(capKiB, current, hardware, logical)
		if err != nil {
			return err
		}
		if capKiB != current {
			if err := s.writeNVMeQueueValue(ctx, path, "max_sectors_kb", capKiB, verify); err != nil {
				return err
			}
		}
	}
	if hasReadAhead {
		if err := s.writeNVMeQueueValue(ctx, path, "read_ahead_kb", readAhead, verify); err != nil {
			return err
		}
	}
	if err := verify(); err != nil {
		return err
	}
	if hasCap {
		effective, err := verifyNVMeQueueCap(path, capKiB)
		if err != nil {
			return err
		}
		klog.V(4).Infof("NVMe queue cap verified: %d KiB", effective)
	}
	if hasReadAhead {
		effective, err := readNVMeQueueNumber(path, "read_ahead_kb")
		if err != nil || effective != readAhead {
			return errors.Join(ErrNVMeIdentityConflict, err)
		}
	}
	return verify()
}

func verifyNVMeQueueCap(path string, capKiB uint64) (uint64, error) {
	effective, err := readNVMeQueueNumber(path, "max_sectors_kb")
	if err != nil {
		return 0, err
	}
	hardware, err := readNVMeQueueNumber(path, "max_hw_sectors_kb")
	if err != nil {
		return 0, err
	}
	logical, err := readNVMeQueueNumber(path, "logical_block_size")
	if err != nil {
		return 0, err
	}
	aligned, err := nvmeAlignedCap(capKiB, effective, hardware, logical)
	if err != nil || effective != aligned {
		return 0, errors.Join(ErrNVMeIdentityConflict, err)
	}
	return effective, nil
}

func (s *NodeService) writeNVMeQueueValue(ctx context.Context, path, key string, value uint64, verify func() error) (resultErr error) {
	if err := verify(); err != nil {
		return err
	}
	if s.nvmeQueueWriteFn != nil {
		return s.nvmeQueueWriteFn(ctx, path, key, value, verify)
	}
	// A pinned sysfs FD cannot follow a replacement namespace with the same name.
	// Do not truncate before checking identity: test fixtures are regular files.
	root, err := os.OpenRoot(filepath.Dir(sysBlockPath))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	relative, err := filepath.Rel(filepath.Dir(sysBlockPath), filepath.Join(path, "queue", key))
	if err != nil {
		return err
	}
	file, err := root.OpenFile(relative, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	current, err := root.Stat(relative)
	if err != nil || !os.SameFile(opened, current) {
		return errors.Join(ErrNVMeIdentityConflict, err)
	}
	if err := verify(); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(file, "%d\n", value); err != nil {
		return err
	}
	return verify()
}
