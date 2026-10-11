package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fenio/tns-csi/pkg/tnsapi"
)

// Importing one volume looked its share/extent up by listing every share or extent on
// the system. Both lookups now ask TrueNAS for the exact path/disk.
func TestImportLooksUpExactShareAndExtent(t *testing.T) {
	t.Run("nfs share by mountpoint", func(t *testing.T) {
		var asked string
		m := &mockClient{
			QueryAllNFSSharesFunc: func(context.Context, string) ([]tnsapi.NFSShare, error) {
				t.Error("import listed every NFS share")
				return nil, nil
			},
			QueryNFSShareFunc: func(_ context.Context, path string) ([]tnsapi.NFSShare, error) {
				asked = path
				return []tnsapi.NFSShare{{ID: 3, Path: path}}, nil
			},
		}
		ds := &tnsapi.Dataset{ID: "tank/legacy", Mountpoint: "/mnt/tank/legacy"}
		props, err := handleNFSImport(context.Background(), m, ds, false, true)
		if err != nil {
			t.Fatal(err)
		}
		if asked != "/mnt/tank/legacy" || props[tnsapi.PropertyNFSShareID] != "3" {
			t.Fatalf("asked for %q, props %v", asked, props)
		}
	})

	t.Run("iscsi extent by disk", func(t *testing.T) {
		var got string
		m := &mockClient{
			QueryISCSIExtentsFunc: func(_ context.Context, f []interface{}) ([]tnsapi.ISCSIExtent, error) {
				b, err := json.Marshal(f)
				if err != nil {
					t.Fatal(err)
				}
				got = string(b)
				return nil, nil
			},
		}
		ds := &tnsapi.Dataset{ID: "tank/legacy-zvol", Type: "VOLUME"}
		_, _ = handleISCSIImport(context.Background(), m, ds, true) //nolint:errcheck // only the query is under test
		if want := `[["disk","=","zvol/tank/legacy-zvol"]]`; got != want {
			t.Fatalf("extent filters = %s, want %s", got, want)
		}
	})
}
