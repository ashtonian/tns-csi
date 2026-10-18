package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/fenio/tns-csi/pkg/tnsapi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestConnectionUsernamePrecedence(t *testing.T) {
	for _, tc := range []struct{ name, flagURL, flagKey, secretUser, discoveredUser, envUser, want string }{
		{"flags bypass Secrets", "ws://flag", "flag-key", "secret", "discovered", " env ", "env"},
		{"explicit Secret", "", "", " secret ", "discovered", "env", "secret"},
		{"discovered Secret", "", "", "", " discovered ", "env", "discovered"},
		{"blank Secret uses env", "", "", " \n", " \n", " env ", "env"},
		{"no username", "", "", "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := "fixture/credentials"
			lookups := 0
			sources := connectionSources{
				readSecret: func(context.Context, string) (*connectionConfig, error) {
					lookups++
					return &connectionConfig{URL: "ws://fixture", Username: tc.secretUser}, nil
				},
				discover: func(context.Context) *connectionConfig {
					lookups++
					return &connectionConfig{URL: "ws://fixture", APIKey: "fixture-key", Username: tc.discoveredUser}
				},
				getenv: func(name string) string {
					if name == "TRUENAS_USERNAME" {
						return tc.envUser
					}
					return ""
				},
			}
			cfg, err := resolveConnectionConfig(context.Background(), &tc.flagURL, &tc.flagKey, &ref, nil, sources)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Username != tc.want {
				t.Fatalf("username=%q; want %q", cfg.Username, tc.want)
			}
			if tc.flagURL != "" && (lookups != 0 || cfg.URL != tc.flagURL || cfg.APIKey != tc.flagKey) {
				t.Fatal("complete flags did not retain the no-lookup path")
			}
		})
	}
}

func TestSecretUsernameExtractionPaths(t *testing.T) {
	secret := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: "fixture"}, Data: map[string][]byte{
		"url": []byte("ws://fixture"), "api-key": []byte("fixture-key"), "username": []byte(" fixture-user\n"),
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/credentials") {
			_ = json.NewEncoder(w).Encode(secret)
			return
		}
		_ = json.NewEncoder(w).Encode(corev1.SecretList{Items: []corev1.Secret{secret}})
	}))
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	config := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: %s\ncontexts:\n- name: fixture\n  context:\n    cluster: fixture\n    namespace: fixture\ncurrent-context: fixture\n", server.URL)
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	explicit, err := getConfigFromSecret(ctx, "fixture/credentials")
	if err != nil {
		t.Fatal(err)
	}
	discovered := autoDiscoverDriverSecret(ctx)
	for _, cfg := range []*connectionConfig{explicit, discovered} {
		if cfg == nil || cfg.Username != "fixture-user" || cfg.APIKey != "fixture-key" || cfg.URL != "ws://fixture" {
			t.Fatal("Secret parser did not preserve credentials and trim username")
		}
	}
}

func TestConnectToTrueNASPropagatesUsername(t *testing.T) {
	var calls atomic.Int32
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
		calls.Add(1)
		if req.Method != "auth.login_ex" || len(req.Params) != 1 {
			t.Errorf("method=%s params=%d", req.Method, len(req.Params))
		}
		_ = wsjson.Write(r.Context(), conn, tnsapi.Response{ID: req.ID, Result: json.RawMessage(`{"response_type":"SUCCESS"}`)})
		_, _, _ = conn.Read(r.Context())
	}))
	t.Cleanup(server.Close)
	c, err := connectToTrueNAS(context.Background(), &connectionConfig{URL: strings.Replace(server.URL, "http://", "ws://", 1), APIKey: "fixture-key", Username: "fixture-user"})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if calls.Load() != 1 {
		t.Fatalf("auth calls=%d", calls.Load())
	}
}
