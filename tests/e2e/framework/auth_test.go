package framework

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/fenio/tns-csi/pkg/tnsapi"
)

func TestAuthConfig(t *testing.T) {
	t.Setenv("TRUENAS_HOST", "fixture.example")
	t.Setenv("TRUENAS_API_KEY", "fixture-key")
	t.Setenv("TRUENAS_USERNAME", " fixture-user\n")
	t.Setenv("TRUENAS_SKIP_TLS_VERIFY", "false")
	cfg, err := NewConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TrueNASUsername != "fixture-user" || cfg.TrueNASSkipTLSVerify {
		t.Fatal("username/TLS mode not propagated")
	}
	t.Setenv("TRUENAS_SKIP_TLS_VERIFY", "")
	cfg, err = NewConfig()
	if err != nil || !cfg.TrueNASSkipTLSVerify {
		t.Fatalf("legacy TLS default: %v", err)
	}
	t.Setenv("TRUENAS_SKIP_TLS_VERIFY", "INVALID_TLS_SENTINEL")
	_, err = NewConfig()
	if err == nil || strings.Contains(err.Error(), "INVALID_TLS_SENTINEL") {
		t.Fatalf("unsafe invalid TLS setting: %v", err)
	}
}

func TestVerifierAuthAndTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			t.Errorf("method=%s", req.Method)
		}
		_ = wsjson.Write(r.Context(), conn, tnsapi.Response{ID: req.ID, Result: json.RawMessage(`{"response_type":"SUCCESS"}`)})
		_, _, _ = conn.Read(r.Context())
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "https://")
	verifier, err := newTrueNASVerifier(host, "fixture-key", true, tnsapi.WithUsername("fixture-user"))
	if err != nil {
		t.Fatal(err)
	}
	verifier.Close()
	_, err = newTrueNASVerifier(host, "fixture-key", false, tnsapi.WithUsername("fixture-user"), tnsapi.WithConnectRetryDelays(0))
	if !errors.Is(err, tnsapi.ErrTLSVerification) {
		t.Fatalf("untrusted cert error=%v", err)
	}
}

func TestPrivateHelmCredentials(t *testing.T) {
	const key = "HELM_KEY_SENTINEL"
	path, cleanup, err := createHelmKeyFile(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })
	data, err := os.ReadFile(path)
	if err != nil || string(data) != key {
		t.Fatal("credential file contents incorrect")
	}
	for _, entry := range []struct {
		path string
		mode os.FileMode
	}{{path, 0o600}, {filepath.Dir(path), 0o700}} {
		info, err := os.Stat(entry.path)
		if err != nil || info.Mode().Perm() != entry.mode {
			t.Fatalf("credential permissions: %v", err)
		}
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("credential file retained after cleanup")
	}
}

func TestHelmDeployCredentialsAndCleanup(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "arguments")
	keyPath := filepath.Join(dir, "credential-path")
	script := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$@\" > \"$FIXTURE_ARGS\"\nfor arg in \"$@\"; do\n case \"$arg\" in truenas.apiKey=*) p=${arg#truenas.apiKey=}; printf '%s' \"$p\" > \"$FIXTURE_KEY_PATH\"; test -f \"$p\";; esac\ndone\nprintf 'HELM_OUTPUT_SECRET_SENTINEL'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "helm"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FIXTURE_ARGS", argsPath)
	t.Setenv("FIXTURE_KEY_PATH", keyPath)
	h := NewHelmDeployer(&Config{TrueNASHost: "fixture.example", TrueNASAPIKey: "HELM_KEY_SENTINEL", TrueNASUsername: "fixture-user", TrueNASSkipTLSVerify: false})
	err := h.Deploy("nfs")
	if err == nil || strings.Contains(err.Error(), "HELM_OUTPUT_SECRET_SENTINEL") {
		t.Fatalf("unsafe deployment error=%v", err)
	}
	argv, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), "HELM_KEY_SENTINEL") || !strings.Contains(string(argv), "--set-file") || !strings.Contains(string(argv), "truenas.skipTLSVerify=false") || !strings.Contains(string(argv), "truenas.username=fixture-user") {
		t.Fatal("child argv has unsafe credentials or wrong auth/TLS settings")
	}
	path, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(string(path)); !os.IsNotExist(err) {
		t.Fatal("credential file retained after command failure")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.runHelm(ctx, "upgrade"); err == nil || strings.Contains(err.Error(), "HELM_OUTPUT_SECRET_SENTINEL") {
		t.Fatal("Helm error retained secret-bearing output")
	}
}
