package driver

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/fenio/tns-csi/pkg/tnsapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeTrueNAS is a small stateful TrueNAS for block-volume delete retries: one ZVOL with
// its properties, plus iSCSI targets/extents/mappings and NVMe-oF namespaces/subsystems
// keyed by ID. Tests fail individual calls, then reassign deleted IDs to another volume
// between attempts, the way TrueNAS can after a delete.
type fakeTrueNAS struct {
	props       map[string]string
	targets     map[int]tnsapi.ISCSITarget
	extents     map[int]tnsapi.ISCSIExtent
	mappings    map[int]tnsapi.ISCSITargetExtent
	namespaces  map[int]tnsapi.NVMeOFNamespace
	subsystems  map[int]tnsapi.NVMeOFSubsystem
	failNext    map[string]error // one-shot failures by call name
	failAlways  map[string]error
	deleted     []string // "<call>:<id>" of every successful delete
	zvolDeleted bool
}

func newFakeTrueNAS(props map[string]string) *fakeTrueNAS {
	return &fakeTrueNAS{
		props:      props,
		targets:    map[int]tnsapi.ISCSITarget{},
		extents:    map[int]tnsapi.ISCSIExtent{},
		mappings:   map[int]tnsapi.ISCSITargetExtent{},
		namespaces: map[int]tnsapi.NVMeOFNamespace{},
		subsystems: map[int]tnsapi.NVMeOFSubsystem{},
		failNext:   map[string]error{},
		failAlways: map[string]error{},
	}
}

var errInjected = errors.New("TrueNAS: permission denied") // not retryable: keeps tests fast

func (f *fakeTrueNAS) fail(call string) error {
	if err := f.failAlways[call]; err != nil {
		return err
	}
	if err := f.failNext[call]; err != nil {
		delete(f.failNext, call)
		return err
	}
	return nil
}

func (f *fakeTrueNAS) record(call string, id int) {
	f.deleted = append(f.deleted, call+":"+strconv.Itoa(id))
}

func (f *fakeTrueNAS) wasDeleted(call string, id int) bool {
	for _, d := range f.deleted {
		if d == call+":"+strconv.Itoa(id) {
			return true
		}
	}
	return false
}

func filterID(filters []interface{}) (int, bool) {
	for _, f := range filters {
		if c, ok := f.([]interface{}); ok && len(c) == 3 && c[0] == "id" && c[1] == "=" {
			if id, ok := c[2].(int); ok {
				return id, true
			}
		}
	}
	return 0, false
}

func (f *fakeTrueNAS) client() *MockAPIClientForSnapshots {
	return &MockAPIClientForSnapshots{
		GetDatasetPropertiesFunc: func(context.Context, string, []string) (map[string]string, error) {
			out := make(map[string]string, len(f.props))
			for k, v := range f.props {
				out[k] = v
			}
			return out, nil
		},
		SetDatasetPropertiesFunc: func(_ context.Context, _ string, p map[string]string) error {
			if err := f.fail("SetDatasetProperties"); err != nil {
				return err
			}
			for k, v := range p {
				f.props[k] = v
			}
			return nil
		},
		DeleteDatasetFunc: func(context.Context, string) error {
			if err := f.fail("DeleteDataset"); err != nil {
				return err
			}
			f.zvolDeleted = true
			return nil
		},
		QueryISCSITargetsFunc: func(_ context.Context, filters []interface{}) ([]tnsapi.ISCSITarget, error) {
			if id, ok := filterID(filters); ok {
				if t, ok := f.targets[id]; ok {
					return []tnsapi.ISCSITarget{t}, nil
				}
			}
			return nil, nil
		},
		QueryISCSIExtentsFunc: func(_ context.Context, filters []interface{}) ([]tnsapi.ISCSIExtent, error) {
			if id, ok := filterID(filters); ok {
				if e, ok := f.extents[id]; ok {
					return []tnsapi.ISCSIExtent{e}, nil
				}
			}
			return nil, nil
		},
		ISCSITargetExtentByTargetFunc: func(_ context.Context, targetID int) ([]tnsapi.ISCSITargetExtent, error) {
			var out []tnsapi.ISCSITargetExtent
			for _, m := range f.mappings {
				if m.Target == targetID {
					out = append(out, m)
				}
			}
			return out, nil
		},
		DeleteISCSITargetExtentFunc: func(_ context.Context, id int) error {
			delete(f.mappings, id)
			f.record("DeleteISCSITargetExtent", id)
			return nil
		},
		DeleteISCSITargetFunc: func(_ context.Context, id int) error {
			if err := f.fail("DeleteISCSITarget"); err != nil {
				return err
			}
			delete(f.targets, id)
			f.record("DeleteISCSITarget", id)
			return nil
		},
		DeleteISCSIExtentFunc: func(_ context.Context, id int) error {
			if err := f.fail("DeleteISCSIExtent"); err != nil {
				return err
			}
			delete(f.extents, id)
			f.record("DeleteISCSIExtent", id)
			return nil
		},
		QueryNVMeOFNamespaceByIDFunc: func(_ context.Context, id int) (*tnsapi.NVMeOFNamespace, error) {
			if ns, ok := f.namespaces[id]; ok {
				return &ns, nil
			}
			var none *tnsapi.NVMeOFNamespace
			return none, nil
		},
		QueryNVMeOFNamespacesFunc: func(_ context.Context, _ []interface{}) ([]tnsapi.NVMeOFNamespace, error) {
			out := make([]tnsapi.NVMeOFNamespace, 0, len(f.namespaces))
			for _, ns := range f.namespaces {
				out = append(out, ns)
			}
			return out, nil
		},
		QueryAllNVMeOFNamespacesFunc: func(context.Context) ([]tnsapi.NVMeOFNamespace, error) {
			out := make([]tnsapi.NVMeOFNamespace, 0, len(f.namespaces))
			for _, ns := range f.namespaces {
				out = append(out, ns)
			}
			return out, nil
		},
		QueryNVMeOFSubsystemsFunc: func(_ context.Context, filters []interface{}) ([]tnsapi.NVMeOFSubsystem, error) {
			if id, ok := filterID(filters); ok {
				if sub, ok := f.subsystems[id]; ok {
					return []tnsapi.NVMeOFSubsystem{sub}, nil
				}
			}
			return nil, nil
		},
		DeleteNVMeOFNamespaceFunc: func(_ context.Context, id int) error {
			if err := f.fail("DeleteNVMeOFNamespace"); err != nil {
				return err
			}
			delete(f.namespaces, id)
			f.record("DeleteNVMeOFNamespace", id)
			return nil
		},
		DeleteNVMeOFSubsystemFunc: func(_ context.Context, id int) error {
			if err := f.fail("DeleteNVMeOFSubsystem"); err != nil {
				return err
			}
			delete(f.subsystems, id)
			f.record("DeleteNVMeOFSubsystem", id)
			return nil
		},
	}
}

// metaFromProps rebuilds the volume metadata the way a retried DeleteVolume does: from
// the ZVOL's current properties.
func (f *fakeTrueNAS) metaFromProps(name, protocol, datasetID string) *VolumeMetadata {
	return &VolumeMetadata{
		Name: name, Protocol: protocol, DatasetID: datasetID, DatasetName: datasetID,
		ISCSITargetID:     tnsapi.StringToInt(f.props[tnsapi.PropertyISCSITargetID]),
		ISCSIExtentID:     tnsapi.StringToInt(f.props[tnsapi.PropertyISCSIExtentID]),
		NVMeOFSubsystemID: tnsapi.StringToInt(f.props[tnsapi.PropertyNVMeSubsystemID]),
		NVMeOFNamespaceID: tnsapi.StringToInt(f.props[tnsapi.PropertyNVMeNamespaceID]),
		NVMeOFNQN:         f.props[tnsapi.PropertyNVMeSubsystemNQN],
	}
}

const (
	iscsiVol     = "pvc-i"
	iscsiZvol    = "tank/csi/pvc-i"
	nvmeVol      = "pvc-n"
	nvmeZvol     = "tank/csi/pvc-n"
	nvmeNQN      = "nqn.2011-06.com.truenas:uuid:1:nqn.2137.csi.tns:pvc-n"
	nvmeNQNShort = "nqn.2137.csi.tns:pvc-n"
)

func newISCSIFake() *fakeTrueNAS {
	f := newFakeTrueNAS(map[string]string{
		tnsapi.PropertyManagedBy: tnsapi.ManagedByValue, tnsapi.PropertyCSIVolumeName: iscsiVol,
		tnsapi.PropertyDeleteStrategy: tnsapi.DeleteStrategyDelete,
		tnsapi.PropertyISCSITargetID:  "5", tnsapi.PropertyISCSIExtentID: "6",
	})
	f.targets[5] = tnsapi.ISCSITarget{ID: 5, Name: iscsiVol}
	f.extents[6] = tnsapi.ISCSIExtent{ID: 6, Name: iscsiVol, Disk: "zvol/" + iscsiZvol}
	f.mappings[50] = tnsapi.ISCSITargetExtent{ID: 50, Target: 5, Extent: 6}
	return f
}

func newNVMeFake() *fakeTrueNAS {
	f := newFakeTrueNAS(map[string]string{
		tnsapi.PropertyManagedBy: tnsapi.ManagedByValue, tnsapi.PropertyCSIVolumeName: nvmeVol,
		tnsapi.PropertyDeleteStrategy:  tnsapi.DeleteStrategyDelete,
		tnsapi.PropertyNVMeSubsystemID: "7", tnsapi.PropertyNVMeNamespaceID: "8",
		tnsapi.PropertyNVMeSubsystemNQN: nvmeNQN,
	})
	f.subsystems[7] = tnsapi.NVMeOFSubsystem{ID: 7, Name: nvmeNQNShort, NQN: nvmeNQN}
	f.namespaces[8] = tnsapi.NVMeOFNamespace{ID: 8, NSID: 1, Device: "zvol/" + nvmeZvol, Subsys: &tnsapi.NVMeOFNamespaceSubsystem{ID: 7}}
	return f
}

// reassignISCSI gives the deleted IDs to another volume, as TrueNAS may after a delete.
func reassignISCSI(f *fakeTrueNAS, targetID, extentID int) {
	if targetID != 0 {
		f.targets[targetID] = tnsapi.ISCSITarget{ID: targetID, Name: "pvc-other"}
		f.mappings[90] = tnsapi.ISCSITargetExtent{ID: 90, Target: targetID, Extent: 99}
	}
	if extentID != 0 {
		f.extents[extentID] = tnsapi.ISCSIExtent{ID: extentID, Name: "pvc-other", Disk: "zvol/tank/csi/pvc-other"}
	}
}

func reassignNVMe(f *fakeTrueNAS, subsystemID, namespaceID int) {
	if subsystemID != 0 {
		f.subsystems[subsystemID] = tnsapi.NVMeOFSubsystem{ID: subsystemID, Name: "nqn.2137.csi.tns:pvc-other", NQN: "nqn.2011-06.com.truenas:uuid:2:nqn.2137.csi.tns:pvc-other"}
	}
	if namespaceID != 0 {
		f.namespaces[namespaceID] = tnsapi.NVMeOFNamespace{ID: namespaceID, NSID: 1, Device: "zvol/tank/csi/pvc-other", Subsys: &tnsapi.NVMeOFNamespaceSubsystem{ID: 33}}
	}
}

// Deleting a block volume removes its presentation objects first and the ZVOL last; the
// ZVOL keeps the IDs in its properties so a retry can finish. If an ID is reused by
// another volume in between (partial cleanup, a failed property write, a ZVOL delete that
// keeps failing), the retry must not delete the other volume's objects.
func TestBlockDeleteRetryNeverDeletesReassignedIDs(t *testing.T) {
	ctx := context.Background()

	t.Run("iscsi: extent delete fails after the target is gone", func(t *testing.T) {
		f := newISCSIFake()
		f.failNext["DeleteISCSIExtent"] = errInjected
		s := NewControllerService(f.client(), NewNodeRegistry(), "")
		if _, err := s.deleteISCSIVolume(ctx, f.metaFromProps(iscsiVol, ProtocolISCSI, iscsiZvol)); err == nil || f.zvolDeleted {
			t.Fatalf("attempt 1: err=%v zvolDeleted=%v; want an error and the ZVOL kept", err, f.zvolDeleted)
		}
		reassignISCSI(f, 5, 0) // target 5 now belongs to pvc-other
		if _, err := s.deleteISCSIVolume(ctx, f.metaFromProps(iscsiVol, ProtocolISCSI, iscsiZvol)); err != nil {
			t.Fatalf("attempt 2: %v", err)
		}
		if _, ok := f.targets[5]; !ok || f.wasDeleted("DeleteISCSITargetExtent", 90) {
			t.Fatalf("retry deleted the reassigned target 5 or its mapping (deletes: %v)", f.deleted)
		}
		if !f.wasDeleted("DeleteISCSIExtent", 6) || !f.zvolDeleted {
			t.Fatalf("retry did not finish: deletes %v, zvolDeleted=%v", f.deleted, f.zvolDeleted)
		}
	})

	t.Run("iscsi: property write and ZVOL delete both fail", func(t *testing.T) {
		f := newISCSIFake()
		f.failAlways["SetDatasetProperties"] = errInjected
		f.failAlways["DeleteDataset"] = errInjected // the ZVOL delete is retried within one attempt
		s := NewControllerService(f.client(), NewNodeRegistry(), "")
		if _, err := s.deleteISCSIVolume(ctx, f.metaFromProps(iscsiVol, ProtocolISCSI, iscsiZvol)); err == nil || f.zvolDeleted {
			t.Fatalf("attempt 1: err=%v zvolDeleted=%v; want an error and the ZVOL kept", err, f.zvolDeleted)
		}
		delete(f.failAlways, "DeleteDataset")
		reassignISCSI(f, 5, 6) // both IDs now belong to pvc-other
		if _, err := s.deleteISCSIVolume(ctx, f.metaFromProps(iscsiVol, ProtocolISCSI, iscsiZvol)); err != nil {
			t.Fatalf("attempt 2: %v", err)
		}
		if _, ok := f.targets[5]; !ok {
			t.Fatalf("retry deleted reassigned target 5 (deletes: %v)", f.deleted)
		}
		if _, ok := f.extents[6]; !ok {
			t.Fatalf("retry deleted reassigned extent 6 (deletes: %v)", f.deleted)
		}
		if f.wasDeleted("DeleteISCSITargetExtent", 90) || !f.zvolDeleted {
			t.Fatalf("deletes %v, zvolDeleted=%v; want other volume's mapping kept and the ZVOL gone", f.deleted, f.zvolDeleted)
		}
	})

	t.Run("nvmeof: subsystem delete fails after the namespace is gone", func(t *testing.T) {
		f := newNVMeFake()
		f.failNext["DeleteNVMeOFSubsystem"] = errInjected
		s := NewControllerService(f.client(), NewNodeRegistry(), "")
		if _, err := s.deleteNVMeOFVolume(ctx, f.metaFromProps(nvmeVol, ProtocolNVMeOF, nvmeZvol)); err == nil || f.zvolDeleted {
			t.Fatalf("attempt 1: err=%v zvolDeleted=%v; want an error and the ZVOL kept", err, f.zvolDeleted)
		}
		reassignNVMe(f, 0, 8) // namespace 8 now belongs to pvc-other
		if _, err := s.deleteNVMeOFVolume(ctx, f.metaFromProps(nvmeVol, ProtocolNVMeOF, nvmeZvol)); err != nil {
			t.Fatalf("attempt 2: %v", err)
		}
		if _, ok := f.namespaces[8]; !ok {
			t.Fatalf("retry deleted reassigned namespace 8 (deletes: %v)", f.deleted)
		}
		if !f.wasDeleted("DeleteNVMeOFSubsystem", 7) || !f.zvolDeleted {
			t.Fatalf("retry did not finish: deletes %v, zvolDeleted=%v", f.deleted, f.zvolDeleted)
		}
	})

	t.Run("nvmeof: property write and ZVOL delete both fail", func(t *testing.T) {
		f := newNVMeFake()
		f.failAlways["SetDatasetProperties"] = errInjected
		f.failAlways["DeleteDataset"] = errInjected // the ZVOL delete is retried within one attempt
		s := NewControllerService(f.client(), NewNodeRegistry(), "")
		if _, err := s.deleteNVMeOFVolume(ctx, f.metaFromProps(nvmeVol, ProtocolNVMeOF, nvmeZvol)); err == nil || f.zvolDeleted {
			t.Fatalf("attempt 1: err=%v zvolDeleted=%v; want an error and the ZVOL kept", err, f.zvolDeleted)
		}
		delete(f.failAlways, "DeleteDataset")
		reassignNVMe(f, 7, 8)
		if _, err := s.deleteNVMeOFVolume(ctx, f.metaFromProps(nvmeVol, ProtocolNVMeOF, nvmeZvol)); err != nil {
			t.Fatalf("attempt 2: %v", err)
		}
		if _, ok := f.namespaces[8]; !ok {
			t.Fatalf("retry deleted reassigned namespace 8 (deletes: %v)", f.deleted)
		}
		if _, ok := f.subsystems[7]; !ok {
			t.Fatalf("retry deleted reassigned subsystem 7 (deletes: %v)", f.deleted)
		}
		if !f.zvolDeleted {
			t.Fatal("retry did not delete the ZVOL")
		}
	})
}

// If an object cannot be looked up, its ID cannot be checked against the volume. The
// attempt must stop (Unavailable, ZVOL and properties kept) rather than delete blind.
func TestBlockDeleteStopsWhenIdentityLookupFails(t *testing.T) {
	ctx := context.Background()

	t.Run("iscsi", func(t *testing.T) {
		f := newISCSIFake()
		mock := f.client()
		mock.QueryISCSITargetsFunc = func(context.Context, []interface{}) ([]tnsapi.ISCSITarget, error) {
			return nil, errInjected
		}
		s := NewControllerService(mock, NewNodeRegistry(), "")
		_, err := s.deleteISCSIVolume(ctx, f.metaFromProps(iscsiVol, ProtocolISCSI, iscsiZvol))
		if status.Code(err) != codes.Unavailable || len(f.deleted) != 0 || f.zvolDeleted {
			t.Fatalf("err=%v deletes=%v zvolDeleted=%v; want Unavailable and nothing deleted", err, f.deleted, f.zvolDeleted)
		}
	})

	t.Run("nvmeof", func(t *testing.T) {
		f := newNVMeFake()
		mock := f.client()
		mock.QueryNVMeOFNamespaceByIDFunc = func(context.Context, int) (*tnsapi.NVMeOFNamespace, error) {
			return nil, errInjected
		}
		s := NewControllerService(mock, NewNodeRegistry(), "")
		_, err := s.deleteNVMeOFVolume(ctx, f.metaFromProps(nvmeVol, ProtocolNVMeOF, nvmeZvol))
		if status.Code(err) != codes.Unavailable || len(f.deleted) != 0 || f.zvolDeleted {
			t.Fatalf("err=%v deletes=%v zvolDeleted=%v; want Unavailable and nothing deleted", err, f.deleted, f.zvolDeleted)
		}
	})
}
