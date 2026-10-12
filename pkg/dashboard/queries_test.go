package dashboard

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fenio/tns-csi/pkg/tnsapi"
)

// fakeClient implements only what these tests need; any other method panics via the
// nil embedded interface, which makes "must not call X" assertions explicit.
type fakeClient struct {
	tnsapi.ClientInterface
	snapshots      func(filters []interface{}) ([]tnsapi.Snapshot, error)
	nfsShares      func(filters []interface{}) ([]tnsapi.NFSShare, error)
	datasets       []tnsapi.DatasetWithProperties
	findDelay      time.Duration
	findCalls      atomic.Int32
	queriedTargets atomic.Int32
}

func (f *fakeClient) FindDatasetsByProperty(_ context.Context, _, _, _ string) ([]tnsapi.DatasetWithProperties, error) {
	f.findCalls.Add(1)
	time.Sleep(f.findDelay)
	return f.datasets, nil
}

func (f *fakeClient) QuerySnapshots(_ context.Context, filters []interface{}) ([]tnsapi.Snapshot, error) {
	return f.snapshots(filters)
}

func (f *fakeClient) QueryNFSShares(_ context.Context, filters []interface{}) ([]tnsapi.NFSShare, error) {
	return f.nfsShares(filters)
}

func (f *fakeClient) QueryISCSITargets(context.Context, []interface{}) ([]tnsapi.ISCSITarget, error) {
	f.queriedTargets.Add(1)
	return nil, nil
}

func managedDataset(id, volume, protocol string) tnsapi.DatasetWithProperties {
	return tnsapi.DatasetWithProperties{
		Dataset: tnsapi.Dataset{ID: id, Name: id},
		UserProperties: map[string]tnsapi.UserProperty{
			tnsapi.PropertyManagedBy:     {Value: tnsapi.ManagedByValue},
			tnsapi.PropertyCSIVolumeName: {Value: volume},
			tnsapi.PropertyProtocol:      {Value: protocol},
		},
	}
}

func asJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The snapshots tab used to download every ZFS snapshot on the system (including
// periodic snapshots that have nothing to do with tns-csi) and filter in Go.
func TestAttachedSnapshotsQueryOnlyManagedDatasets(t *testing.T) {
	var got string
	f := &fakeClient{
		datasets: []tnsapi.DatasetWithProperties{managedDataset("tank/csi/pvc-1", "pvc-1", "nfs")},
		snapshots: func(filters []interface{}) ([]tnsapi.Snapshot, error) {
			got = asJSON(t, filters)
			return []tnsapi.Snapshot{{Name: "tank/csi/pvc-1@s1", Dataset: "tank/csi/pvc-1"}}, nil
		},
	}
	snaps, err := findAttachedSnapshots(context.Background(), f, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := `[["dataset","in",["tank/csi/pvc-1"]]]`; got != want {
		t.Fatalf("snapshot filters = %s, want %s", got, want)
	}
	if len(snaps) != 1 {
		t.Fatalf("got %d snapshots, want 1", len(snaps))
	}

	t.Run("no managed volumes means no snapshot query", func(t *testing.T) {
		empty := &fakeClient{snapshots: func([]interface{}) ([]tnsapi.Snapshot, error) {
			t.Error("pool.snapshot.query issued with no managed datasets")
			return nil, nil
		}}
		if _, err := findAttachedSnapshots(context.Background(), empty, ""); err != nil {
			t.Fatal(err)
		}
	})
}

// The health check bulk-loads each protocol's objects once; protocols with no managed
// volumes must not be queried at all.
func TestHealthQueriesOnlyProtocolsInUse(t *testing.T) {
	f := &fakeClient{
		datasets: []tnsapi.DatasetWithProperties{managedDataset("tank/csi/pvc-1", "pvc-1", "iscsi")},
	}
	if _, err := CheckVolumeHealth(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if f.queriedTargets.Load() != 1 {
		t.Fatalf("iSCSI targets queried %d times, want 1", f.queriedTargets.Load())
	}
	// NFS/SMB/NVMe-oF methods are not implemented on fakeClient: reaching them panics.
}

func TestUnmanagedVolumesQueryOnlySharesUnderThePool(t *testing.T) {
	var got string
	f := &fakeClient{nfsShares: func(filters []interface{}) ([]tnsapi.NFSShare, error) {
		got = asJSON(t, filters)
		return nil, nil
	}}
	c := &unmanagedFake{fakeClient: f}
	if _, err := FindUnmanagedVolumes(context.Background(), c, "tank", false, ""); err != nil {
		t.Fatal(err)
	}
	if want := `[["path","^","/mnt/tank/"]]`; got != want {
		t.Fatalf("share filters = %s, want %s", got, want)
	}
}

type unmanagedFake struct{ *fakeClient }

func (u *unmanagedFake) QueryAllDatasets(context.Context, string) ([]tnsapi.Dataset, error) {
	return nil, nil
}

func (u *unmanagedFake) FindManagedDatasets(context.Context, string) ([]tnsapi.DatasetWithProperties, error) {
	return nil, nil
}

// One dashboard refresh fires several partials in parallel, and each used to run the
// same managed-dataset query. The cached client collapses them into one request.
func TestCachedClientDedupesPropertyLookups(t *testing.T) {
	f := &fakeClient{findDelay: 20 * time.Millisecond}
	c := newCachedClient(f, time.Minute)
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.FindDatasetsByProperty(context.Background(), "", tnsapi.PropertyManagedBy, tnsapi.ManagedByValue); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, err := c.FindDatasetsByProperty(context.Background(), "", tnsapi.PropertyManagedBy, tnsapi.ManagedByValue); err != nil {
		t.Fatal(err)
	}
	if n := f.findCalls.Load(); n != 1 {
		t.Fatalf("underlying lookups = %d, want 1", n)
	}
	if _, err := c.FindDatasetsByProperty(context.Background(), "", tnsapi.PropertyDetachedSnapshot, "true"); err != nil {
		t.Fatal(err)
	}
	if n := f.findCalls.Load(); n != 2 {
		t.Fatalf("a different property must not share the cache entry (lookups = %d, want 2)", n)
	}

	under := &fakeClient{}
	expired := newCachedClient(under, time.Nanosecond)
	for range 2 {
		if _, err := expired.FindDatasetsByProperty(context.Background(), "", "p", "v"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	if n := under.findCalls.Load(); n != 2 {
		t.Fatalf("lookups after expiry = %d, want 2", n)
	}
}
