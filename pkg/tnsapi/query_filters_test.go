package tnsapi

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

// queryRecorder answers every request, records the query-filters of each call per method,
// and returns canned results for the methods it knows.
type queryRecorder struct {
	results map[string]string
	filters map[string][]json.RawMessage
	mu      sync.Mutex
}

func newQueryRecorder(t *testing.T, results map[string]string) (*Client, *queryRecorder) {
	t.Helper()
	server := newMockWSServer()
	t.Cleanup(server.Close)
	rec := &queryRecorder{results: results, filters: map[string][]json.RawMessage{}}
	server.handler = func(conn *websocket.Conn) {
		ctx := context.Background()
		for {
			_, message, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var req Request
			if json.Unmarshal(message, &req) != nil {
				continue
			}
			resp := Response{ID: req.ID, Result: json.RawMessage(`true`)}
			if body, ok := rec.results[req.Method]; ok {
				filters := json.RawMessage(`null`)
				if len(req.Params) > 0 {
					if b, mErr := json.Marshal(req.Params[0]); mErr == nil {
						filters = b
					}
				}
				rec.mu.Lock()
				rec.filters[req.Method] = append(rec.filters[req.Method], filters)
				rec.mu.Unlock()
				resp.Result = json.RawMessage(body)
			}
			b, err := json.Marshal(resp)
			if err != nil || conn.Write(ctx, websocket.MessageText, b) != nil {
				return
			}
		}
	}
	client, err := NewClient(server.URL(), "test-api-key", false)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(func() { cleanupClient(client) })
	return client, rec
}

// calls returns the filters sent with each call of method, as JSON strings.
func (r *queryRecorder) calls(method string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.filters[method]))
	for _, f := range r.filters[method] {
		out = append(out, string(f))
	}
	return out
}

func wantSingleCall(t *testing.T, rec *queryRecorder, method, wantFilters string) {
	t.Helper()
	got := rec.calls(method)
	if len(got) != 1 || got[0] != wantFilters {
		t.Fatalf("%s calls = %v, want exactly one with filters %s", method, got, wantFilters)
	}
}

// Looking up one subsystem by NQN used to download every subsystem and filter in Go,
// and a miss downloaded the whole list again just to log it. The lookup now filters on
// the server, makes one request, and still re-checks the name client-side.
func TestNVMeOFSubsystemByNQNFiltersOnServer(t *testing.T) {
	client, rec := newQueryRecorder(t, map[string]string{
		"nvmet.subsys.query": `[{"id": 1, "name": "nqn.x:pvc-1", "subnqn": "u:nqn.x:pvc-1"},
		                        {"id": 2, "name": "nqn.x:pvc-2", "subnqn": "u:nqn.x:pvc-2"}]`,
	})
	sub, err := client.NVMeOFSubsystemByNQN(context.Background(), "nqn.x:pvc-1")
	if err != nil {
		t.Fatalf("NVMeOFSubsystemByNQN() error = %v", err)
	}
	if sub.ID != 1 {
		t.Fatalf("got subsystem %d, want 1 (client-side re-check must drop non-matching rows)", sub.ID)
	}
	wantSingleCall(t, rec, "nvmet.subsys.query", `[["name","=","nqn.x:pvc-1"]]`)
}

func TestNVMeOFSubsystemByNQNMissMakesOneRequest(t *testing.T) {
	client, rec := newQueryRecorder(t, map[string]string{"nvmet.subsys.query": `[]`})
	if _, err := client.NVMeOFSubsystemByNQN(context.Background(), "nqn.x:gone"); err == nil {
		t.Fatal("NVMeOFSubsystemByNQN() for a missing NQN returned no error")
	}
	wantSingleCall(t, rec, "nvmet.subsys.query", `[["name","=","nqn.x:gone"]]`)
}

func TestFilteredQueriesPassFiltersThrough(t *testing.T) {
	client, rec := newQueryRecorder(t, map[string]string{
		"sharing.nfs.query":     `[]`,
		"sharing.smb.query":     `[]`,
		"nvmet.namespace.query": `[]`,
		"nvmet.subsys.query":    `[]`,
	})
	ctx := context.Background()
	f := []interface{}{[]interface{}{"path", "=", "/mnt/tank/pvc-1"}}
	if _, err := client.QueryNFSShares(ctx, f); err != nil {
		t.Fatal(err)
	}
	if _, err := client.QuerySMBShares(ctx, f); err != nil {
		t.Fatal(err)
	}
	if _, err := client.QueryNVMeOFNamespaces(ctx, []interface{}{[]interface{}{"subsys_id", "=", 7}}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.QueryNVMeOFSubsystems(ctx, []interface{}{[]interface{}{"id", "=", 7}}); err != nil {
		t.Fatal(err)
	}
	wantSingleCall(t, rec, "sharing.nfs.query", `[["path","=","/mnt/tank/pvc-1"]]`)
	wantSingleCall(t, rec, "sharing.smb.query", `[["path","=","/mnt/tank/pvc-1"]]`)
	wantSingleCall(t, rec, "nvmet.namespace.query", `[["subsys_id","=",7]]`)
	wantSingleCall(t, rec, "nvmet.subsys.query", `[["id","=",7]]`)
}
