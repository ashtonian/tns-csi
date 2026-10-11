package sanity

import (
	"context"
	"testing"

	"github.com/fenio/tns-csi/pkg/tnsapi"
)

// The driver now sends ownership properties with pool.dataset.create (user_properties)
// and pool.snapshot.clone (dataset_properties). The mock must keep them, or csi-sanity
// only ever exercises ownership through the later SetDatasetProperties call.
func TestMockKeepsCreateTimeProperties(t *testing.T) {
	ctx := context.Background()
	props := []tnsapi.UserPropertyKV{{Key: tnsapi.PropertyManagedBy, Value: tnsapi.ManagedByValue}}
	m := NewMockClient()

	if _, err := m.CreateDataset(ctx, tnsapi.DatasetCreateParams{Name: "tank/fs", Type: "FILESYSTEM", UserProperties: props}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateZvol(ctx, tnsapi.ZvolCreateParams{Name: "tank/vol", Volsize: 1 << 30, UserProperties: props}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateSnapshot(ctx, tnsapi.SnapshotCreateParams{Dataset: "tank/fs", Name: "s1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CloneSnapshot(ctx, tnsapi.CloneSnapshotParams{
		Snapshot: "tank/fs@s1", Dataset: "tank/clone",
		DatasetProperties: map[string]string{tnsapi.PropertyManagedBy: tnsapi.ManagedByValue},
	}); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"tank/fs", "tank/vol", "tank/clone"} {
		got, err := m.GetDatasetProperties(ctx, id, []string{tnsapi.PropertyManagedBy})
		if err != nil {
			t.Fatal(err)
		}
		if got[tnsapi.PropertyManagedBy] != tnsapi.ManagedByValue {
			t.Errorf("%s: create-time %s = %q, want %q", id, tnsapi.PropertyManagedBy, got[tnsapi.PropertyManagedBy], tnsapi.ManagedByValue)
		}
	}
}
