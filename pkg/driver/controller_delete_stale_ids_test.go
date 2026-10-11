package driver

import (
	"context"
	"testing"

	"github.com/fenio/tns-csi/pkg/tnsapi"
)

// Presentation objects are deleted before the ZVOL, so a ZVOL delete that fails (e.g.
// dependent clones, retried for as long as the clones exist) leaves a ZVOL whose
// properties still name the deleted target/extent or namespace/subsystem. TrueNAS can
// hand those IDs to a new volume, and every retry would then delete that volume's
// presentation. The IDs must be zeroed on the ZVOL once their objects are gone, before
// the ZVOL delete is attempted.
func TestBlockVolumeDeleteForgetsPresentationIDsBeforeZVOL(t *testing.T) {
	recordForget := func(rec *orderedCalls, keys ...string) func(context.Context, string, map[string]string) error {
		return func(_ context.Context, _ string, props map[string]string) error {
			for _, k := range keys {
				if props[k] != "0" {
					return nil
				}
			}
			rec.add("ForgetIDs")
			return nil
		}
	}
	noProps := func(context.Context, string, []string) (map[string]string, error) { return map[string]string{}, nil }

	t.Run("iscsi", func(t *testing.T) {
		rec := &orderedCalls{}
		mock := &MockAPIClientForSnapshots{
			GetDatasetPropertiesFunc: noProps,
			SetDatasetPropertiesFunc: recordForget(rec, tnsapi.PropertyISCSITargetID, tnsapi.PropertyISCSIExtentID),
			DeleteDatasetFunc:        func(context.Context, string) error { rec.add("DeleteDataset"); return nil },
			DeleteISCSITargetFunc:    func(context.Context, int) error { rec.add("DeleteISCSITarget"); return nil },
			DeleteISCSIExtentFunc:    func(context.Context, int) error { rec.add("DeleteISCSIExtent"); return nil },
		}
		s := NewControllerService(mock, NewNodeRegistry(), "")
		if _, err := s.deleteISCSIVolume(context.Background(), &VolumeMetadata{
			Name: "tank/csi/pvc-i", Protocol: ProtocolISCSI, DatasetID: "tank/csi/pvc-i", ISCSITargetID: 5, ISCSIExtentID: 6,
		}); err != nil {
			t.Fatalf("deleteISCSIVolume() error = %v", err)
		}
		f, ex, d := rec.index("ForgetIDs"), rec.index("DeleteISCSIExtent"), rec.index("DeleteDataset")
		if f < 0 || f < ex || f > d {
			t.Fatalf("call order %v: want target/extent IDs zeroed after the extent delete and before the ZVOL delete", rec.calls)
		}
	})

	t.Run("nvmeof", func(t *testing.T) {
		rec := &orderedCalls{}
		mock := &MockAPIClientForSnapshots{
			GetDatasetPropertiesFunc:  noProps,
			SetDatasetPropertiesFunc:  recordForget(rec, tnsapi.PropertyNVMeSubsystemID, tnsapi.PropertyNVMeNamespaceID),
			DeleteDatasetFunc:         func(context.Context, string) error { rec.add("DeleteDataset"); return nil },
			DeleteNVMeOFNamespaceFunc: func(context.Context, int) error { rec.add("DeleteNVMeOFNamespace"); return nil },
			DeleteNVMeOFSubsystemFunc: func(context.Context, int) error { rec.add("DeleteNVMeOFSubsystem"); return nil },
		}
		s := NewControllerService(mock, NewNodeRegistry(), "")
		if _, err := s.deleteNVMeOFVolume(context.Background(), &VolumeMetadata{
			Name: "tank/csi/pvc-n", Protocol: ProtocolNVMeOF, DatasetID: "tank/csi/pvc-n", NVMeOFSubsystemID: 7, NVMeOFNamespaceID: 8,
		}); err != nil {
			t.Fatalf("deleteNVMeOFVolume() error = %v", err)
		}
		f, ss, d := rec.index("ForgetIDs"), rec.index("DeleteNVMeOFSubsystem"), rec.index("DeleteDataset")
		if f < 0 || f < ss || f > d {
			t.Fatalf("call order %v: want namespace/subsystem IDs zeroed after the subsystem delete and before the ZVOL delete", rec.calls)
		}
	})
}
