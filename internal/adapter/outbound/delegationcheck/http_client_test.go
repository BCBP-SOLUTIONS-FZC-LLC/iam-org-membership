// Package delegationcheck — unit tests for the DeptDelegate HTTP client.
//
// Module:   iam-org-membership
// Feature:  Delegation Service client — DeptDelegate (DLG-I3).
//
//	Called synchronously on the admin dept-membership Assign/Remove
//	path (§8.8.4 precision lookup). Fails open (caller degrades to
//	tenant-wide impact scoping on transport/non-2xx).
//
// File:     internal/adapter/outbound/delegationcheck/http_client.go
package delegationcheck

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

// ── DeptDelegate happy path ───────────────────────────────────────────────

func TestDeptDelegate_Happy_Found(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()
	deptID := uuid.New()
	delegationID := uuid.New()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/internal/delegations/dept-delegate", r.URL.Path)

		// Internal headers must be set (IAPI-2/mesh trust boundary)
		assert.Equal(t, "iam-system", r.Header.Get("x-user-id"))
		assert.Equal(t, "iam-system", r.Header.Get("x-tenant-roles"))
		assert.Equal(t, tenantID.String(), r.Header.Get("x-tenant-id"))

		// Query params
		assert.Equal(t, tenantID.String(), r.URL.Query().Get("tenant_id"))
		assert.Equal(t, userID.String(), r.URL.Query().Get("user_id"))
		assert.Equal(t, deptID.String(), r.URL.Query().Get("dept_id"))

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"found":         true,
			"delegation_id": delegationID,
			"delegator_id":  userID,
			"delegate_id":   uuid.New(),
		})
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	got, err := c.DeptDelegate(context.Background(), tenantID, userID, deptID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, delegationID, *got)
}

func TestDeptDelegate_Happy_NotFound(t *testing.T) {
	// `{"found": false}` is the valid "no active delegation" answer (DLG-I3).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"found": false,
		})
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	got, err := c.DeptDelegate(context.Background(), uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Nil(t, got, "nil means no active dept delegation — caller degrades gracefully")
}

func TestDeptDelegate_Happy_FoundButNilDelegationID_ReturnsNil(t *testing.T) {
	// Edge: found=true but delegation_id omitted (shouldn't happen in practice,
	// but the client must handle it defensively — returns nil, nil).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"found": true,
			// delegation_id deliberately omitted → DelegationID == nil
		})
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	got, err := c.DeptDelegate(context.Background(), uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Nil(t, got)
}

// ── Error paths ───────────────────────────────────────────────────────────

func TestDeptDelegate_EmptyBaseURL_Errors(t *testing.T) {
	// New() with no DELEGATION_BASE_URL env var leaves baseURL empty.
	c := New(nil)
	_, err := c.DeptDelegate(context.Background(), uuid.New(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "baseURL not configured")
}

func TestDeptDelegate_TransportError_Errors(t *testing.T) {
	// Port 1 is never open — connection refused gives a transport error.
	c := NewHTTPClient("http://127.0.0.1:1", 200*time.Millisecond, nil)
	_, err := c.DeptDelegate(context.Background(), uuid.New(), uuid.New(), uuid.New())
	require.Error(t, err)
}

func TestDeptDelegate_Non2xx_ClientError_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"insufficient_role"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	_, err := c.DeptDelegate(context.Background(), uuid.New(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
}

func TestDeptDelegate_Non2xx_ServerError_Errors(t *testing.T) {
	// 5xx also triggers the IncXsvcError("5xx") metrics path + returns error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"internal_error"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	_, err := c.DeptDelegate(context.Background(), uuid.New(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

// ── Constructor behaviours ─────────────────────────────────────────────────

func TestNewHTTPClient_NilLogger_UsesDefault(t *testing.T) {
	// nil logger must not panic — warn() is a no-op.
	c := NewHTTPClient("http://delegation.internal", 5*time.Second, nil)
	assert.NotNil(t, c)
}

func TestNewHTTPClient_ZeroTimeout_UsesDefaultTimeout(t *testing.T) {
	// timeout ≤ 0 must be replaced with the 1000 ms default (Gap-8: raised
	// from 300ms, which was too tight for the DLG-I3 dept-scope lookup).
	c := NewHTTPClient("http://delegation.internal", 0, nil)
	assert.NotNil(t, c)
	assert.Equal(t, 1000*time.Millisecond, c.client.Timeout)
}

func TestNewHTTPClient_ExplicitURL_ReturnsFunctionalClient(t *testing.T) {
	c := NewHTTPClient("http://delegation.internal", 5*time.Second, nil)
	assert.NotNil(t, c)
}

// ── setInternalHeaders ────────────────────────────────────────────────────

func TestSetInternalHeaders_SetsExpectedHeaders(t *testing.T) {
	tenantID := uuid.New()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com", nil)
	c := NewHTTPClient("http://delegation.internal", 5*time.Second, nil)
	c.setInternalHeaders(req, tenantID)

	assert.Equal(t, "iam-system", req.Header.Get("x-user-id"))
	assert.Equal(t, tenantID.String(), req.Header.Get("x-tenant-id"))
	assert.Equal(t, "iam-system", req.Header.Get("x-tenant-roles"))
}
