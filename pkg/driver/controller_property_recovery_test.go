package driver

import (
	"context"
	"testing"

	"github.com/fenio/tns-csi/pkg/tnsapi"
)

// Ownership properties are now written by the create call, so every volume has
// tns-csi:managed_by from birth. The handle-existing recovery must therefore key on the
// presentation ID property, not managed_by: a CreateVolume that times out between
// creating the share/target and recording its ID would otherwise never record it, and
// DeleteVolume (which reads IDs from properties only) would leak the share/target.
func TestPropertyRecoveryWritesPresentationIDsWhenOnlyOwnershipIsSet(t *testing.T) {
	ownershipOnly := func(context.Context, string, []string) (map[string]string, error) {
		return map[string]string{
			tnsapi.PropertyManagedBy:     tnsapi.ManagedByValue,
			tnsapi.PropertyCSIVolumeName: "pvc-1",
		}, nil
	}
	cases := []struct {
		invoke func(s *ControllerService)
		name   string
		key    string
		want   string
	}{
		{name: "nfs", key: tnsapi.PropertyNFSShareID, want: "11", invoke: func(s *ControllerService) {
			s.ensureNFSProperties(context.Background(), "tank/pvc-1", &nfsVolumeParams{volumeName: "pvc-1"}, &tnsapi.NFSShare{ID: 11, Path: "/mnt/tank/pvc-1"})
		}},
		{name: "smb", key: tnsapi.PropertySMBShareID, want: "12", invoke: func(s *ControllerService) {
			s.ensureSMBProperties(context.Background(), "tank/pvc-1", &smbVolumeParams{volumeName: "pvc-1"}, &tnsapi.SMBShare{ID: 12, Name: "pvc-1"})
		}},
		{name: "iscsi", key: tnsapi.PropertyISCSITargetID, want: "13", invoke: func(s *ControllerService) {
			s.ensureISCSIProperties(context.Background(), "tank/pvc-1", &iscsiVolumeParams{volumeName: "pvc-1"}, &tnsapi.ISCSITarget{ID: 13}, &tnsapi.ISCSIExtent{ID: 14}, "iqn.x:pvc-1")
		}},
		{name: "nvmeof", key: tnsapi.PropertyNVMeNamespaceID, want: "16", invoke: func(s *ControllerService) {
			s.ensureNVMeOFProperties(context.Background(), "tank/pvc-1", &nvmeofVolumeParams{volumeName: "pvc-1"}, &tnsapi.NVMeOFSubsystem{ID: 15, NQN: "nqn.x"}, &tnsapi.NVMeOFNamespace{ID: 16})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var written map[string]string
			mock := &MockAPIClientForSnapshots{
				GetDatasetPropertiesFunc: ownershipOnly,
				SetDatasetPropertiesFunc: func(_ context.Context, _ string, props map[string]string) error {
					written = props
					return nil
				},
			}
			tc.invoke(NewControllerService(mock, NewNodeRegistry(), ""))
			if written[tc.key] != tc.want {
				t.Fatalf("recovery wrote %s=%q, want %q (written: %v)", tc.key, written[tc.key], tc.want, written)
			}
		})
	}

	t.Run("complete set is left alone", func(t *testing.T) {
		calls := 0
		mock := &MockAPIClientForSnapshots{
			GetDatasetPropertiesFunc: func(context.Context, string, []string) (map[string]string, error) {
				return map[string]string{tnsapi.PropertyManagedBy: tnsapi.ManagedByValue, tnsapi.PropertyNFSShareID: "11"}, nil
			},
			SetDatasetPropertiesFunc: func(context.Context, string, map[string]string) error { calls++; return nil },
		}
		NewControllerService(mock, NewNodeRegistry(), "").ensureNFSProperties(context.Background(), "tank/pvc-1", &nfsVolumeParams{volumeName: "pvc-1"}, &tnsapi.NFSShare{ID: 11})
		if calls != 0 {
			t.Fatalf("SetDatasetProperties called %d times for a complete property set", calls)
		}
	})
}
