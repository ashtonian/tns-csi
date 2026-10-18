package driver

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/fenio/tns-csi/pkg/mount"
	"github.com/fenio/tns-csi/pkg/volumeprobe"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Raw-device lookup must not depend on metadata for unrelated namespaces.
var sysDevBlockHealthPath = "/sys/dev/block"

func (s *NodeService) healthMount(ctx context.Context, path string) (mount.Entry, bool, error) {
	if s.healthMountFn != nil {
		return s.healthMountFn(ctx, path)
	}
	return mount.MountedEntryLexical(ctx, path)
}

func (s *NodeService) runHealthProbe(ctx context.Context, volumeID string, request volumeprobe.Request) (volumeprobe.Result, error) {
	if s.healthProbeFn != nil {
		return s.healthProbeFn(ctx, volumeID, request)
	}
	runner := volumeprobe.Runner{Directory: s.healthProbeDirectory}
	return runner.Probe(ctx, volumeID, request)
}

// All application-path I/O, including staging sidecars, stays inside the helper.
// The parent reads only its mount table, local policy directory and device metadata.
func (s *NodeService) probeMountedVolume(ctx context.Context, volumeID, path, stagingPath, expectedNQN string) (volumeprobe.Result, error) {
	if err := ctx.Err(); err != nil {
		return volumeprobe.Result{}, status.FromContextError(err).Err()
	}
	if !filepath.IsAbs(path) || (stagingPath != "" && !filepath.IsAbs(stagingPath)) {
		return volumeprobe.Result{}, status.Error(codes.InvalidArgument, "volume paths must be absolute")
	}
	lookup, err := s.lookupHealthMount(ctx, volumeID, path)
	if status.Code(err) == codes.NotFound {
		return volumeprobe.Result{Code: volumeprobe.Missing}, nil
	}
	if err != nil {
		return volumeprobe.Result{}, err
	}
	entry := lookup.entry
	snapshot, err := s.healthSnapshot(volumeID, entry, expectedNQN)
	if err != nil {
		return volumeprobe.Result{}, nvmeLifecycleError(ctx, "cannot establish NVMe health identity", err)
	}
	request := volumeprobe.Request{Path: path, MountID: entry.MountID, Device: entry.Device, Filesystem: entry.FSType, CheckSCSI: snapshot.source == "" && strings.HasPrefix(entry.Source, "/dev/")}
	metadataStage := stagingPath
	if metadataStage == "" && (snapshot.source != "" || lookup.raw) {
		metadataStage = path
	}
	if metadataStage != "" {
		request.MetadataPath = nvmeStagingMetadataPath(metadataStage)
		request.MarkerPath = metadataStage + nvmePolicyMarkerSuffix
	}
	result, err := s.runHealthProbe(ctx, volumeID, request)
	if ctx.Err() != nil {
		return volumeprobe.Result{}, status.FromContextError(ctx.Err()).Err()
	}
	if err != nil {
		return volumeprobe.Result{}, status.Errorf(codes.Unavailable, "filesystem observation could not complete: %v", err)
	}
	if err := s.completeHealthObservation(ctx, volumeID, lookup.path, snapshot, result); err != nil {
		return volumeprobe.Result{}, err
	}
	return result, nil
}

type healthMountLookup struct {
	path  string
	entry mount.Entry
	raw   bool
}

func (s *NodeService) lookupHealthMount(ctx context.Context, volumeID, path string) (healthMountLookup, error) {
	lookup := healthMountLookup{path: path}
	entry, mounted, err := s.healthMount(ctx, path)
	if err != nil {
		return lookup, nvmeLifecycleError(ctx, "cannot read health mount identity", err)
	}
	if !mounted {
		resolved, resolveErr := s.resolveHealthMount(ctx, volumeID, path)
		if resolveErr != nil {
			return lookup, resolveErr
		}
		lookup.path, lookup.raw = resolved.Path, resolved.Raw
		entry, mounted, err = s.healthMount(ctx, lookup.path)
		if err != nil || !mounted || !sameResolvedMount(entry, resolved) {
			return lookup, status.Error(codes.Unavailable, "resolved mount identity changed")
		}
	}
	if entry.MountID <= 0 || !validNVMeDeviceNumber(entry.Device) || entry.FSType == "" || entry.Source == "" {
		return lookup, status.Error(codes.Unavailable, "mount identity is incomplete")
	}
	lookup.entry = entry
	return lookup, nil
}

func sameResolvedMount(entry mount.Entry, resolved *volumeprobe.ResolvedMount) bool {
	return entry.MountID == resolved.MountID && entry.Source == resolved.Source && entry.Device == resolved.Device && entry.FSType == resolved.Filesystem
}

func (s *NodeService) resolveHealthMount(ctx context.Context, volumeID, path string) (*volumeprobe.ResolvedMount, error) {
	result, err := s.runHealthProbe(ctx, volumeID, volumeprobe.Request{Path: path, ResolveMount: true})
	if ctx.Err() != nil {
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "mount path resolution could not complete: %v", err)
	}
	if result.Code == volumeprobe.Missing {
		return nil, status.Error(codes.NotFound, "volume path is missing or not mounted")
	}
	if result.Code != volumeprobe.OK || result.Mount == nil || !filepath.IsAbs(result.Mount.Path) {
		return nil, status.Error(codes.Unavailable, "mount path could not be resolved")
	}
	return result.Mount, nil
}

type healthMountSnapshot struct {
	record   *nvmeNodePolicyRecord
	source   string
	nqn      string
	identity nvmeNamespaceIdentity
	entry    mount.Entry
}

func (s *NodeService) healthSnapshot(volumeID string, entry mount.Entry, expectedNQN string) (healthMountSnapshot, error) {
	snapshot := healthMountSnapshot{entry: entry, nqn: expectedNQN}
	record, err := s.readNVMeNodeRecord(volumeID, "")
	if err != nil && !errors.Is(err, errNVMePolicyRecordNotFound) {
		return snapshot, status.Error(codes.FailedPrecondition, "retained NVMe identity is unreadable")
	}
	if record != nil {
		if expectedNQN != "" && expectedNQN != record.Target.NQN {
			return snapshot, status.Error(codes.FailedPrecondition, "retained NVMe identity conflicts with staging request")
		}
		snapshot.nqn = record.Target.NQN
	}
	snapshot.record = record
	snapshot.source, err = healthNVMeSource(entry.Source)
	if err != nil {
		return snapshot, err
	}
	snapshot.identity, err = s.healthNVMeIdentity(snapshot.source, entry.Device, snapshot.nqn, record)
	return snapshot, err
}

func (s *NodeService) completeHealthObservation(ctx context.Context, volumeID, path string, snapshot healthMountSnapshot, result volumeprobe.Result) error {
	after, mounted, err := s.healthMount(ctx, path)
	if err != nil || !mounted || after != snapshot.entry {
		return status.Error(codes.Unavailable, "mount identity changed during filesystem observation")
	}
	record, recordErr := s.readNVMeNodeRecord(volumeID, "")
	if (recordErr != nil && !errors.Is(recordErr, errNVMePolicyRecordNotFound)) || !reflect.DeepEqual(record, snapshot.record) {
		return status.Error(codes.Unavailable, "retained volume identity changed during filesystem observation")
	}
	if result.PolicyRecord != "" && (record == nil || result.PolicyRecord != nvmeRecordName(volumeID)) {
		return status.Error(codes.FailedPrecondition, "staging policy cannot be bound to its durable record")
	}
	source, number, nqn := snapshot.source, snapshot.entry.Device, snapshot.nqn
	if result.RawDevice != "" {
		source, err = nvmeDeviceForHealthNumber(result.RawDevice)
		if err != nil {
			return nvmeLifecycleError(ctx, "cannot identify raw health source", err)
		}
		number = result.RawDevice
	}
	if result.StagingNQN != "" {
		if nqn != "" && nqn != result.StagingNQN {
			return status.Error(codes.FailedPrecondition, "staging NQN conflicts with retained volume identity")
		}
		nqn = result.StagingNQN
	}
	current, err := s.healthNVMeIdentity(source, number, nqn, record)
	if err != nil {
		return nvmeLifecycleError(ctx, "NVMe identity or state changed during filesystem observation", err)
	}
	if snapshot.identity.NQN != "" && snapshot.identity != current {
		return status.Error(codes.Unavailable, "namespace identity changed during filesystem observation")
	}
	if (source != "" && nqn == "") || (nqn != "" && source == "") {
		return status.Error(codes.Unavailable, "NVMe health needs retained volume identity and a matching source")
	}
	return nil
}

func healthNVMeSource(source string) (string, error) {
	if strings.HasPrefix(filepath.Base(source), "nvme") {
		return source, nil
	}
	if strings.HasPrefix(source, "/dev/") {
		resolved, err := filepath.EvalSymlinks(source)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(filepath.Base(resolved), "nvme") {
			return resolved, nil
		}
	}
	return "", nil
}

func nvmeDeviceForHealthNumber(number string) (string, error) {
	if !validNVMeDeviceNumber(number) {
		return "", ErrNVMeIdentityConflict
	}
	device, err := filepath.EvalSymlinks(filepath.Join(sysDevBlockHealthPath, number))
	if err != nil {
		return "", err
	}
	actual, err := readSysfsValue(filepath.Join(device, "dev"))
	if err != nil || actual != number {
		return "", errors.Join(ErrNVMeIdentityConflict, err)
	}
	name := filepath.Base(device)
	if !nvmeNamespaceName.MatchString(name) {
		return "", nil
	}
	return "/dev/" + name, nil
}

func (s *NodeService) healthNVMeIdentity(source, number, nqn string, record *nvmeNodePolicyRecord) (nvmeNamespaceIdentity, error) {
	if source == "" {
		return nvmeNamespaceIdentity{}, nil
	}
	identity, err := nvmeNamespaceIdentityAt(filepath.Join(sysBlockPath, filepath.Base(source)))
	if err != nil {
		return identity, err
	}
	if identity.Dev != number || (nqn != "" && identity.NQN != nqn) || (record != nil && !sameNVMeStableIdentity(identity, record.Namespace)) {
		return identity, ErrNVMeIdentityConflict
	}
	readNumber := s.nvmeDeviceNumberFn
	if readNumber == nil {
		readNumber = nvmeBlockDeviceNumber
	}
	actual, err := readNumber(source)
	if err != nil || actual != number {
		return identity, errors.Join(ErrNVMeIdentityConflict, err)
	}
	if err := s.healthNVMeController(source, identity, record); err != nil {
		return identity, err
	}
	return identity, nil
}

func (s *NodeService) healthNVMeController(source string, identity nvmeNamespaceIdentity, record *nvmeNodePolicyRecord) error {
	controller, state, err := nvmeControllerForNamespace(sysBlockPath, source)
	if err != nil || state != nvmeSubsystemStateLive {
		return errors.Join(ErrNVMeDiscoveryUnknown, err)
	}
	path := identity.Owner
	if !nvmeControllerDirName.MatchString(filepath.Base(path)) {
		path = filepath.Join(path, filepath.Base(controller))
	}
	nqn, err := readSysfsValue(filepath.Join(path, "subsysnqn"))
	if err != nil || nqn != identity.NQN {
		return errors.Join(ErrNVMeIdentityConflict, err)
	}
	if record != nil {
		parameters := make(map[string]string, len(record.Target.Parameters)+4)
		for key, value := range record.Target.Parameters {
			parameters[key] = value
		}
		parameters["nqn"], parameters["server"], parameters["transport"], parameters["port"] = record.Target.NQN, record.Target.Server, record.Target.Transport, record.Target.Port
		params, parameterErr := s.validateNVMeOFParams(parameters)
		if parameterErr != nil {
			return parameterErr
		}
		if len(params.addressPins) > 0 {
			if _, endpointErr := nvmeScopedEndpoints(identity.Owner, params); endpointErr != nil {
				return endpointErr
			}
		}
	}
	return nil
}

func (s *NodeService) mountedVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	result, err := s.probeMountedVolume(ctx, req.GetVolumeId(), req.GetVolumePath(), req.GetStagingTargetPath(), "")
	if err != nil {
		return nil, err
	}
	if result.Code == volumeprobe.Missing {
		return nil, status.Error(codes.NotFound, "volume is not mounted at the requested path")
	}
	if result.Code != volumeprobe.OK {
		return nil, status.Errorf(codes.Unavailable, "volume statistics unavailable: %s", result.Code)
	}
	if result.FreeBytes > result.TotalBytes || result.AvailableBytes > result.TotalBytes || result.FreeInodes > result.TotalInodes {
		return nil, status.Error(codes.Unavailable, "filesystem returned inconsistent counters")
	}
	usage := &csi.VolumeUsage{Unit: csi.VolumeUsage_BYTES, Total: safeUint64ToInt64(result.TotalBytes)}
	response := &csi.NodeGetVolumeStatsResponse{Usage: []*csi.VolumeUsage{usage}}
	if result.RawDevice == "" {
		usage.Used = safeUint64ToInt64(result.TotalBytes - result.FreeBytes)
		usage.Available = safeUint64ToInt64(result.AvailableBytes)
		response.Usage = append(response.Usage, &csi.VolumeUsage{Unit: csi.VolumeUsage_INODES, Total: safeUint64ToInt64(result.TotalInodes), Used: safeUint64ToInt64(result.TotalInodes - result.FreeInodes), Available: safeUint64ToInt64(result.FreeInodes)})
	}
	return response, nil
}

func (s *NodeService) checkMountedNVMeStage(ctx context.Context, volumeID, path, nqn string, capability *csi.VolumeCapability) (bool, error) {
	lookup, err := s.lookupHealthMount(ctx, volumeID, path)
	if status.Code(err) == codes.NotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if expected := capability.GetMount().GetFsType(); expected != "" && expected != lookup.entry.FSType {
		return true, status.Error(codes.FailedPrecondition, "mounted filesystem type conflicts with the staging request")
	}
	return true, s.verifyMountedNVMeHealth(ctx, volumeID, path, nqn)
}

func (s *NodeService) verifyMountedNVMeHealth(ctx context.Context, volumeID, path, nqn string) error {
	result, err := s.probeMountedVolume(ctx, volumeID, path, path, nqn)
	if err != nil {
		return err
	}
	if result.Code != volumeprobe.OK || result.RootSick != 0 {
		return status.Error(codes.Unavailable, "already-mounted filesystem liveness could not be established; retain its current consumers for operator investigation")
	}
	return nil
}
