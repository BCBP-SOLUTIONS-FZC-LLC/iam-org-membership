package realmprovisioner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetIDPCount_ExactPath asserts the adapter calls ONLY
// /api/v1/internal/tenants/{id}/realm — not /realm-info or any other path.
// A wrong path can never pass this test.
func TestGetIDPCount_ExactPath(t *testing.T) {
	tenantID := uuid.New()
	var gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"idp_count": 2})
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0, nil)
	n, err := c.GetIDPCount(context.Background(), tenantID)

	require.NoError(t, err)
	assert.Equal(t, 2, n)
	wantPath := "/api/v1/internal/tenants/" + tenantID.String() + "/realm"
	assert.Equal(t, wantPath, gotPath,
		"adapter must call %s — if this fails the URL was changed back to /realm-info", wantPath)
}

func TestGetIDPCount_ParsesIDPCountField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"idp_count": 5, "realm_id": "some-realm"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0, nil)
	n, err := c.GetIDPCount(context.Background(), uuid.New())

	require.NoError(t, err)
	assert.Equal(t, 5, n)
}

func TestGetIDPCount_ZeroIdPs_Allowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"idp_count": 0}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0, nil)
	n, err := c.GetIDPCount(context.Background(), uuid.New())

	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestGetIDPCount_EmptyBaseURL_DevFallback(t *testing.T) {
	c := NewHTTPClient("", 0, nil)
	n, err := c.GetIDPCount(context.Background(), uuid.New())

	require.NoError(t, err)
	assert.Equal(t, 0, n, "dev fallback must return 0, nil")
}

func TestGetIDPCount_TransportError_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Close() // close immediately to force a transport error

	c := NewHTTPClient(srv.URL, 0, nil)
	_, err := c.GetIDPCount(context.Background(), uuid.New())

	assert.Error(t, err, "transport error must propagate (fail-closed)")
}

func TestGetIDPCount_404_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0, nil)
	_, err := c.GetIDPCount(context.Background(), uuid.New())

	assert.Error(t, err, "404 must return error, not (0, nil) — a tenant Core knows exists must have a realm")
}

func TestGetIDPCount_500_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0, nil)
	_, err := c.GetIDPCount(context.Background(), uuid.New())

	assert.Error(t, err, "5xx must propagate as error (fail-closed)")
}

func TestGetIDPCount_UnparseableBody_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0, nil)
	_, err := c.GetIDPCount(context.Background(), uuid.New())

	assert.Error(t, err, "unparseable body must return error (fail-closed)")
}

func TestGetIDPCount_SetsCorrectInternalHeaders(t *testing.T) {
	tenantID := uuid.New()
	var gotTenantID, gotUserID, gotRoles string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenantID = r.Header.Get("x-tenant-id")
		gotUserID = r.Header.Get("x-user-id")
		gotRoles = r.Header.Get("x-tenant-roles")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"idp_count": 0}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0, nil)
	_, _ = c.GetIDPCount(context.Background(), tenantID)

	assert.Equal(t, tenantID.String(), gotTenantID)
	assert.True(t, strings.Contains(gotUserID, "iam-system") || gotUserID != "",
		"x-user-id must be set")
	assert.Equal(t, "iam-system", gotRoles)
}
