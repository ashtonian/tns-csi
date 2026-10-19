package tnsapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"k8s.io/klog/v2"
)

type authLogBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (b *authLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *authLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureAuthLogs(t *testing.T, levels ...string) *authLogBuffer {
	t.Helper()
	state := klog.CaptureState()
	t.Cleanup(func() { klog.Flush(); state.Restore() })
	fs := flag.NewFlagSet("auth-logs", flag.ContinueOnError)
	klog.InitFlags(fs)
	level := "6"
	if len(levels) > 0 {
		level = levels[0]
	}
	for name, value := range map[string]string{"v": level, "logtostderr": "false", "alsologtostderr": "false"} {
		if err := fs.Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	buf := &authLogBuffer{}
	klog.SetOutput(buf)
	return buf
}

func TestModernOnlyServerRequiresUsername(t *testing.T) {
	server := newMockWSServer()
	t.Cleanup(server.Close)
	var calls atomic.Int32
	server.handler = func(conn *websocket.Conn) {
		serveMockRequests(conn, func(req Request) []Response {
			calls.Add(1)
			if req.Method != methodAuthLoginWithAPIKey {
				t.Errorf("unexpected method %s", req.Method)
			}
			return []Response{{ID: req.ID, Error: &Error{Code: -32601, Message: "method not found"}}}
		})
	}
	client, err := NewClient(server.URL(), "fixture-key", false, WithConnectRetryDelays(0))
	if client != nil {
		t.Cleanup(func() { cleanupClient(client) })
	}
	if !errors.Is(err, ErrAuthenticationUsernameRequired) || calls.Load() != 1 {
		t.Fatalf("legacy login: error=%v calls=%d; want rejection and one request", err, calls.Load())
	}
}

func TestNewClientStopsOnTLSVerificationFailure(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(fmt.Sprintf("modern=%t", modern), func(t *testing.T) {
			var handshakes, requests atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				requests.Add(1)
			}))
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			server.TLS = &tls.Config{
				MinVersion: tls.VersionTLS12,
				GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
					handshakes.Add(1)
					return server.TLS, nil
				},
			}
			server.StartTLS()
			t.Cleanup(server.Close)
			opts := []ClientOption{WithConnectRetryDelays(0, 0, 0)}
			if modern {
				opts = append(opts, WithUsername("fixture-user"))
			}
			client, err := NewClient("wss"+strings.TrimPrefix(server.URL, "https"), "fixture-key", false, opts...)
			if client != nil {
				t.Cleanup(func() { cleanupClient(client) })
				t.Fatal("untrusted certificate returned an authenticated client")
			}
			if !errors.Is(err, ErrTLSVerification) || handshakes.Load() != 1 || requests.Load() != 0 {
				t.Fatalf("TLS rejection: error=%v handshakes=%d requests=%d; want verification failure, one handshake and no requests", err, handshakes.Load(), requests.Load())
			}
		})
	}
}

func TestAuthResponseRedaction(t *testing.T) {
	logs := captureAuthLogs(t, "5")
	const sentinel = "AUTH_RESPONSE_TOKEN_SENTINEL"
	server := newMockWSServer()
	t.Cleanup(server.Close)
	server.handler = func(conn *websocket.Conn) {
		ctx := context.Background()
		var req Request
		if err := wsjson.Read(ctx, conn, &req); err != nil {
			return
		}
		payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": true, "token": sentinel})
		if err != nil {
			t.Error(err)
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
			return
		}
		_, _, _ = conn.Read(ctx)
	}
	client, err := NewClient(server.URL(), "fixture-key", false, WithConnectRetryDelays(0))
	if err != nil {
		t.Fatal(err)
	}
	cleanupClient(client)
	klog.Flush()
	if strings.Contains(logs.String(), sentinel) {
		t.Fatal("authentication response token leaked to logs")
	}
}

func assertAuthRequest(t *testing.T, req Request, modern bool) {
	t.Helper()
	method := methodAuthLoginWithAPIKey
	params := []interface{}{"fixture-key"}
	if modern {
		method = methodAuthLoginEx
		params = []interface{}{map[string]any{
			"mechanism": "API_KEY_PLAIN", "username": "fixture-user", "api_key": "fixture-key",
			"login_options": map[string]any{"user_info": false},
		}}
	}
	if req.Method != method || req.JSONRPC != "2.0" || req.ID == "" || !reflect.DeepEqual(req.Params, params) {
		t.Errorf("unexpected authentication request: method=%s id=%s", req.Method, req.ID)
	}
}

func authTestOptions(modern bool) []ClientOption {
	opts := []ClientOption{WithConnectRetryDelays(0)}
	if modern {
		opts = append(opts, WithUsername(" fixture-user\n"))
	}
	return opts
}

func directAuthTestClient(t *testing.T, url string, modern bool) *Client {
	t.Helper()
	c := &Client{url: url, apiKey: "fixture-key", pending: make(map[string]chan *Response),
		closeCh: make(chan struct{}), authGate: make(chan struct{}, 1)}
	for _, opt := range authTestOptions(modern) {
		opt(c)
	}
	if err := c.connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupClient(c) })
	return c
}

func TestAuthenticationResultContract(t *testing.T) {
	logs := captureAuthLogs(t)
	cases := []struct {
		want error
		name string
		raw  string
	}{
		{nil, "success", `{"response_type":"SUCCESS","user_info":null,"authenticator":"LEVEL_1"}`},
		{nil, "newer success", `{"response_type":"SUCCESS","reconnect_token":"TOKEN_SENTINEL","future":1}`},
		{ErrAuthenticationRejected, "rejected", `{"response_type":"AUTH_ERR"}`},
		{ErrAuthenticationExpired, "expired", `{"response_type":"EXPIRED"}`},
		{ErrAuthenticationDenied, "denied", `{"response_type":"DENIED"}`},
		{ErrAuthenticationUnsupported, "otp", `{"response_type":"OTP_REQUIRED","username":"USER_SENTINEL"}`},
		{ErrAuthenticationUnsupported, "redirect", `{"response_type":"REDIRECT","urls":["REDIRECT_SENTINEL"]}`},
		{ErrAuthenticationUnsupported, "scram", `{"response_type":"SCRAM_RESPONSE","rfc_str":"CHALLENGE_SENTINEL"}`},
		{ErrAuthenticationMalformed, "unknown", `{"response_type":"UNKNOWN_SENTINEL"}`},
		{ErrAuthenticationMalformed, "wrong discriminator type", `{"response_type":1}`},
		{ErrAuthenticationMalformed, "missing discriminator", `{}`},
		{ErrAuthenticationMalformed, "bool", `true`},
		{ErrAuthenticationMalformed, "string", `"VALUE_SENTINEL"`},
		{ErrAuthenticationMalformed, "array", `[]`},
		{ErrAuthenticationMalformed, "null", `null`},
		{ErrAuthenticationMalformed, "absent", ``},
	}
	for _, tc := range cases {
		for _, path := range []string{"initial", "session", "direct"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				server := newMockWSServer()
				t.Cleanup(server.Close)
				var authCalls, queries atomic.Int32
				server.handler = func(conn *websocket.Conn) {
					serveMockRequests(conn, func(req Request) []Response {
						if isAuthMethod(req.Method) {
							assertAuthRequest(t, req, true)
							n := authCalls.Add(1)
							raw := tc.raw
							if path == "session" && n == 1 {
								raw = `{"response_type":"SUCCESS"}`
							}
							return []Response{{ID: req.ID, Result: json.RawMessage(raw)}}
						}
						if path != "session" || req.Method != "pool.dataset.query" {
							t.Errorf("unexpected RPC %s", req.Method)
						}
						if queries.Add(1) == 1 {
							return []Response{{ID: req.ID, Error: enotauthenticatedError()}}
						}
						return []Response{{ID: req.ID, Result: json.RawMessage(`[]`)}}
					})
				}
				var err error
				if path == "direct" {
					err = directAuthTestClient(t, server.URL(), true).authenticateDirect()
				} else {
					var c *Client
					c, err = NewClient(server.URL(), "fixture-key", false, authTestOptions(true)...)
					if c != nil {
						t.Cleanup(func() { cleanupClient(c) })
					}
					if path == "session" && err == nil {
						ctx, cancel := context.WithTimeout(context.Background(), time.Second)
						defer cancel()
						err = c.Call(ctx, "pool.dataset.query", nil, nil)
					}
				}
				if !errors.Is(err, tc.want) {
					t.Fatalf("error=%v; want %v", err, tc.want)
				}
				wantCalls := int32(1)
				if path == "session" {
					wantCalls = 2
				}
				if authCalls.Load() != wantCalls {
					t.Errorf("auth calls=%d; want %d", authCalls.Load(), wantCalls)
				}
			})
		}
	}
	klog.Flush()
	for _, sentinel := range []string{"TOKEN_SENTINEL", "USER_SENTINEL", "REDIRECT_SENTINEL", "CHALLENGE_SENTINEL", "UNKNOWN_SENTINEL", "VALUE_SENTINEL"} {
		if strings.Contains(logs.String(), sentinel) {
			t.Fatal("modern auth response leaked to logs")
		}
	}
}

func TestLegacyResultShapes(t *testing.T) {
	for _, raw := range []string{"", "null", `"true"`, `{}`, `[]`, `1`} {
		for _, direct := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/direct=%v", raw, direct), func(t *testing.T) {
				server := newMockWSServer()
				t.Cleanup(server.Close)
				server.handler = func(conn *websocket.Conn) {
					serveMockRequests(conn, func(req Request) []Response {
						assertAuthRequest(t, req, false)
						return []Response{{ID: req.ID, Result: json.RawMessage(raw)}}
					})
				}
				var err error
				if direct {
					err = directAuthTestClient(t, server.URL(), false).authenticateDirect()
				} else {
					_, err = NewClient(server.URL(), "fixture-key", false, authTestOptions(false)...)
				}
				if !errors.Is(err, ErrAuthenticationMalformed) {
					t.Fatalf("error=%v; want malformed", err)
				}
			})
		}
	}
}

func TestAuthStructuredErrorRedaction(t *testing.T) {
	logs := captureAuthLogs(t)
	const sentinel = "ERROR_REASON_EXTRA_TRACE_SENTINEL"
	// ENOTSUP is what Linux middleware sends for an assurance-level mechanism
	// rejection; EOPNOTSUPP is the same errno under its other name.
	names := []string{"ENOMETHOD", "EACCES", "ENOTAUTHENTICATED", "ENOTSUP", "EOPNOTSUPP", "UNKNOWN"}
	for _, modern := range []bool{false, true} {
		for _, path := range []string{"initial", "session", "direct"} {
			for _, name := range names {
				t.Run(fmt.Sprintf("%s/modern=%v/%s", name, modern, path), func(t *testing.T) {
					server := newMockWSServer()
					t.Cleanup(server.Close)
					var authCalls, queries atomic.Int32
					server.handler = func(conn *websocket.Conn) {
						serveMockRequests(conn, func(req Request) []Response {
							if !isAuthMethod(req.Method) {
								if path != "session" || req.Method != "pool.dataset.query" || queries.Add(1) != 1 {
									t.Errorf("unexpected RPC %s", req.Method)
								}
								return []Response{{ID: req.ID, Error: enotauthenticatedError()}}
							}
							assertAuthRequest(t, req, modern)
							if authCalls.Add(1) == 1 && path == "session" {
								success := `true`
								if modern {
									success = `{"response_type":"SUCCESS"}`
								}
								return []Response{{ID: req.ID, Result: json.RawMessage(success)}}
							}
							return []Response{{ID: req.ID, Error: &Error{Code: -32001, Message: sentinel,
								Data: &ErrorData{ErrorName: name, Reason: sentinel, Extra: sentinel, Trace: &ErrorTrace{Repr: sentinel}}}}}
						})
					}
					var err error
					switch path {
					case "direct":
						err = directAuthTestClient(t, server.URL(), modern).authenticateDirect()
					case "initial":
						_, err = NewClient(server.URL(), "fixture-key", false, authTestOptions(modern)...)
					case "session":
						c, newErr := NewClient(server.URL(), "fixture-key", false, authTestOptions(modern)...)
						if newErr != nil {
							t.Fatal(newErr)
						}
						t.Cleanup(func() { cleanupClient(c) })
						ctx, cancel := context.WithTimeout(context.Background(), time.Second)
						defer cancel()
						err = c.Call(ctx, "pool.dataset.query", nil, nil)
					}
					want := ErrAuthenticationFailed
					switch name {
					case "ENOMETHOD":
						want = ErrAuthenticationUsernameRequired
						if modern {
							want = ErrAuthenticationMethodUnavailable
						}
					case "EACCES":
						want = ErrAuthenticationDenied
					case "ENOTAUTHENTICATED":
						want = ErrAuthenticationRejected
					case "ENOTSUP", "EOPNOTSUPP":
						want = ErrAuthenticationMechanismDisallowed
					}
					wantCalls := int32(1)
					if path == "session" {
						wantCalls = 2
					}
					if !errors.Is(err, want) || authCalls.Load() != wantCalls {
						t.Fatalf("error=%v auth calls=%d; want %v and %d", err, authCalls.Load(), want, wantCalls)
					}
					for current := err; current != nil; current = errors.Unwrap(current) {
						if strings.Contains(fmt.Sprintf("%+v", current), sentinel) {
							t.Fatal("server detail retained in error chain")
						}
					}
					var apiErr *Error
					if errors.As(err, &apiErr) {
						t.Fatal("raw server auth error retained")
					}
				})
			}
		}
	}
	klog.Flush()
	if strings.Contains(logs.String(), sentinel) {
		t.Fatal("server auth error leaked to logs")
	}
}

func TestDirectAuthCorrelationAndPeerClose(t *testing.T) {
	logs := captureAuthLogs(t)
	const sentinel = "PEER_ID_PAYLOAD_CLOSE_SENTINEL"
	for _, kind := range []string{"wrong-success", "wrong-error", "event", "malformed", "close"} {
		t.Run(kind, func(t *testing.T) {
			server := newMockWSServer()
			t.Cleanup(server.Close)
			server.handler = func(conn *websocket.Conn) {
				var req Request
				ctx := context.Background()
				if err := wsjson.Read(ctx, conn, &req); err != nil {
					return
				}
				switch kind {
				case "wrong-success":
					_ = wsjson.Write(ctx, conn, Response{ID: sentinel, Result: json.RawMessage(`{"response_type":"SUCCESS"}`)})
				case "wrong-error":
					_ = wsjson.Write(ctx, conn, Response{ID: sentinel, Error: &Error{Message: sentinel}})
				case "event":
					_ = conn.Write(ctx, websocket.MessageText, []byte(`{"method":"`+sentinel+`"}`))
				case "malformed":
					_ = conn.Write(ctx, websocket.MessageText, []byte(`{"`+sentinel))
				case "close":
					_ = conn.Close(websocket.StatusPolicyViolation, sentinel)
				}
			}
			err := directAuthTestClient(t, server.URL(), true).authenticateDirect()
			want := ErrResponseIDMismatch
			if kind == "malformed" {
				want = ErrAuthenticationMalformed
			}
			if kind == "close" {
				want = ErrConnectionClosed
			}
			if !errors.Is(err, want) || strings.Contains(fmt.Sprintf("%+v", err), sentinel) {
				t.Fatalf("error=%v; want safe %v", err, want)
			}
		})
	}
	c := &Client{pending: make(map[string]chan *Response)}
	c.processResponse([]byte(`{"id":"` + sentinel + `","result":"` + sentinel + `"}`))
	c.processResponse([]byte(`{"` + sentinel))
	klog.Flush()
	if strings.Contains(logs.String(), sentinel) {
		t.Fatal("peer-controlled payload leaked to logs")
	}
}

func TestModernConcurrentSessionRecovery(t *testing.T) {
	const callers = 8
	server := newMockWSServer()
	t.Cleanup(server.Close)
	state := &concurrentReauthState{callerCount: callers}
	server.handler = func(conn *websocket.Conn) {
		serveMockRequests(conn, func(req Request) []Response {
			auth := isAuthMethod(req.Method)
			if auth {
				assertAuthRequest(t, req, true)
				req.Method = methodAuthLoginWithAPIKey
			}
			responses := state.handle(req)
			for i := range responses {
				if (auth || responses[i].ID != req.ID) && responses[i].Error == nil {
					responses[i].Result = json.RawMessage(`{"response_type":"SUCCESS"}`)
				}
			}
			return responses
		})
	}
	c, err := NewClient(server.URL(), "fixture-key", false, authTestOptions(true)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupClient(c) })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, callers)
	for range callers {
		go func() { done <- c.Call(ctx, "test.method", nil, nil) }()
	}
	for range callers {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.authCalls != 2 || state.dataCalls != callers*2 {
		t.Fatalf("auth=%d data=%d", state.authCalls, state.dataCalls)
	}
}

func TestModernAuthDoesNotWaitOnOwnReconnect(t *testing.T) {
	server := newMockWSServer()
	t.Cleanup(server.Close)
	server.handler = func(conn *websocket.Conn) {
		serveMockRequests(conn, func(req Request) []Response {
			assertAuthRequest(t, req, true)
			return []Response{{ID: req.ID, Result: json.RawMessage(`{"response_type":"SUCCESS"}`)}}
		})
	}
	c, err := NewClient(server.URL(), "fixture-key", false, authTestOptions(true)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupClient(c) })
	if !c.beginReconnect() {
		t.Fatal("unexpected reconnect")
	}
	defer c.endReconnect()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.authenticate(ctx); !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("error=%v; want immediate connection failure", err)
	}
}

func TestModernInitialRetryPreservesOptions(t *testing.T) {
	server := newMockWSServer()
	t.Cleanup(server.Close)
	var requests atomic.Int32
	server.handler = func(conn *websocket.Conn) {
		serveMockRequests(conn, func(req Request) []Response {
			assertAuthRequest(t, req, true)
			if requests.Add(1) == 1 {
				_ = conn.CloseNow()
				return nil
			}
			return []Response{{ID: req.ID, Result: json.RawMessage(`{"response_type":"SUCCESS"}`)}}
		})
	}
	c, err := NewClient(server.URL(), "fixture-key", false, WithUsername("fixture-user"), WithReadLimit(32768), WithConnectRetryDelays(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupClient(c) })
	if c.readLimit != 32768 || requests.Load() != 2 {
		t.Fatalf("read limit=%d auth requests=%d", c.readLimit, requests.Load())
	}
}

func TestRPCPermissionDoesNotReauthenticate(t *testing.T) {
	for _, modern := range []bool{false, true} {
		for _, nested := range []bool{false, true} {
			t.Run(fmt.Sprintf("modern=%t/nested=%t", modern, nested), func(t *testing.T) {
				server := newMockWSServer()
				t.Cleanup(server.Close)
				var auth, denied, allowed atomic.Int32
				server.handler = func(conn *websocket.Conn) {
					serveMockRequests(conn, func(req Request) []Response {
						if isAuthMethod(req.Method) {
							auth.Add(1)
							assertAuthRequest(t, req, modern)
							result := `true`
							if modern {
								result = `{"response_type":"SUCCESS"}`
							}
							return []Response{{ID: req.ID, Result: json.RawMessage(result)}}
						}
						if req.Method == "system.product_type" {
							allowed.Add(1)
							return []Response{{ID: req.ID, Result: json.RawMessage(`"SCALE"`)}}
						}
						if req.Method != "system.advanced.config" {
							t.Errorf("unexpected RPC %s", req.Method)
						}
						denied.Add(1)
						apiErr := &Error{ErrorName: "EACCES", ErrorCode: 13, Reason: "query requires a different role"}
						if nested {
							apiErr = &Error{Code: -32001, Message: "CallError", Data: &ErrorData{ErrorName: "EACCES", Error: 13, Reason: "query requires a different role"}}
						}
						return []Response{{ID: req.ID, Error: apiErr}}
					})
				}
				c, err := NewClient(server.URL(), "fixture-key", false, authTestOptions(modern)...)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { cleanupClient(c) })
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				var wg sync.WaitGroup
				for range 8 {
					wg.Go(func() {
						err := c.Call(ctx, "system.advanced.config", nil, nil)
						var apiErr *Error
						if !errors.As(err, &apiErr) {
							t.Errorf("error=%v; want RPC permission error", err)
							return
						}
						if nested {
							if apiErr.Data == nil || apiErr.Data.ErrorName != "EACCES" || apiErr.Data.Error != 13 {
								t.Errorf("nested permission error lost: %v", err)
							}
						} else if apiErr.ErrorName != "EACCES" || apiErr.ErrorCode != 13 {
							t.Errorf("top-level permission error lost: %v", err)
						}
						if errors.Is(err, ErrAuthenticationRejected) || errors.Is(err, ErrAuthenticationDenied) {
							t.Errorf("RPC permission error classified as login rejection: %v", err)
						}
					})
				}
				wg.Wait()
				var product string
				if err := c.Call(ctx, "system.product_type", nil, &product); err != nil || product != "SCALE" {
					t.Fatalf("session unusable after permission denial: result=%q error=%v", product, err)
				}
				if auth.Load() != 1 || denied.Load() != 8 || allowed.Load() != 1 {
					t.Fatalf("auth=%d denied=%d allowed=%d; want one login and no RPC replay", auth.Load(), denied.Load(), allowed.Load())
				}
			})
		}
	}
}

func TestPermanentSessionAuthFailureStopsLaterCallers(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(fmt.Sprintf("modern=%v", modern), func(t *testing.T) {
			server := newMockWSServer()
			t.Cleanup(server.Close)
			var authCalls, dataCalls atomic.Int32
			server.handler = func(conn *websocket.Conn) {
				serveMockRequests(conn, func(req Request) []Response {
					if isAuthMethod(req.Method) {
						assertAuthRequest(t, req, modern)
						if authCalls.Add(1) > 1 {
							return []Response{{ID: req.ID, Error: &Error{ErrorName: "EACCES"}}}
						}
						result := `true`
						if modern {
							result = `{"response_type":"SUCCESS"}`
						}
						return []Response{{ID: req.ID, Result: json.RawMessage(result)}}
					}
					dataCalls.Add(1)
					return []Response{{ID: req.ID, Error: enotauthenticatedError()}}
				})
			}
			c, err := NewClient(server.URL(), "fixture-key", false, authTestOptions(modern)...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cleanupClient(c) })
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			for range 3 {
				if err := c.Call(ctx, "pool.dataset.query", nil, nil); !errors.Is(err, ErrAuthenticationDenied) {
					t.Fatalf("error=%v", err)
				}
			}
			if authCalls.Load() != 2 || dataCalls.Load() != 1 {
				t.Fatalf("auth=%d data=%d; want one terminal recovery", authCalls.Load(), dataCalls.Load())
			}
		})
	}
}

func TestModernCancellationDoesNotPoisonNextRecovery(t *testing.T) {
	server := newMockWSServer()
	t.Cleanup(server.Close)
	blocked := make(chan struct{})
	var auth atomic.Int32
	server.handler = func(conn *websocket.Conn) {
		serveMockRequests(conn, func(req Request) []Response {
			if isAuthMethod(req.Method) {
				assertAuthRequest(t, req, true)
				if auth.Add(1) == 2 {
					close(blocked)
					return nil
				}
				return []Response{{ID: req.ID, Result: json.RawMessage(`{"response_type":"SUCCESS"}`)}}
			}
			if auth.Load() < 3 {
				return []Response{{ID: req.ID, Error: enotauthenticatedError()}}
			}
			return []Response{{ID: req.ID, Result: json.RawMessage(`[]`)}}
		})
	}
	// Cancellation can close the socket while a write finishes; allow normal
	// transport recovery without relying on the production five-second backoff.
	opts := append(authTestOptions(true), func(c *Client) { c.retryInterval = time.Millisecond })
	c, err := NewClient(server.URL(), "fixture-key", false, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupClient(c) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Call(ctx, "pool.dataset.query", nil, nil) }()
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("reauthentication never started")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled caller error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled caller remained blocked")
	}
	nextCtx, nextCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer nextCancel()
	if err := c.Call(nextCtx, "pool.dataset.query", nil, nil); err != nil {
		t.Fatalf("later recovery: %v", err)
	}
	if auth.Load() != 3 {
		t.Fatalf("auth calls=%d; want initial, canceled and fresh recovery", auth.Load())
	}
}

func TestBlankUsernameKeepsLegacyAuthentication(t *testing.T) {
	server := newMockWSServer()
	t.Cleanup(server.Close)
	server.handler = func(conn *websocket.Conn) {
		serveMockRequests(conn, func(req Request) []Response {
			assertAuthRequest(t, req, false)
			return []Response{{ID: req.ID, Result: json.RawMessage(`true`)}}
		})
	}
	for _, username := range []string{"", " \n\t"} {
		c, err := NewClient(server.URL(), "fixture-key", false, WithUsername(username), WithConnectRetryDelays(0))
		if err != nil {
			t.Fatal(err)
		}
		cleanupClient(c)
	}
}

func TestModernSocketReconnectAndUncertainMutations(t *testing.T) {
	for _, method := range []string{"pool.dataset.query", "pool.dataset.create", "nvmet.namespace.create", "pool.snapshot.create", "pool.snapshot.clone", "future.mutation"} {
		for _, reject := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reject=%v", method, reject), func(t *testing.T) {
				server := newMockWSServer()
				t.Cleanup(server.Close)
				var auth, data atomic.Int32
				server.handler = func(conn *websocket.Conn) {
					serveMockRequests(conn, func(req Request) []Response {
						if isAuthMethod(req.Method) {
							assertAuthRequest(t, req, true)
							if auth.Add(1) > 1 && reject {
								return []Response{{ID: req.ID, Result: json.RawMessage(`{"response_type":"DENIED"}`)}}
							}
							return []Response{{ID: req.ID, Result: json.RawMessage(`{"response_type":"SUCCESS"}`)}}
						}
						if req.Method != method {
							t.Errorf("unexpected RPC %s", req.Method)
						}
						if data.Add(1) == 1 {
							_ = conn.CloseNow()
							return nil
						}
						return []Response{{ID: req.ID, Result: json.RawMessage(`[]`)}}
					})
				}
				opts := append(authTestOptions(true), func(c *Client) { c.retryInterval = time.Millisecond; c.reinitializeDelay = time.Millisecond })
				c, err := NewClient(server.URL(), "fixture-key", false, opts...)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { cleanupClient(c) })
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				err = c.Call(ctx, method, nil, nil)
				if method != "pool.dataset.query" {
					if !errors.Is(err, ErrUncertainOutcome) || data.Load() != 1 {
						t.Fatalf("mutation: error=%v count=%d", err, data.Load())
					}
				} else if !reject && (err != nil || data.Load() != 2) {
					t.Fatalf("query: error=%v count=%d", err, data.Load())
				}
				if reject {
					select {
					case <-c.closeCh:
					case <-ctx.Done():
						t.Fatal("terminal authentication did not stop read loop")
					}
					if err := c.Call(ctx, method, nil, nil); !errors.Is(err, ErrAuthenticationDenied) {
						t.Fatalf("new caller error=%v", err)
					}
					if auth.Load() != 2 {
						t.Fatalf("auth attempts=%d; want initial + one rejection", auth.Load())
					}
				}
			})
		}
	}
}

func TestPermanentReinitializationStops(t *testing.T) {
	server := newMockWSServer()
	t.Cleanup(server.Close)
	server.handler = func(conn *websocket.Conn) {
		serveMockRequests(conn, func(req Request) []Response {
			assertAuthRequest(t, req, true)
			return []Response{{ID: req.ID, Result: json.RawMessage(`{"response_type":"EXPIRED"}`)}}
		})
	}
	c := &Client{url: server.URL(), apiKey: "fixture-key", pending: make(map[string]chan *Response),
		closeCh: make(chan struct{}), authGate: make(chan struct{}, 1), reinitializeDelay: time.Millisecond}
	WithUsername("fixture-user")(c)
	t.Cleanup(func() { cleanupClient(c) })
	if c.reinitializeConnection() {
		t.Fatal("permanent auth failure continued reinitialization")
	}
	if !errors.Is(c.closedError(), ErrAuthenticationExpired) {
		t.Fatalf("terminal error=%v", c.closedError())
	}
	c.cleanupReadLoop()
}

func TestModernRapidSecondSessionLoss(t *testing.T) {
	server := newMockWSServer()
	t.Cleanup(server.Close)
	var authenticated atomic.Bool
	var authCalls atomic.Int32
	server.handler = func(conn *websocket.Conn) {
		serveMockRequests(conn, func(req Request) []Response {
			if isAuthMethod(req.Method) {
				assertAuthRequest(t, req, true)
				authCalls.Add(1)
				authenticated.Store(true)
				return []Response{{ID: req.ID, Result: json.RawMessage(`{"response_type":"SUCCESS"}`)}}
			}
			if !authenticated.Load() {
				return []Response{{ID: req.ID, Error: enotauthenticatedError()}}
			}
			return []Response{{ID: req.ID, Result: json.RawMessage(`[]`)}}
		})
	}
	c, err := NewClient(server.URL(), "fixture-key", false, authTestOptions(true)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupClient(c) })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for range 2 {
		authenticated.Store(false)
		if err := c.Call(ctx, "pool.dataset.query", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if authCalls.Load() != 3 {
		t.Fatalf("auth calls=%d; want initial plus two distinct recoveries", authCalls.Load())
	}
}

func TestReinitializationAfterTransportFailureStopsOnExpiredAuth(t *testing.T) {
	for _, method := range []string{"pool.dataset.query", "pool.dataset.create"} {
		t.Run(method, func(t *testing.T) {
			server := newMockWSServer()
			t.Cleanup(server.Close)
			var connections, dataCalls atomic.Int32
			server.handler = func(conn *websocket.Conn) {
				n := connections.Add(1)
				if n == 2 {
					_ = conn.CloseNow()
					return
				}
				serveMockRequests(conn, func(req Request) []Response {
					if isAuthMethod(req.Method) {
						assertAuthRequest(t, req, true)
						result := `{"response_type":"SUCCESS"}`
						if n >= 3 {
							result = `{"response_type":"EXPIRED"}`
						}
						return []Response{{ID: req.ID, Result: json.RawMessage(result)}}
					}
					dataCalls.Add(1)
					_ = conn.CloseNow()
					return nil
				})
			}
			opts := append(authTestOptions(true), func(c *Client) {
				c.maxRetries = 1
				c.retryInterval = time.Millisecond
				c.reinitializeDelay = time.Millisecond
			})
			c, err := NewClient(server.URL(), "fixture-key", false, opts...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cleanupClient(c) })
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err = c.Call(ctx, method, nil, nil)
			want := ErrAuthenticationExpired
			if method == "pool.dataset.create" {
				want = ErrUncertainOutcome
			}
			if !errors.Is(err, want) {
				t.Fatalf("call error=%v; want %v", err, want)
			}
			select {
			case <-c.closeCh:
			case <-ctx.Done():
				t.Fatal("reinitialization did not terminate")
			}
			if connections.Load() != 3 || dataCalls.Load() != 1 {
				t.Fatalf("connections=%d mutations/queries=%d", connections.Load(), dataCalls.Load())
			}
			if err := c.Call(ctx, method, nil, nil); !errors.Is(err, ErrAuthenticationExpired) {
				t.Fatalf("new caller error=%v", err)
			}
		})
	}
}
