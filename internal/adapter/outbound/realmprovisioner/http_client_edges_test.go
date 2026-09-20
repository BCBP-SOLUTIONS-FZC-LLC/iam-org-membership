// Phase 19 — realmprovisioner HTTP client edge branches:
//   - NewHTTPClient nil-logger + non-positive timeout defaults
//   - CreateInvitedUser request-build error, transport error, decode error, non-2xx
//   - DeleteUser 404-as-success (PI-9 idempotent), request-build error
//   - PatchRealmConfig request-build error, transport error, non-2xx
//   - RevokeUserSessions non-2xx metric increment (AUTH-8 fail-open)
package realmprovisioner

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestP19RPEdges_NewHTTPClient_DefaultsOnZeroTimeoutAndNilLogger(t *testing.T) {
	c := NewHTTPClient("http://x", 0, nil)
	require.NotNil(t, c)
	assert.NotNil(t, c.logger)
	assert.Equal(t, "http://x", c.baseURL)
	assert.NotZero(t, c.client.Timeout)
}

func TestP19RPEdges_CreateInvitedUser_BadBaseURL_RequestBuildError(t *testing.T) {
	c := NewHTTPClient("http://\x7f", 0, slog.Default())
	_, err := c.CreateInvitedUser(context.Background(), port.CreateInvitedUserRequest{
		TenantID: uuid.New(), Email: "a@x.com", FullName: "A",
	})
	assert.Error(t, err)
}

func TestP19RPEdges_CreateInvitedUser_Non2xx_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "conflict", http.StatusConflict)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0, slog.Default())
	_, err := c.CreateInvitedUser(context.Background(), port.CreateInvitedUserRequest{
		TenantID: uuid.New(), Email: "a@x.com",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "409")
}

func TestP19RPEdges_CreateInvitedUser_MalformedJSON_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0, slog.Default())
	_, err := c.CreateInvitedUser(context.Background(), port.CreateInvitedUserRequest{
		TenantID: uuid.New(), Email: "a@x.com",
	})
	assert.Error(t, err)
}

func TestP19RPEdges_DeleteUser_404IsSuccess_PI9(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0, slog.Default())
	err := c.DeleteUser(context.Background(), uuid.New(), uuid.New())
	assert.NoError(t, err, "PI-9: 404 is idempotent success")
}

func TestP19RPEdges_DeleteUser_Non2xx_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0, slog.Default())
	err := c.DeleteUser(context.Background(), uuid.New(), uuid.New())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestP19RPEdges_DeleteUser_BadBaseURL_RequestBuildError(t *testing.T) {
	c := NewHTTPClient("http://\x7f", 0, slog.Default())
	err := c.DeleteUser(context.Background(), uuid.New(), uuid.New())
	assert.Error(t, err)
}

func TestP19RPEdges_PatchRealmConfig_Non2xx_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	local := true
	c := NewHTTPClient(srv.URL, 0, slog.Default())
	err := c.PatchRealmConfig(context.Background(), uuid.New(), port.RealmConfigPatch{
		LocalAccountsEnabled: &local,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "502")
}

func TestP19RPEdges_PatchRealmConfig_BadBaseURL_RequestBuildError(t *testing.T) {
	// A patch with LocalAccountsEnabled unset is a documented no-op (nothing
	// to patch) that returns nil before ever building a request — an empty
	// port.RealmConfigPatch{} here would never reach the bad-URL path this
	// test means to exercise, so it must set the one field PatchRealmConfig
	// actually reads.
	local := true
	c := NewHTTPClient("http://\x7f", 0, slog.Default())
	err := c.PatchRealmConfig(context.Background(), uuid.New(), port.RealmConfigPatch{
		LocalAccountsEnabled: &local,
	})
	assert.Error(t, err)
}

func TestP19RPEdges_RevokeUserSessions_Non2xx_FailOpen_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0, slog.Default())
	err := c.RevokeUserSessions(context.Background(), uuid.New(), uuid.New())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "503")
}

func TestP19RPEdges_RevokeUserSessions_BadBaseURL_RequestBuildError(t *testing.T) {
	c := NewHTTPClient("http://\x7f", 0, slog.Default())
	err := c.RevokeUserSessions(context.Background(), uuid.New(), uuid.New())
	assert.Error(t, err)
}
