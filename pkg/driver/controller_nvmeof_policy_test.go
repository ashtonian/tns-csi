package driver

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/fenio/tns-csi/pkg/tnsapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNVMeControllerPolicyRoundTripAndRetry(t *testing.T) {
	properties := make(map[string]string)
	mock := &MockAPIClientForSnapshots{}
	mock.GetDatasetPropertiesFunc = func(context.Context, string, []string) (map[string]string, error) { return properties, nil }
	writes := 0
	mock.SetDatasetPropertiesFunc = func(_ context.Context, _ string, values map[string]string) error {
		writes++
		for key, value := range values {
			properties[key] = value
		}
		return nil
	}
	s := NewControllerService(mock, nil, "")
	parameters := map[string]string{"protocol": ProtocolNVMeOF, nvmeMaxSectorsKey: "1024", nvmeReadAheadKey: "0", nvmeCtrlLossKey: "1800", nvmeIOQueuesKey: "0x8", "port": "4421", "transport": "tcp"}
	req := &csi.CreateVolumeRequest{Parameters: parameters}
	response := func() *csi.CreateVolumeResponse {
		return &csi.CreateVolumeResponse{Volume: &csi.Volume{VolumeId: "tank/csi/volume", VolumeContext: map[string]string{"nqn": nqnA, "server": "192.0.2.10", "nsid": "1", VolumeContextKeyClonedFromSnap: "true"}}}
	}
	for range 2 {
		resp := response()
		if err := s.finalizeNVMePolicy(context.Background(), req, resp); err != nil {
			t.Fatal(err)
		}
		for key, value := range parameters {
			if resp.Volume.VolumeContext[key] != value && key != "protocol" {
				t.Fatalf("response lost %s", key)
			}
		}
		if resp.Volume.VolumeContext[VolumeContextKeyClonedFromSnap] != "true" {
			t.Fatal("finalization lost clone no-format flag")
		}
	}
	if writes != 1 {
		t.Fatalf("same request wrote policy %d times; want one", writes)
	}
	dataset := &tnsapi.DatasetWithProperties{
		Dataset: tnsapi.Dataset{ID: "tank/csi/volume", Name: "tank/csi/volume"},
		UserProperties: map[string]tnsapi.UserProperty{
			tnsapi.PropertyManagedBy:        {Value: tnsapi.ManagedByValue},
			tnsapi.PropertyProtocol:         {Value: ProtocolNVMeOF},
			tnsapi.PropertyNVMeSubsystemNQN: {Value: nqnA},
			tnsapi.PropertyNVMePolicy:       {Value: properties[tnsapi.PropertyNVMePolicy]},
		},
	}
	meta, err := extractVolumeMetadata(dataset.ID, dataset)
	if err != nil {
		t.Fatal(err)
	}
	reconstructed := buildVolumeContext(*meta)
	for _, key := range []string{nvmeMaxSectorsKey, nvmeReadAheadKey, nvmeCtrlLossKey, nvmeIOQueuesKey, "port", "transport", "server", "nqn"} {
		if key == "server" || key == "nqn" {
			continue
		}
		if reconstructed[key] != parameters[key] {
			t.Fatalf("reconstruction lost %s: %v", key, reconstructed)
		}
	}
	delete(parameters, nvmeMaxSectorsKey)
	if err := s.finalizeNVMePolicy(context.Background(), req, response()); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("retry dropped original cap: %v", err)
	}
	if writes != 1 {
		t.Fatal("conflicting retry overwrote retained policy")
	}
}

func TestNVMeControllerPolicyPersistenceFailureRetainsResources(t *testing.T) {
	mock := &MockAPIClientForSnapshots{}
	mock.SetDatasetPropertiesFunc = func(context.Context, string, map[string]string) error { return errors.New("metadata write failed") }
	mock.DeleteDatasetFunc = func(context.Context, string) error { t.Fatal("policy failure destroyed volume"); return nil }
	mock.DeleteNVMeOFSubsystemFunc = func(context.Context, int) error { t.Fatal("policy failure deleted presentation"); return nil }
	s := NewControllerService(mock, nil, "")
	req := &csi.CreateVolumeRequest{Parameters: map[string]string{"protocol": ProtocolNVMeOF, nvmeMaxSectorsKey: "1024"}}
	resp := &csi.CreateVolumeResponse{Volume: &csi.Volume{VolumeId: "tank/csi/volume", VolumeContext: map[string]string{"server": "192.0.2.10", "nqn": nqnA}}}
	if err := s.finalizeNVMePolicy(context.Background(), req, resp); status.Code(err) != codes.Unavailable {
		t.Fatalf("persistence failure=%v; want Unavailable", err)
	}
}

func TestNVMeClonePolicyDoesNotInheritSourceIdentity(t *testing.T) {
	source := nvmeStoredPolicy{Version: 1, VolumeID: "tank/csi/source", NQN: nqnA, Server: "192.0.2.10", Transport: "tcp", Port: "4420", Parameters: map[string]string{nvmeMaxSectorsKey: "512"}}
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	mock := &MockAPIClientForSnapshots{}
	mock.GetDatasetPropertiesFunc = func(context.Context, string, []string) (map[string]string, error) {
		return map[string]string{tnsapi.PropertyNVMePolicy: string(encoded)}, nil
	}
	var written nvmeStoredPolicy
	mock.SetDatasetPropertiesFunc = func(_ context.Context, _ string, properties map[string]string) error {
		var parseErr error
		written, parseErr = parseStoredNVMePolicy(properties[tnsapi.PropertyNVMePolicy])
		return parseErr
	}
	s := NewControllerService(mock, nil, "")
	req := &csi.CreateVolumeRequest{Parameters: map[string]string{"protocol": ProtocolNVMeOF}, VolumeContentSource: &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "generic-snapshot"}}}}
	resp := &csi.CreateVolumeResponse{Volume: &csi.Volume{VolumeId: "tank/csi/clone", VolumeContext: map[string]string{"server": "192.0.2.10", "nqn": nqnB}}}
	if err := s.finalizeNVMePolicy(context.Background(), req, resp); err != nil {
		t.Fatal(err)
	}
	if written.VolumeID != resp.Volume.VolumeId || written.NQN != nqnB || !reflect.DeepEqual(written.Parameters, map[string]string{}) {
		t.Fatalf("clone retained source policy provenance: %+v", written)
	}
}

func TestNVMeCreateValidatesPolicyBeforeAlternatePaths(t *testing.T) {
	s := NewControllerService(nil, nil, "")
	req := &csi.CreateVolumeRequest{
		Name: "volume", Parameters: map[string]string{"protocol": ProtocolNVMeOF, "pool": "tank", "server": "192.0.2.10", nvmeMaxSectorsKey: "invalid"},
		VolumeCapabilities:  []*csi.VolumeCapability{{AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}, AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}}},
		VolumeContentSource: &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "generic-snapshot"}}},
	}
	if _, err := s.CreateVolume(context.Background(), req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid policy reached alternate path: %v", err)
	}
}
