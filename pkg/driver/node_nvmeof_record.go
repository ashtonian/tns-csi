package driver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"

	"github.com/fenio/tns-csi/pkg/metrics"
	"github.com/fenio/tns-csi/pkg/tnsapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

const nvmePolicyMarkerSuffix = ".tns-csi-nvmeof-policy"

var errNVMePolicyRecordNotFound = errors.New("NVMe node policy record not found")

type nvmeNodePolicyRecord struct {
	Namespace nvmeNamespaceIdentity `json:"namespace"`
	Target    nvmeStoredPolicy      `json:"target"`
	Version   int                   `json:"version"`
}

func nvmeRecordName(volumeID string) string {
	sum := sha256.Sum256([]byte(volumeID))
	return hex.EncodeToString(sum[:]) + ".json"
}

func readNVMePolicyFile(path string) (data []byte, resultErr error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.Join(errInvalidNVMePolicy, err)
	}
	data, err = io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || len(data) > 16384 {
		return nil, errors.Join(errInvalidNVMePolicy, err)
	}
	return data, nil
}

func (s *NodeService) readNVMeNodeRecord(volumeID, stagingPath string) (*nvmeNodePolicyRecord, error) {
	if stagingPath != "" {
		marker, markerErr := readNVMePolicyFile(stagingPath + nvmePolicyMarkerSuffix)
		if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
			return nil, markerErr
		}
		if markerErr == nil && string(marker) != nvmeRecordName(volumeID) {
			return nil, errInvalidNVMePolicy
		}
	}
	path := filepath.Join(s.nvmePolicyDirectory, nvmeRecordName(volumeID))
	data, err := readNVMePolicyFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if stagingPath != "" {
			if _, markerErr := readNVMePolicyFile(stagingPath + nvmePolicyMarkerSuffix); !errors.Is(markerErr, os.ErrNotExist) {
				return nil, fmt.Errorf("%w: staging policy exists without its durable record", errInvalidNVMePolicy)
			}
		}
		return nil, errNVMePolicyRecordNotFound
	}
	if err != nil {
		return nil, err
	}
	var record nvmeNodePolicyRecord
	if decodeErr := decodeNVMePolicyJSON(data, &record); decodeErr != nil {
		return nil, decodeErr
	}
	encodedTarget, err := json.Marshal(record.Target)
	if err != nil {
		return nil, err
	}
	target, err := parseStoredNVMePolicy(string(encodedTarget))
	if err != nil || record.Version != 1 || target.VolumeID != volumeID || record.Namespace.NQN != target.NQN || record.Namespace.NSID != "1" || !validNVMeDeviceNumber(record.Namespace.Dev) || record.Namespace.Owner == "" {
		return nil, errors.Join(errInvalidNVMePolicy, err)
	}
	return &record, nil
}

func nvmeRecordTarget(volumeID string, params *nvmeOFConnectionParams) nvmeStoredPolicy {
	return nvmeStoredPolicy{
		Version: 1, VolumeID: volumeID, NQN: params.nqn, Server: params.server,
		Transport: params.transport, Port: params.port, Parameters: params.explicitPolicy.parameters(),
	}
}

// A profile is resolved anew on every call. Only explicit PV fields are durable.
func (s *NodeService) prepareNVMePolicy(ctx context.Context, volumeID, devicePath, stagingPath string, params *nvmeOFConnectionParams) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	defer func() {
		// Caller cancellation is not a policy refusal.
		if resultErr == nil || ctx.Err() != nil {
			return
		}
		reason := nvmePolicyRefusalReason(resultErr)
		metrics.RecordNVMePolicyRefusal(reason)
		klog.Warningf("Refusing NVMe policy step for volume %s on %s (NQN %s, reason %s): %v", volumeID, devicePath, params.nqn, reason, resultErr)
	}()
	record, err := s.readNVMeNodeRecord(volumeID, stagingPath)
	if err != nil && !errors.Is(err, errNVMePolicyRecordNotFound) {
		return status.Error(codes.FailedPrecondition, "retained node NVMe policy is unreadable; restore its metadata before use")
	}
	target := nvmeRecordTarget(volumeID, params)
	if record != nil && !reflect.DeepEqual(record.Target, target) {
		return status.Error(codes.FailedPrecondition, "retained node NVMe policy differs from the requested volume or explicit parameters")
	}
	if len(params.addressPins) == 0 {
		return nil
	}
	// Compare persistent namespace identifiers before the first possible queue write.
	if record != nil {
		identity, identityErr := nvmeNamespaceIdentityAt(filepath.Join(sysBlockPath, filepath.Base(devicePath)))
		if identityErr != nil || !sameNVMeStableIdentity(identity, record.Namespace) {
			return errors.Join(ErrNVMeIdentityConflict, identityErr)
		}
	}
	identity, err := s.enforceNVMeQueuePolicy(ctx, devicePath, params)
	if err != nil {
		return err
	}
	if record == nil {
		record = &nvmeNodePolicyRecord{Version: 1, Target: target, Namespace: identity}
		if err := os.MkdirAll(s.nvmePolicyDirectory, 0o700); err != nil {
			return err
		}
		if err := syncNVMeMetadataDirectory(filepath.Dir(s.nvmePolicyDirectory)); err != nil {
			return err
		}
		data, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := writeNVMePolicyFile(ctx, filepath.Join(s.nvmePolicyDirectory, nvmeRecordName(volumeID)), data); err != nil {
			return err
		}
	}
	if stagingPath != "" {
		if err := writeNVMePolicyFile(ctx, stagingPath+nvmePolicyMarkerSuffix, []byte(nvmeRecordName(volumeID))); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// The CO deletes the staging path's parent after unstage, so the marker must not outlive
// the stage. The durable record in the policy directory is retained.
func removeNVMePolicyMarker(stagingPath string) error {
	err := os.Remove(stagingPath + nvmePolicyMarkerSuffix)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// nvmePolicyRefusalReason maps an enforcement error to a bounded metric label.
func nvmePolicyRefusalReason(err error) string {
	switch {
	case errors.Is(err, ErrNVMeIdentityConflict), errors.Is(err, errNVMeAmbiguousNamespace):
		return "identity"
	case errors.Is(err, ErrNVMeDiscoveryUnknown), errors.Is(err, errNVMeNoController):
		return "unknown"
	case errors.Is(err, errInvalidNVMePolicy):
		return "invalid"
	case status.Code(err) == codes.FailedPrecondition:
		// prepareNVMePolicy reports unreadable or conflicting retained records this way.
		return "record"
	default:
		return "other"
	}
}

// Atomic replacement plus directory sync makes a successful stage restart-safe.
func writeNVMePolicyFile(ctx context.Context, path string, data []byte) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	var suffix [16]byte
	if _, randomErr := rand.Read(suffix[:]); randomErr != nil {
		return randomErr
	}
	temporary := ".nvme-policy-" + hex.EncodeToString(suffix[:])
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if removeErr := root.Remove(temporary); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			klog.Warningf("Could not remove temporary NVMe policy metadata: %v", removeErr)
		}
	}()
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if persistenceErr := errors.Join(writeErr, syncErr, closeErr, ctx.Err()); persistenceErr != nil {
		return persistenceErr
	}
	if renameErr := root.Rename(temporary, filepath.Base(path)); renameErr != nil {
		return renameErr
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func (s *NodeService) restoreNVMeNodeParams(ctx context.Context, volumeID, stagingPath string) (*nvmeOFConnectionParams, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	record, err := s.readNVMeNodeRecord(volumeID, stagingPath)
	if err != nil && !errors.Is(err, errNVMePolicyRecordNotFound) {
		return nil, status.Error(codes.FailedPrecondition, "node NVMe policy record cannot be restored")
	}
	if record == nil {
		if s.apiClient != nil {
			properties, readErr := s.apiClient.GetDatasetProperties(ctx, volumeID, []string{tnsapi.PropertyNVMePolicy})
			if readErr != nil {
				return nil, nvmeLifecycleError(ctx, "cannot establish legacy policy absence", readErr)
			}
			if properties[tnsapi.PropertyNVMePolicy] != "" {
				return nil, status.Error(codes.FailedPrecondition, "restage NVMe volume to restore its retained remote policy before expansion")
			}
		}
		if s.nvmeNodePolicy != nil && len(s.nvmeNodePolicy.profiles) > 0 {
			return nil, status.Error(codes.FailedPrecondition, "restage the legacy NVMe volume to bind its endpoint before expansion with a node profile")
		}
		return nil, errNVMePolicyRecordNotFound
	}
	parameters := record.Target.Parameters
	parameters["nqn"] = record.Target.NQN
	parameters["server"] = record.Target.Server
	parameters["transport"] = record.Target.Transport
	parameters["port"] = record.Target.Port
	return s.validateNVMeOFParams(parameters)
}
