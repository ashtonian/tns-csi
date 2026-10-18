package tnsapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"strings"
)

const methodAuthLoginEx = "auth.login_ex"

// Authentication failures contain no server-provided text or credentials.
var (
	ErrAuthenticationExpired           = errors.New("authentication credential expired: renew the API key")
	ErrAuthenticationDenied            = errors.New("authentication denied: check account API access and server policy")
	ErrAuthenticationUnsupported       = errors.New("authentication requires an unsupported challenge or redirect")
	ErrAuthenticationMalformed         = errors.New("invalid authentication response")
	ErrAuthenticationUsernameRequired  = errors.New("legacy authentication unavailable: configure the API key owner's username")
	ErrAuthenticationMethodUnavailable = errors.New("modern authentication unavailable: check the server version and API endpoint")
	ErrAuthenticationFailed            = errors.New("authentication failed: check server authentication configuration")
	ErrTLSVerification                 = errors.New("TLS certificate verification failed: check the hostname and trusted CA")
)

// WithUsername selects login_ex using the API key owner's username. An empty
// username retains legacy API-key-only authentication.
func WithUsername(username string) ClientOption {
	return func(c *Client) { c.username = strings.TrimSpace(username) }
}

func isAuthMethod(method string) bool {
	return method == methodAuthLoginWithAPIKey || method == methodAuthLoginEx
}

func (c *Client) authenticationRequest() (method string, params []interface{}) {
	if c.username == "" {
		return methodAuthLoginWithAPIKey, []interface{}{c.apiKey}
	}
	return methodAuthLoginEx, []interface{}{map[string]any{
		"mechanism": "API_KEY_PLAIN", "username": c.username, "api_key": c.apiKey,
		"login_options": map[string]bool{"user_info": false},
	}}
}

func validateAuthenticationResult(method string, raw json.RawMessage) error {
	if method == methodAuthLoginWithAPIKey {
		switch string(bytes.TrimSpace(raw)) {
		case "true":
			return nil
		case "false":
			return ErrAuthenticationRejected
		default:
			return ErrAuthenticationMalformed
		}
	}
	var result struct {
		ResponseType string `json:"response_type"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return ErrAuthenticationMalformed
	}
	switch result.ResponseType {
	case "SUCCESS":
		return nil
	case "AUTH_ERR":
		return ErrAuthenticationRejected
	case "EXPIRED":
		return ErrAuthenticationExpired
	case "DENIED":
		return ErrAuthenticationDenied
	case "OTP_REQUIRED", "REDIRECT", "SCRAM_RESPONSE":
		return ErrAuthenticationUnsupported
	default:
		return ErrAuthenticationMalformed
	}
}

func isAuthenticationError(err error) bool {
	for _, failure := range []error{
		ErrAuthenticationRejected, ErrAuthenticationExpired, ErrAuthenticationDenied,
		ErrAuthenticationUnsupported, ErrAuthenticationMalformed,
		ErrAuthenticationUsernameRequired, ErrAuthenticationMethodUnavailable,
		ErrAuthenticationFailed, ErrResponseIDMismatch,
	} {
		if errors.Is(err, failure) {
			return true
		}
	}
	return false
}

func sanitizeAuthenticationError(method string, err error) error {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		name := apiErr.ErrorName
		if apiErr.Data != nil && name == "" {
			name = apiErr.Data.ErrorName
		}
		switch {
		case apiErr.Code == -32601 || name == "ENOMETHOD":
			if method == methodAuthLoginWithAPIKey {
				return ErrAuthenticationUsernameRequired
			}
			return ErrAuthenticationMethodUnavailable
		case name == "EACCES" || name == "EPERM":
			return ErrAuthenticationDenied
		case apiErr.Code == 401 || name == errNameNotAuthenticated:
			return ErrAuthenticationRejected
		default:
			return ErrAuthenticationFailed
		}
	}
	if isAuthenticationError(err) {
		return err
	}
	return sanitizeConnectionError(err)
}

// Retaining an arbitrary transport error can retain a peer's close reason.
func sanitizeConnectionError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, ErrClientClosed):
		return ErrClientClosed
	case errors.Is(err, errResponseLost):
		return errors.Join(ErrConnectionClosed, errResponseLost)
	default:
		var certErr *tls.CertificateVerificationError
		if errors.As(err, &certErr) {
			return ErrTLSVerification
		}
		return ErrConnectionClosed
	}
}

func connectionErrorClass(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrTLSVerification):
		return "tls-verification"
	case isAuthenticationError(err):
		return "authentication"
	default:
		return "connection"
	}
}
