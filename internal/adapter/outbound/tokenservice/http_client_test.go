// Package tokenservice — unit tests for the IsServiceAccount HTTP client.
//
// Module:   iam-org-membership
// Feature:  Token Service client — IsServiceAccount (TS-5, AUTH-9).
//
//	Called synchronously on the membership-create (P-6/I-3) and
//	role-grant (P-10/P-28) paths. Fails open (caller degrades to
//	allowing the operation on transport/non-2xx).
//
// File:     internal/adapter/outbound/tokenservice/http_client.go
package tokenservice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── IsServiceAccount happy path ───────────────────────────────────────────

func TestIsServiceAccount_Happy_Found(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/internal/tenants/"+tenantID.String()+"/service-accounts", r.URL.Path)

		// Internal headers must be set (mesh trust boundary)
		assert.Equal(t, "iam-system", r.Header.Get("x-user-id"))
		assert.Equal(t, "iam-system", r.Header.Get("x-tenant-roles"))
		assert.Equal(t, tenantID.String(), r.Header.Get("x-tenant-id"))

		assert.Equal(t, userID.String(), r.URL.Query().Get("principal_sub"))

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"principal_id": uuid.New(),
		})
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	got, err := c.IsServiceAccount(context.Background(), tenantID, userID)
	require.NoError(t, err)
	assert.True(t, got)
}

func TestIsServiceAccount_Happy_NotFound(t *testing.T) {
	// 404 principal_not_found is the expected, common answer — almost every
	// subject checked is a real human user, not the tenant's automation
	// principal.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"principal_not_found"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	got, err := c.IsServiceAccount(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.False(t, got, "false means not a service account — the common case")
}

// ── Error paths ───────────────────────────────────────────────────────────

func TestIsServiceAccount_EmptyBaseURL_Errors(t *testing.T) {
	// New() with no TOKEN_SERVICE_BASE_URL env var leaves baseURL empty.
	c := New(nil)
	_, err := c.IsServiceAccount(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "baseURL not configured")
}

func TestIsServiceAccount_TransportError_Errors(t *testing.T) {
	// Port 1 is never open — connection refused gives a transport error.
	c := NewHTTPClient("http://127.0.0.1:1", 200*time.Millisecond, nil)
	_, err := c.IsServiceAccount(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
}

func TestIsServiceAccount_Non2xx_ClientError_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"insufficient_role"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	_, err := c.IsServiceAccount(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
}

func TestIsServiceAccount_Non2xx_ServerError_Errors(t *testing.T) {
	// 5xx also triggers the IncDependencyError("5xx") metrics path + returns error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"internal_error"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	_, err := c.IsServiceAccount(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestIsServiceAccount_MalformedBody_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{not-json`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	_, err := c.IsServiceAccount(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode response")
}

// ── Constructor behaviours ─────────────────────────────────────────────────

func TestNewHTTPClient_NilLogger_UsesDefault(t *testing.T) {
	// nil logger must not panic — warn() is a no-op.
	c := NewHTTPClient("http://tokenservice.internal", 5*time.Second, nil)
	assert.NotNil(t, c)
}

func TestNewHTTPClient_ZeroTimeout_UsesDefaultTimeout(t *testing.T) {
	c := NewHTTPClient("http://tokenservice.internal", 0, nil)
	assert.NotNil(t, c)
	assert.Equal(t, 1000*time.Millisecond, c.client.Timeout)
}

func TestNewHTTPClient_ExplicitURL_ReturnsFunctionalClient(t *testing.T) {
	c := NewHTTPClient("http://tokenservice.internal", 5*time.Second, nil)
	assert.NotNil(t, c)
}

// ── setInternalHeaders ────────────────────────────────────────────────────

func TestSetInternalHeaders_SetsExpectedHeaders(t *testing.T) {
	tenantID := uuid.New()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com", nil)
	c := NewHTTPClient("http://tokenservice.internal", 5*time.Second, nil)
	c.setInternalHeaders(req, tenantID)

	assert.Equal(t, "iam-system", req.Header.Get("x-user-id"))
	assert.Equal(t, tenantID.String(), req.Header.Get("x-tenant-id"))
	assert.Equal(t, "iam-system", req.Header.Get("x-tenant-roles"))
}
