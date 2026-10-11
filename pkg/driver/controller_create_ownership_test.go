package driver

import (
	"context"
	"testing"

	"github.com/fenio/tns-csi/pkg/metrics"
	"github.com/fenio/tns-csi/pkg/tnsapi"
)

func userPropsMap(kvs []tnsapi.UserPropertyKV) map[string]string {
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		m[kv.Key] = kv.Value
	}
	return m
}

// The ownership properties must be part of the create call itself. When they were only
// written by a later, non-fatal pool.dataset.update, a failure there left a dataset
// without tns-csi:managed_by: DeleteVolume then treated it as "not found" and the
// dataset (plus its share/target) leaked forever.
func TestCreateCallsCarryOwnershipProperties(t *testing.T) {
	type want struct{ protocol, name, strategy string }
	check := func(t *testing.T, got map[string]string, w want) {
		t.Helper()
		expect := map[string]string{
			tnsapi.PropertyManagedBy:      tnsapi.ManagedByValue,
			tnsapi.PropertyCSIVolumeName:  w.name,
			tnsapi.PropertyProtocol:       w.protocol,
			tnsapi.PropertyDeleteStrategy: w.strategy,
			tnsapi.PropertySchemaVersion:  tnsapi.SchemaVersionV1,
		}
		for k, v := range expect {
			if got[k] != v {
				t.Errorf("user_properties[%s] = %q, want %q (all: %v)", k, got[k], v, got)
			}
		}
	}

	t.Run("nfs", func(t *testing.T) {
		var got map[string]string
		mock := &MockAPIClientForSnapshots{CreateDatasetFunc: func(_ context.Context, p tnsapi.DatasetCreateParams) (*tnsapi.Dataset, error) {
			got = userPropsMap(p.UserProperties)
			return &tnsapi.Dataset{ID: p.Name, Name: p.Name}, nil
		}}
		s := NewControllerService(mock, NewNodeRegistry(), "")
		params := &nfsVolumeParams{volumeName: "pvc-1", datasetName: "tank/csi/pvc-1", deleteStrategy: tnsapi.DeleteStrategyRetain, requestedCapacity: 1 << 30}
		if _, _, err := s.getOrCreateDataset(context.Background(), params, nil, metrics.NewVolumeOperationTimer(metrics.ProtocolNFS, "create")); err != nil {
			t.Fatal(err)
		}
		check(t, got, want{tnsapi.ProtocolNFS, "pvc-1", tnsapi.DeleteStrategyRetain})
	})

	t.Run("smb", func(t *testing.T) {
		var got map[string]string
		mock := &MockAPIClientForSnapshots{CreateDatasetFunc: func(_ context.Context, p tnsapi.DatasetCreateParams) (*tnsapi.Dataset, error) {
			got = userPropsMap(p.UserProperties)
			return &tnsapi.Dataset{ID: p.Name, Name: p.Name}, nil
		}}
		s := NewControllerService(mock, NewNodeRegistry(), "")
		params := &nfsVolumeParams{volumeName: "pvc-2", datasetName: "tank/csi/pvc-2", deleteStrategy: tnsapi.DeleteStrategyDelete, shareType: "SMB", requestedCapacity: 1 << 30}
		if _, _, err := s.getOrCreateDataset(context.Background(), params, nil, metrics.NewVolumeOperationTimer(metrics.ProtocolSMB, "create")); err != nil {
			t.Fatal(err)
		}
		check(t, got, want{tnsapi.ProtocolSMB, "pvc-2", tnsapi.DeleteStrategyDelete})
	})

	t.Run("iscsi", func(t *testing.T) {
		var got map[string]string
		mock := &MockAPIClientForSnapshots{CreateZvolFunc: func(_ context.Context, p tnsapi.ZvolCreateParams) (*tnsapi.Dataset, error) {
			got = userPropsMap(p.UserProperties)
			return &tnsapi.Dataset{ID: p.Name, Name: p.Name}, nil
		}}
		s := NewControllerService(mock, NewNodeRegistry(), "")
		params := &iscsiVolumeParams{volumeName: "pvc-3", zvolName: "tank/csi/pvc-3", deleteStrategy: tnsapi.DeleteStrategyDelete, requestedCapacity: 1 << 30}
		if _, _, err := s.getOrCreateZVOLForISCSI(context.Background(), params, nil, metrics.NewVolumeOperationTimer(metrics.ProtocolISCSI, "create")); err != nil {
			t.Fatal(err)
		}
		check(t, got, want{tnsapi.ProtocolISCSI, "pvc-3", tnsapi.DeleteStrategyDelete})
	})

	t.Run("nvmeof", func(t *testing.T) {
		var got map[string]string
		mock := &MockAPIClientForSnapshots{CreateZvolFunc: func(_ context.Context, p tnsapi.ZvolCreateParams) (*tnsapi.Dataset, error) {
			got = userPropsMap(p.UserProperties)
			return &tnsapi.Dataset{ID: p.Name, Name: p.Name}, nil
		}}
		s := NewControllerService(mock, NewNodeRegistry(), "")
		params := &nvmeofVolumeParams{volumeName: "pvc-4", zvolName: "tank/csi/pvc-4", deleteStrategy: tnsapi.DeleteStrategyRetain, requestedCapacity: 1 << 30}
		if _, _, err := s.getOrCreateZVOL(context.Background(), params, nil, metrics.NewVolumeOperationTimer(metrics.ProtocolNVMeOF, "create")); err != nil {
			t.Fatal(err)
		}
		check(t, got, want{tnsapi.ProtocolNVMeOF, "pvc-4", tnsapi.DeleteStrategyRetain})
	})
}
