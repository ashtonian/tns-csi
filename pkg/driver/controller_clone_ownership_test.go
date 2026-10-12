package driver

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/fenio/tns-csi/pkg/tnsapi"
)

// Clones are new datasets too. Their ownership properties used to be written only by a
// later, non-fatal SetDatasetProperties, so a failure there left a clone without
// tns-csi:managed_by, the same leak the create path had. pool.snapshot.clone accepts
// dataset_properties, so ownership goes into the clone call itself.
func TestSnapshotCloneCarriesOwnershipProperties(t *testing.T) {
	var got map[string]string
	mock := &MockAPIClientForSnapshots{CloneSnapshotFunc: func(_ context.Context, p tnsapi.CloneSnapshotParams) (*tnsapi.Dataset, error) {
		got = p.DatasetProperties
		return &tnsapi.Dataset{ID: p.Dataset, Name: p.Dataset}, nil
	}}
	s := NewControllerService(mock, NewNodeRegistry(), "")
	req := &csi.CreateVolumeRequest{
		Name:          "pvc-clone",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 2 << 30},
		Parameters:    map[string]string{"deleteStrategy": tnsapi.DeleteStrategyRetain},
	}
	meta := &SnapshotMetadata{Protocol: tnsapi.ProtocolNVMeOF, DatasetName: "tank/csi/src", SnapshotName: "tank/csi/src@snap"}

	cp, err := s.validateCloneParameters(req, meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.executeSnapshotClone(context.Background(), meta, cp); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		tnsapi.PropertyManagedBy:      tnsapi.ManagedByValue,
		tnsapi.PropertyCSIVolumeName:  "pvc-clone",
		tnsapi.PropertyProtocol:       tnsapi.ProtocolNVMeOF,
		tnsapi.PropertyDeleteStrategy: tnsapi.DeleteStrategyRetain,
		tnsapi.PropertyCapacityBytes:  "2147483648",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("clone dataset_properties[%s] = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
}
