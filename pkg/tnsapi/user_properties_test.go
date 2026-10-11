package tnsapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// pool.dataset.create takes user_properties as [{"key": ..., "value": ...}] (TrueNAS
// 25.10 PoolDatasetCreateUserProperty). Mock-based tests cannot catch a wire-shape
// mismatch, so pin it here.
func TestCreateParamsSerializeUserProperties(t *testing.T) {
	props := UserPropertyList(OwnershipPropertiesV1("pvc-1", ProtocolNFS, DeleteStrategyRetain, "", 1<<30))

	for _, params := range []any{
		DatasetCreateParams{Name: "tank/csi/pvc-1", Type: "FILESYSTEM", UserProperties: props},
		ZvolCreateParams{Name: "tank/csi/pvc-1", Type: "VOLUME", Volsize: 1 << 30, UserProperties: props},
	} {
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `{"key":"tns-csi:managed_by","value":"tns-csi"}`) {
			t.Errorf("serialized params missing managed_by user property: %s", raw)
		}
	}

	// Omitted entirely when unset, so callers that don't set it send what they sent before.
	raw, err := json.Marshal(DatasetCreateParams{Name: "x", Type: "FILESYSTEM"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "user_properties") {
		t.Errorf("empty UserProperties should be omitted: %s", raw)
	}
}

func TestUserPropertyListIsSortedAndComplete(t *testing.T) {
	got := UserPropertyList(map[string]string{"b:2": "y", "a:1": "x"})
	if len(got) != 2 || got[0].Key != "a:1" || got[1].Key != "b:2" {
		t.Fatalf("UserPropertyList() = %+v, want sorted [a:1 b:2]", got)
	}
}
