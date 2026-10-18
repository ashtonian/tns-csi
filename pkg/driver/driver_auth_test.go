package driver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/fenio/tns-csi/pkg/tnsapi"
)

func TestNewDriverPropagatesAPIUsername(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		var req tnsapi.Request
		if err := wsjson.Read(r.Context(), conn, &req); err != nil {
			return
		}
		if req.Method != "auth.login_ex" {
			t.Errorf("auth method=%s", req.Method)
		}
		if len(req.Params) == 1 {
			params, ok := req.Params[0].(map[string]interface{})
			if !ok || params["username"] != "fixture-user" || params["api_key"] != "fixture-key" {
				t.Error("auth credentials were not propagated")
			}
		} else {
			t.Error("wrong auth parameter count")
		}
		_ = wsjson.Write(r.Context(), conn, tnsapi.Response{ID: req.ID, Result: json.RawMessage(`{"response_type":"SUCCESS"}`)})
		_, _, _ = conn.Read(r.Context())
	}))
	t.Cleanup(server.Close)
	d, err := NewDriver(Config{APIURL: strings.Replace(server.URL, "http://", "ws://", 1), APIKey: "fixture-key", APIUsername: "fixture-user", TestMode: true})
	if err != nil {
		t.Fatal(err)
	}
	client, ok := d.apiClient.(*tnsapi.Client)
	if !ok {
		t.Fatal("unexpected API client type")
	}
	client.Close()
}
