// Phase 19 — realmprovisioner HTTP client ResetMFA branches (RP-9, LLD §16
// OQ-8/F6).
//
// ResetMFA is fail-closed: any transport error or non-2xx response is
// returned to the caller, never swallowed (unlike RevokeUserSessions which
// is AUTH-8 fail-open).
//
// Four branches covered:
//  1. baseURL == "" → no-op, returns nil (dev fallback)
//  2. Transport error (server closed before call)
//  3. Non-2xx response → error containing the status code
//  4. 2xx response → no error
package realmprovisioner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResetMFA_EmptyBaseURL_NoOpReturnsNil(t *testing.T) {
	// Branch 1: baseURL == "" → dev fallback, no-op.
	c := NewHTTPClient("", 0, nil)
	err := c.ResetMFA(context.Background(), uuid.New(), uuid.New())
	assert.NoError(t, err, "empty baseURL must be a no-op returning nil")
}

func TestResetMFA_TransportError_ReturnsError(t *testing.T) {
	// Branch 2: server closed before the request is made → transport error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// Close the server immediately so any connection attempt fails.
	srv.Close()

	c := NewHTTPClient(srv.URL, 0, nil)
	err := c.ResetMFA(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err, "transport error must be surfaced to the caller (fail-closed)")
}

func TestResetMFA_Non2xx_ReturnsError(t *testing.T) {
	// Branch 3: server returns a non-2xx status (e.g. 503 Service Unavailable).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "realm provisioner unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0, nil)
	err := c.ResetMFA(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err, "non-2xx must be surfaced to the caller (fail-closed)")
	assert.Contains(t, err.Error(), "503")
}

func TestResetMFA_Success_2xx_ReturnsNil(t *testing.T) {
	// Branch 4: server returns 200 OK → no error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Contains(t, r.URL.Path, "/mfa-reset")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0, nil)
	err := c.ResetMFA(context.Background(), uuid.New(), uuid.New())
	assert.NoError(t, err, "2xx response must return nil (success)")
}
