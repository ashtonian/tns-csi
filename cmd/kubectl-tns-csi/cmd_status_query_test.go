package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fenio/tns-csi/pkg/tnsapi"
)

// Checking one volume's status listed every NFS share, NVMe-oF subsystem and namespace
// on the system to find a single ID. The checks now look that ID up directly.
func TestVolumeStatusLooksUpByID(t *testing.T) {
	t.Run("nfs", func(t *testing.T) {
		var asked int
		m := &mockClient{
			QueryAllNFSSharesFunc: func(context.Context, string) ([]tnsapi.NFSShare, error) {
				t.Error("status listed every NFS share")
				return nil, nil
			},
			QueryNFSShareByIDFunc: func(_ context.Context, id int) (*tnsapi.NFSShare, error) {
				asked = id
				return &tnsapi.NFSShare{ID: id, Path: "/mnt/tank/pvc-1", Enabled: true}, nil
			},
		}
		props := map[string]tnsapi.UserProperty{tnsapi.PropertyNFSShareID: {Value: "5"}}
		st := &VolumeStatus{}
		if err := checkNFSStatus(context.Background(), m, nil, props, st); err != nil {
			t.Fatal(err)
		}
		if asked != 5 || !st.NFSEnabled || st.NFSSharePath != "/mnt/tank/pvc-1" {
			t.Fatalf("asked share %d, status %+v", asked, st)
		}
	})

	t.Run("nfs share missing", func(t *testing.T) {
		m := &mockClient{QueryNFSShareByIDFunc: func(context.Context, int) (*tnsapi.NFSShare, error) { return nil, nil }}
		props := map[string]tnsapi.UserProperty{tnsapi.PropertyNFSShareID: {Value: "5"}}
		if err := checkNFSStatus(context.Background(), m, nil, props, &VolumeStatus{}); err == nil {
			t.Fatal("missing share reported healthy")
		}
	})

	t.Run("nvmeof", func(t *testing.T) {
		var subsysFilters string
		var askedNS int
		m := &mockClient{
			ListAllNVMeOFSubsystemsFunc: func(context.Context) ([]tnsapi.NVMeOFSubsystem, error) {
				t.Error("status listed every NVMe-oF subsystem")
				return nil, nil
			},
			QueryAllNVMeOFNamespacesFunc: func(context.Context) ([]tnsapi.NVMeOFNamespace, error) {
				t.Error("status listed every NVMe-oF namespace")
				return nil, nil
			},
			QueryNVMeOFSubsystemsFunc: func(_ context.Context, f []interface{}) ([]tnsapi.NVMeOFSubsystem, error) {
				b, err := json.Marshal(f)
				if err != nil {
					t.Fatal(err)
				}
				subsysFilters = string(b)
				return []tnsapi.NVMeOFSubsystem{{ID: 7, NQN: "nqn.x"}}, nil
			},
			QueryNVMeOFNamespaceByIDFunc: func(_ context.Context, id int) (*tnsapi.NVMeOFNamespace, error) {
				askedNS = id
				return &tnsapi.NVMeOFNamespace{ID: id}, nil
			},
		}
		props := map[string]tnsapi.UserProperty{
			tnsapi.PropertyNVMeSubsystemID: {Value: "7"},
			tnsapi.PropertyNVMeNamespaceID: {Value: "8"},
		}
		st := &VolumeStatus{}
		if err := checkNVMeOFStatus(context.Background(), m, nil, props, st); err != nil {
			t.Fatal(err)
		}
		if want := `[["id","=",7]]`; subsysFilters != want || askedNS != 8 || st.NVMeNQN != "nqn.x" {
			t.Fatalf("subsystem filters %s (want %s), namespace %d, status %+v", subsysFilters, want, askedNS, st)
		}
	})
}
