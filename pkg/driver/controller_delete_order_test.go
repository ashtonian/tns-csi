package driver

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// orderedCalls records destructive calls in order.
type orderedCalls struct{ calls []string }

func (o *orderedCalls) add(name string) { o.calls = append(o.calls, name) }

func (o *orderedCalls) index(name string) int {
	for i, c := range o.calls {
		if c == name {
			return i
		}
	}
	return -1
}

// The ZVOL holds the volume's ownership properties, which is how a retried DeleteVolume
// finds the volume again. Deleting the ZVOL first and the target/subsystem afterwards
// meant a failed presentation cleanup was never retried: the next DeleteVolume found no
// volume, reported success, and the target/subsystem leaked forever. Presentation
// objects must go first, and a failure there must leave the ZVOL in place.
func TestBlockVolumeDeleteKeepsZVOLWhenPresentationCleanupFails(t *testing.T) {
	cleanupFailure := errors.New("TrueNAS: permission denied") // not retryable: keeps the test fast

	t.Run("iscsi target delete fails", func(t *testing.T) {
		rec := &orderedCalls{}
		mock := &MockAPIClientForSnapshots{
			GetDatasetPropertiesFunc: func(context.Context, string, []string) (map[string]string, error) {
				return map[string]string{}, nil
			},
			DeleteDatasetFunc:     func(context.Context, string) error { rec.add("DeleteDataset"); return nil },
			DeleteISCSITargetFunc: func(context.Context, int) error { rec.add("DeleteISCSITarget"); return cleanupFailure },
			DeleteISCSIExtentFunc: func(context.Context, int) error { rec.add("DeleteISCSIExtent"); return nil },
		}
		s := NewControllerService(mock, NewNodeRegistry(), "")
		_, err := s.deleteISCSIVolume(context.Background(), ownedMeta(mock, &VolumeMetadata{
			Name: "tank/csi/pvc-i", Protocol: ProtocolISCSI, DatasetID: "tank/csi/pvc-i", ISCSITargetID: 5, ISCSIExtentID: 6,
		}))
		if status.Code(err) == codes.OK {
			t.Fatal("deleteISCSIVolume() succeeded although the target could not be deleted")
		}
		if rec.index("DeleteDataset") >= 0 {
			t.Fatalf("ZVOL deleted although target cleanup failed (calls %v): the target would leak", rec.calls)
		}
	})

	t.Run("nvmeof subsystem delete fails", func(t *testing.T) {
		rec := &orderedCalls{}
		mock := &MockAPIClientForSnapshots{
			GetDatasetPropertiesFunc: func(context.Context, string, []string) (map[string]string, error) {
				return map[string]string{}, nil
			},
			DeleteDatasetFunc:         func(context.Context, string) error { rec.add("DeleteDataset"); return nil },
			DeleteNVMeOFNamespaceFunc: func(context.Context, int) error { rec.add("DeleteNVMeOFNamespace"); return nil },
			DeleteNVMeOFSubsystemFunc: func(context.Context, int) error { rec.add("DeleteNVMeOFSubsystem"); return cleanupFailure },
		}
		s := NewControllerService(mock, NewNodeRegistry(), "")
		_, err := s.deleteNVMeOFVolume(context.Background(), ownedMeta(mock, &VolumeMetadata{
			Name: "tank/csi/pvc-n", Protocol: ProtocolNVMeOF, DatasetID: "tank/csi/pvc-n", NVMeOFSubsystemID: 7, NVMeOFNamespaceID: 8,
		}))
		if status.Code(err) == codes.OK {
			t.Fatal("deleteNVMeOFVolume() succeeded although the subsystem could not be deleted")
		}
		if rec.index("DeleteDataset") >= 0 {
			t.Fatalf("ZVOL deleted although subsystem cleanup failed (calls %v): the subsystem would leak", rec.calls)
		}
	})
}

func TestBlockVolumeDeleteRemovesPresentationBeforeZVOL(t *testing.T) {
	t.Run("iscsi", func(t *testing.T) {
		rec := &orderedCalls{}
		mock := &MockAPIClientForSnapshots{
			GetDatasetPropertiesFunc: func(context.Context, string, []string) (map[string]string, error) {
				return map[string]string{}, nil
			},
			DeleteDatasetFunc:     func(context.Context, string) error { rec.add("DeleteDataset"); return nil },
			DeleteISCSITargetFunc: func(context.Context, int) error { rec.add("DeleteISCSITarget"); return nil },
			DeleteISCSIExtentFunc: func(context.Context, int) error { rec.add("DeleteISCSIExtent"); return nil },
		}
		s := NewControllerService(mock, NewNodeRegistry(), "")
		if _, err := s.deleteISCSIVolume(context.Background(), ownedMeta(mock, &VolumeMetadata{
			Name: "tank/csi/pvc-i", Protocol: ProtocolISCSI, DatasetID: "tank/csi/pvc-i", ISCSITargetID: 5, ISCSIExtentID: 6,
		})); err != nil {
			t.Fatalf("deleteISCSIVolume() error = %v", err)
		}
		if d, tg, ex := rec.index("DeleteDataset"), rec.index("DeleteISCSITarget"), rec.index("DeleteISCSIExtent"); d < 0 || d < tg || d < ex {
			t.Fatalf("call order %v: want target and extent deleted before the ZVOL", rec.calls)
		}
	})

	t.Run("nvmeof", func(t *testing.T) {
		rec := &orderedCalls{}
		mock := &MockAPIClientForSnapshots{
			GetDatasetPropertiesFunc: func(context.Context, string, []string) (map[string]string, error) {
				return map[string]string{}, nil
			},
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
		if d, ns, ss := rec.index("DeleteDataset"), rec.index("DeleteNVMeOFNamespace"), rec.index("DeleteNVMeOFSubsystem"); d < 0 || d < ns || d < ss {
			t.Fatalf("call order %v: want namespace and subsystem deleted before the ZVOL", rec.calls)
		}
	})
}
