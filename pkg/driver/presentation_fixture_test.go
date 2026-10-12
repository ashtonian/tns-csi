package driver

import (
	"context"
	"path"

	"github.com/fenio/tns-csi/pkg/tnsapi"
)

// ownedMeta makes mock report meta's own presentation objects as existing: the iSCSI
// target (named after the volume) and extent (backed by its ZVOL), and the NVMe-oF
// subsystem and namespace. The namespace disappears once it is deleted, as on TrueNAS.
// Hooks a test already set are left alone. Block deletes check each object against the
// volume before deleting it by ID, so a mock where every lookup reports "not found"
// would correctly delete nothing.
func ownedMeta(mock *MockAPIClientForSnapshots, meta *VolumeMetadata) *VolumeMetadata {
	name := path.Base(meta.Name)
	device := "zvol/" + meta.DatasetID

	if mock.QueryISCSITargetsFunc == nil {
		mock.QueryISCSITargetsFunc = func(_ context.Context, filters []interface{}) ([]tnsapi.ISCSITarget, error) {
			if id, ok := filterID(filters); ok && id == meta.ISCSITargetID {
				return []tnsapi.ISCSITarget{{ID: id, Name: name}}, nil
			}
			return nil, nil
		}
	}
	if mock.QueryISCSIExtentsFunc == nil {
		mock.QueryISCSIExtentsFunc = func(_ context.Context, filters []interface{}) ([]tnsapi.ISCSIExtent, error) {
			if id, ok := filterID(filters); ok && id == meta.ISCSIExtentID {
				return []tnsapi.ISCSIExtent{{ID: id, Name: name, Disk: device}}, nil
			}
			return nil, nil
		}
	}
	if mock.QueryNVMeOFSubsystemsFunc == nil {
		mock.QueryNVMeOFSubsystemsFunc = func(_ context.Context, filters []interface{}) ([]tnsapi.NVMeOFSubsystem, error) {
			if id, ok := filterID(filters); ok && id == meta.NVMeOFSubsystemID {
				nqn := meta.NVMeOFNQN
				if nqn == "" {
					nqn = "nqn.2137.csi.tns:" + name
				}
				return []tnsapi.NVMeOFSubsystem{{ID: id, Name: nqn, NQN: nqn}}, nil
			}
			return nil, nil
		}
	}
	if mock.QueryNVMeOFNamespaceByIDFunc == nil {
		namespaceGone := false
		deleteNamespace := mock.DeleteNVMeOFNamespaceFunc
		mock.DeleteNVMeOFNamespaceFunc = func(ctx context.Context, id int) error {
			var err error
			if deleteNamespace != nil {
				err = deleteNamespace(ctx, id)
			}
			if err == nil || tnsapi.IsNotFound(err) {
				namespaceGone = true
			}
			return err
		}
		mock.QueryNVMeOFNamespaceByIDFunc = func(_ context.Context, id int) (*tnsapi.NVMeOFNamespace, error) {
			if !namespaceGone && id == meta.NVMeOFNamespaceID {
				return &tnsapi.NVMeOFNamespace{ID: id, NSID: 1, Device: device}, nil
			}
			var none *tnsapi.NVMeOFNamespace
			return none, nil
		}
	}
	return meta
}
