package driver

import (
	"context"
	"testing"

	"github.com/fenio/tns-csi/pkg/tnsapi"
)

// Presentation objects are deleted before the ZVOL, and the ZVOL's properties record their
// IDs. Each ID is cleared as soon as its own object is gone, so a later failure (the next
// object, or the ZVOL delete) leaves only the IDs of objects that still exist. Retries
// also check every object against the volume before deleting it (see
// TestBlockDeleteRetryNeverDeletesReassignedIDs), so a failed clear is not dangerous.
func TestBlockVolumeDeleteForgetsEachPresentationIDAfterItsDelete(t *testing.T) {
	recordForget := func(rec *orderedCalls) func(context.Context, string, map[string]string) error {
		return func(_ context.Context, _ string, props map[string]string) error {
			for k, v := range props {
				if v == "0" {
					rec.add("Forget:" + k)
				}
			}
			return nil
		}
	}
	noProps := func(context.Context, string, []string) (map[string]string, error) { return map[string]string{}, nil }
	inOrder := func(t *testing.T, rec *orderedCalls, steps ...string) {
		t.Helper()
		last := -1
		for _, step := range steps {
			i := rec.index(step)
			if i < 0 || i < last {
				t.Fatalf("call order %v: want %v in that order", rec.calls, steps)
			}
			last = i
		}
	}

	t.Run("iscsi", func(t *testing.T) {
		rec := &orderedCalls{}
		mock := &MockAPIClientForSnapshots{
			GetDatasetPropertiesFunc: noProps,
			SetDatasetPropertiesFunc: recordForget(rec),
			DeleteDatasetFunc:        func(context.Context, string) error { rec.add("DeleteDataset"); return nil },
			DeleteISCSITargetFunc:    func(context.Context, int) error { rec.add("DeleteISCSITarget"); return nil },
			DeleteISCSIExtentFunc:    func(context.Context, int) error { rec.add("DeleteISCSIExtent"); return nil },
		}
		s := NewControllerService(mock, NewNodeRegistry(), "")
		if _, err := s.deleteISCSIVolume(context.Background(), ownedMeta(mock, &VolumeMetadata{
			Name: "tank/csi/pvc-i", Protocol: ProtocolISCSI, DatasetID: "tank/csi/pvc-i", ISCSITargetID: 5, ISCSIExtentID: 6,
		})); err != nil {
			t.Fatalf("deleteISCSIVolume() error = %v", err)
		}
		inOrder(t, rec, "DeleteISCSITarget", "Forget:"+tnsapi.PropertyISCSITargetID,
			"DeleteISCSIExtent", "Forget:"+tnsapi.PropertyISCSIExtentID, "DeleteDataset")
	})

	t.Run("nvmeof", func(t *testing.T) {
		rec := &orderedCalls{}
		mock := &MockAPIClientForSnapshots{
			GetDatasetPropertiesFunc:  noProps,
			SetDatasetPropertiesFunc:  recordForget(rec),
			DeleteDatasetFunc:         func(context.Context, string) error { rec.add("DeleteDataset"); return nil },
			DeleteNVMeOFNamespaceFunc: func(context.Context, int) error { rec.add("DeleteNVMeOFNamespace"); return nil },
			DeleteNVMeOFSubsystemFunc: func(context.Context, int) error { rec.add("DeleteNVMeOFSubsystem"); return nil },
		}
		s := NewControllerService(mock, NewNodeRegistry(), "")
		if _, err := s.deleteNVMeOFVolume(context.Background(), ownedMeta(mock, &VolumeMetadata{
			Name: "tank/csi/pvc-n", Protocol: ProtocolNVMeOF, DatasetID: "tank/csi/pvc-n", NVMeOFSubsystemID: 7, NVMeOFNamespaceID: 8,
		})); err != nil {
			t.Fatalf("deleteNVMeOFVolume() error = %v", err)
		}
		inOrder(t, rec, "DeleteNVMeOFNamespace", "Forget:"+tnsapi.PropertyNVMeNamespaceID,
			"DeleteNVMeOFSubsystem", "Forget:"+tnsapi.PropertyNVMeSubsystemID, "DeleteDataset")
	})
}
