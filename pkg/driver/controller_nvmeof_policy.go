package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/fenio/tns-csi/pkg/tnsapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var nvmePolicyKeys = [...]string{
	nvmeReconnectDelayKey, nvmeCtrlLossKey, nvmeKeepAliveKey,
	nvmeMaxSectorsKey, nvmeReadAheadKey, nvmeIOQueuesKey, nvmeQueueSizeKey,
}

func (s *ControllerService) completeCreateVolume(ctx context.Context, req *csi.CreateVolumeRequest, resp *csi.CreateVolumeResponse, checkMode string) (*csi.CreateVolumeResponse, error) {
	applyCreateVolumeContext(req, resp, checkMode)
	if err := s.finalizeNVMePolicy(ctx, req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

type nvmeStoredPolicy struct {
	Parameters map[string]string `json:"parameters"`
	VolumeID   string            `json:"volumeID"`
	NQN        string            `json:"nqn"`
	Server     string            `json:"server"`
	Transport  string            `json:"transport"`
	Port       string            `json:"port"`
	Version    int               `json:"version"`
}

func injectNVMePolicyParams(volumeContext, parameters map[string]string) {
	injectQueueParams(volumeContext, parameters[nvmeIOQueuesKey], parameters[nvmeQueueSizeKey])
	for _, key := range nvmePolicyKeys {
		if value, present := parameters[key]; present && value != "" {
			volumeContext[key] = value
		}
	}
	for _, key := range []string{"transport", "port"} {
		if parameters[key] != "" {
			volumeContext[key] = parameters[key]
		}
	}
}

func parseStoredNVMePolicy(raw string) (nvmeStoredPolicy, error) {
	var stored nvmeStoredPolicy
	if len(raw) > 4096 {
		return stored, errInvalidNVMePolicy
	}
	if err := decodeNVMePolicyJSON([]byte(raw), &stored); err != nil {
		return stored, err
	}
	if stored.Version != 1 || stored.VolumeID == "" || stored.NQN == "" || stored.Server == "" || stored.Transport == "" || stored.Parameters == nil {
		return stored, errInvalidNVMePolicy
	}
	port, err := strconv.ParseUint(stored.Port, 10, 16)
	if err != nil || port == 0 {
		return stored, errInvalidNVMePolicy
	}
	policy, err := parseNVMePolicy(stored.Parameters, false)
	if err != nil {
		return stored, err
	}
	if !reflect.DeepEqual(policy.parameters(), stored.Parameters) {
		return stored, fmt.Errorf("%w: stored parameters are not canonical", errInvalidNVMePolicy)
	}
	return stored, nil
}

// finalizeNVMePolicy runs after presentation creation but before returning success.
// Failed persistence retains resources for idempotent reconciliation, never cleanup.
func (s *ControllerService) finalizeNVMePolicy(ctx context.Context, req *csi.CreateVolumeRequest, resp *csi.CreateVolumeResponse) error {
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	if req.GetParameters()["protocol"] != ProtocolNVMeOF || resp == nil || resp.Volume == nil {
		return nil
	}
	if resp.Volume.VolumeContext == nil {
		resp.Volume.VolumeContext = make(map[string]string)
	}
	injectNVMePolicyParams(resp.Volume.VolumeContext, req.GetParameters())
	policy, err := validateNVMePolicyParameters(req.GetParameters())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	stored := nvmeStoredPolicy{
		Version: 1, VolumeID: resp.Volume.VolumeId, NQN: resp.Volume.VolumeContext["nqn"],
		Server: resp.Volume.VolumeContext["server"], Transport: resp.Volume.VolumeContext["transport"],
		Port: resp.Volume.VolumeContext["port"], Parameters: policy.parameters(),
	}
	if stored.Transport == "" {
		stored.Transport = nvmeTransportTCP
	}
	if stored.Port == "" {
		stored.Port = "4420"
	}
	properties, err := s.apiClient.GetDatasetProperties(ctx, stored.VolumeID, []string{tnsapi.PropertyNVMePolicy})
	if err != nil {
		return nvmePolicyPersistenceError(ctx, "cannot verify retained NVMe policy; resources retained for retry")
	}
	raw := properties[tnsapi.PropertyNVMePolicy]
	if raw != "" {
		previous, parseErr := parseStoredNVMePolicy(raw)
		if parseErr != nil {
			return status.Error(codes.FailedPrecondition, "retained NVMe policy is unreadable or unsupported")
		}
		if previous.VolumeID == stored.VolumeID {
			if !reflect.DeepEqual(previous, stored) {
				return status.Error(codes.AlreadyExists, "volume already has different explicit NVMe policy or endpoint identity")
			}
			return nil
		}
		if req.GetVolumeContentSource() == nil {
			return status.Error(codes.FailedPrecondition, "retained NVMe policy belongs to another volume")
		}
	} else if len(stored.Parameters) == 0 && req.GetVolumeContentSource() == nil {
		return nil
	}
	encoded, err := json.Marshal(stored)
	if err != nil || len(encoded) > 4096 {
		return status.Error(codes.InvalidArgument, "NVMe policy metadata exceeds the supported representation")
	}
	if err := s.apiClient.SetDatasetProperties(ctx, stored.VolumeID, map[string]string{tnsapi.PropertyNVMePolicy: string(encoded)}); err != nil {
		return nvmePolicyPersistenceError(ctx, "cannot persist explicit NVMe policy; resources retained for retry")
	}
	return nil
}

func nvmePolicyPersistenceError(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	return status.Error(codes.Unavailable, message)
}

func restoreNVMeMetadataPolicy(meta *VolumeMetadata, dataset *tnsapi.DatasetWithProperties) error {
	property, present := dataset.UserProperties[tnsapi.PropertyNVMePolicy]
	if !present {
		return nil
	}
	stored, err := parseStoredNVMePolicy(property.Value)
	if err != nil || stored.VolumeID != dataset.ID || stored.NQN != meta.NVMeOFNQN {
		return status.Error(codes.FailedPrecondition, "NVMe policy cannot be bound to the reconstructed volume")
	}
	meta.NVMeOFPolicy = stored.Parameters
	meta.Server = stored.Server
	meta.NVMeOFTransport = stored.Transport
	meta.NVMeOFPort = stored.Port
	return nil
}

func validateAdoptedNVMePolicy(dataset *tnsapi.DatasetWithProperties, parameters map[string]string) error {
	property, present := dataset.UserProperties[tnsapi.PropertyNVMePolicy]
	if !present {
		return nil
	}
	stored, err := parseStoredNVMePolicy(property.Value)
	if err != nil || stored.VolumeID != dataset.ID {
		return status.Error(codes.FailedPrecondition, "adoption has unknown NVMe policy provenance")
	}
	requested, err := parseNVMePolicy(parameters, false)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if !reflect.DeepEqual(stored.Parameters, requested.parameters()) {
		return status.Error(codes.AlreadyExists, "adoption would change or discard retained explicit NVMe policy")
	}
	return nil
}
