package driver

import (
	"context"
	"testing"

	"github.com/fenio/tns-csi/pkg/tnsapi"
)

// Checking whether one dataset exists used an "id starts with" query, which returns the
// dataset's entire subtree; for the detached-snapshots parent that is every detached
// snapshot ever taken. Exact lookups must use an exact ID query.
func TestExactDatasetLookupsDoNotListSubtrees(t *testing.T) {
	newMock := func(t *testing.T) (*MockAPIClientForSnapshots, *[]string) {
		t.Helper()
		var asked []string
		return &MockAPIClientForSnapshots{
			QueryAllDatasetsFunc: func(_ context.Context, prefix string) ([]tnsapi.Dataset, error) {
				t.Errorf("prefix query for %q lists its whole subtree", prefix)
				return nil, nil
			},
			GetDatasetFunc: func(_ context.Context, id string) (*tnsapi.Dataset, error) {
				asked = append(asked, id)
				return &tnsapi.Dataset{ID: id, Name: id}, nil
			},
		}, &asked
	}

	t.Run("queryExactDataset", func(t *testing.T) {
		mock, asked := newMock(t)
		got, err := NewControllerService(mock, NewNodeRegistry(), "").queryExactDataset(context.Background(), "tank/csi/pvc-1")
		if err != nil || len(got) != 1 || got[0].ID != "tank/csi/pvc-1" {
			t.Fatalf("queryExactDataset() = %v, %v", got, err)
		}
		if len(*asked) != 1 {
			t.Fatalf("exact lookups = %v, want one", *asked)
		}
	})

	t.Run("queryExactDataset not found", func(t *testing.T) {
		mock, _ := newMock(t)
		mock.GetDatasetFunc = func(context.Context, string) (*tnsapi.Dataset, error) { return nil, tnsapi.ErrDatasetNotFound }
		got, err := NewControllerService(mock, NewNodeRegistry(), "").queryExactDataset(context.Background(), "tank/csi/gone")
		if err != nil || len(got) != 0 {
			t.Fatalf("queryExactDataset(missing) = %v, %v; want empty, nil", got, err)
		}
	})

	t.Run("detached snapshots parent", func(t *testing.T) {
		mock, asked := newMock(t)
		if err := NewControllerService(mock, NewNodeRegistry(), "").ensureDetachedSnapshotsParentDataset(context.Background(), "tank/csi-detached-snapshots"); err != nil {
			t.Fatal(err)
		}
		if len(*asked) != 1 || (*asked)[0] != "tank/csi-detached-snapshots" {
			t.Fatalf("exact lookups = %v, want the parent only", *asked)
		}
	})
}
