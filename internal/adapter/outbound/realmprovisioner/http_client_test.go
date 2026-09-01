// Phase 10 — outbound clients · RealmProvisioner.
//
// Module:   iam-org-membership
// Feature:  RP client — CreateInvitedUser, DeleteUser (PI-9 idempotent),
//
//	PatchRealmConfig (T-15), RevokeUserSessions (AUTH-8 fail-open)
//
// File:     internal/adapter/outbound/realmprovisioner/http_client.go
//
// Test IDs: P10-RP-NNN.
package realmprovisioner

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═════════════════════════════════════════════════════════════════════════
// CreateInvitedUser
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P10-RP-001
// Feature:           RP · CreateInvitedUser happy path
// Scenario:          Positive — 201 returns a keycloak_user_id
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestP10RP001_CreateInvitedUserHappy(t *testing.T) {
	kcID := uuid.New()
	tenantID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/internal/tenants/"+tenantID.String()+"/users", r.URL.Path)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"keycloak_user_id": kcID})
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	resp, err := c.CreateInvitedUser(context.Background(), port.CreateInvitedUserRequest{
		TenantID: tenantID, Email: "u@e.com", FullName: "U",
	})
	require.NoError(t, err)
	assert.Equal(t, kcID, resp.KeycloakUserID)
}

// Test Case ID:      P10-RP-001b
// Feature:           RP · CreateInvitedUser — F5, required_actions sent verbatim
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10RP001b_CreateInvitedUserSendsRequiredActionsVerbatim(t *testing.T) {
	tenantID := uuid.New()
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"keycloak_user_id": uuid.New()})
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	_, err := c.CreateInvitedUser(context.Background(), port.CreateInvitedUserRequest{
		TenantID: tenantID, Email: "u@e.com", FullName: "U",
		RequiredActions: []string{port.RequiredActionVerifyEmail, port.RequiredActionUpdatePassword, port.RequiredActionConfigureTOTP},
	})
	require.NoError(t, err)
	assert.ElementsMatch(t,
		[]any{port.RequiredActionVerifyEmail, port.RequiredActionUpdatePassword, port.RequiredActionConfigureTOTP},
		gotBody["required_actions"], "RP-5: required_actions must be applied verbatim")
}

// Test Case ID:      P10-RP-001c
// Feature:           RP · CreateInvitedUser — no required_actions set → key omitted
// Priority: P2 · Severity: Minor · Automation Status: Automated
func TestP10RP001c_CreateInvitedUserOmitsRequiredActionsWhenUnset(t *testing.T) {
	tenantID := uuid.New()
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"keycloak_user_id": uuid.New()})
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	_, err := c.CreateInvitedUser(context.Background(), port.CreateInvitedUserRequest{
		TenantID: tenantID, Email: "u@e.com", FullName: "U",
	})
	require.NoError(t, err)
	_, present := gotBody["required_actions"]
	assert.False(t, present, "empty/unset RequiredActions must not send a null key")
}

// Test Case ID:      P10-RP-002
// Feature:           RP · CreateInvitedUser dev fallback (baseURL="")
// Scenario:          Positive — returns a random kc user id, no error
// Priority: P2 · Severity: Minor · Automation Status: Automated
func TestP10RP002_CreateInvitedUserDevFallback(t *testing.T) {
	c := NewHTTPClient("", 5*time.Second, slog.Default())
	resp, err := c.CreateInvitedUser(context.Background(), port.CreateInvitedUserRequest{
		TenantID: uuid.New(), Email: "u@e.com", FullName: "U",
	})
	require.NoError(t, err, "empty baseURL is dev-fallback, not an error")
	assert.NotEqual(t, uuid.Nil, resp.KeycloakUserID)
}

// Test Case ID:      P10-RP-003
// Feature:           RP · CreateInvitedUser non-2xx → error
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10RP003_CreateInvitedUserNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	_, err := c.CreateInvitedUser(context.Background(), port.CreateInvitedUserRequest{
		TenantID: uuid.New(), Email: "u@e.com", FullName: "U",
	})
	require.Error(t, err)
}

// ═════════════════════════════════════════════════════════════════════════
// DeleteUser
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P10-RP-010
// Feature:           PI-9 · DeleteUser 404 treated as success (idempotent)
// Scenario:          Positive — server returns 404 → no error
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestP10RP010_DeleteUser404Idempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	err := c.DeleteUser(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err, "PI-9: 404 on DeleteUser is idempotent success")
}

// Test Case ID:      P10-RP-011
// Feature:           DeleteUser happy path (204)
// Scenario:          Positive — server returns 204
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10RP011_DeleteUser204Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	err := c.DeleteUser(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
}

// Test Case ID:      P10-RP-012
// Feature:           DeleteUser 5xx → error
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10RP012_DeleteUser5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	err := c.DeleteUser(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
}

// Test Case ID:      P10-RP-013
// Feature:           DeleteUser dev fallback (baseURL="")
// Scenario:          Positive — no-op success
// Priority: P2 · Severity: Minor · Automation Status: Automated
func TestP10RP013_DeleteUserDevFallback(t *testing.T) {
	c := NewHTTPClient("", 5*time.Second, slog.Default())
	err := c.DeleteUser(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
}

// ═════════════════════════════════════════════════════════════════════════
// PatchRealmConfig
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P10-RP-020
// Feature:           T-15 · PatchRealmConfig happy path (200)
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10RP020_PatchRealmConfigHappy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPatch, r.Method)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	flag := true
	err := c.PatchRealmConfig(context.Background(), uuid.New(), port.RealmConfigPatch{LocalAccountsEnabled: &flag})
	require.NoError(t, err)
}

// Test Case ID:      P10-RP-021
// Feature:           T-15 · PatchRealmConfig 5xx → error (caller flips realm_sync_pending)
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10RP021_PatchRealmConfig5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	flag := false
	err := c.PatchRealmConfig(context.Background(), uuid.New(), port.RealmConfigPatch{LocalAccountsEnabled: &flag})
	require.Error(t, err)
}

// Test Case ID:      P10-RP-022
// Feature:           PatchRealmConfig dev fallback (baseURL="")
// Priority: P2 · Severity: Minor · Automation Status: Automated
func TestP10RP022_PatchRealmConfigDevFallback(t *testing.T) {
	c := NewHTTPClient("", 5*time.Second, slog.Default())
	flag := true
	err := c.PatchRealmConfig(context.Background(), uuid.New(), port.RealmConfigPatch{LocalAccountsEnabled: &flag})
	require.NoError(t, err)
}

// ═════════════════════════════════════════════════════════════════════════
// RevokeUserSessions — AUTH-8 fail-open
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P10-RP-030
// Feature:           AUTH-8 · RevokeUserSessions happy (204)
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10RP030_RevokeUserSessionsHappy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	err := c.RevokeUserSessions(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
}

// Test Case ID:      P10-RP-031
// Feature:           AUTH-8 · RevokeUserSessions dev fallback
// Priority: P2 · Severity: Minor · Automation Status: Automated
func TestP10RP031_RevokeUserSessionsDevFallback(t *testing.T) {
	c := NewHTTPClient("", 5*time.Second, slog.Default())
	err := c.RevokeUserSessions(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
}

// Test Case ID:      P10-RP-032
// Feature:           AUTH-8 · 5xx propagates (caller must still not fail the primary op)
// Priority: P2 · Severity: Major · Automation Status: Automated
func TestP10RP032_RevokeUserSessions5xxPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	err := c.RevokeUserSessions(context.Background(), uuid.New(), uuid.New())
	// AUTH-8: 5xx surfaces so caller can increment the fail counter.
	// Caller MUST NOT abort the primary op; that discipline is at the caller.
	require.Error(t, err)
}

// ═════════════════════════════════════════════════════════════════════════
// Header propagation
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P10-RP-040
// Feature:           Internal-lane headers set on all RP calls
// Scenario:          Positive — x-tenant-id / x-user-id present
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10RP040_HeadersPropagated(t *testing.T) {
	var gotTenant, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant = r.Header.Get("x-tenant-id")
		gotUser = r.Header.Get("x-user-id")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	tenant := uuid.New()
	err := c.DeleteUser(context.Background(), tenant, uuid.New())
	require.NoError(t, err)
	assert.Equal(t, tenant.String(), gotTenant)
	assert.Equal(t, "iam-system", gotUser)
}

// ═════════════════════════════════════════════════════════════════════════
// F8 (RP↔O&M alignment review) — contract tests: every RealmProvisionerClient
// route is versioned under /api/v1/internal and tenant-nested, matching RP
// LLD v0.22's aligned internal-route convention. One test per method,
// asserting the exact path (not just the prefix) so a future regression to
// the old flat/unversioned shape fails loudly.
// ═════════════════════════════════════════════════════════════════════════

func TestF8_CreateInvitedUser_PathIsVersionedAndTenantNested(t *testing.T) {
	tenantID := uuid.New()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"keycloak_user_id": uuid.New()})
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	_, err := c.CreateInvitedUser(context.Background(), port.CreateInvitedUserRequest{
		TenantID: tenantID, Email: "u@e.com", FullName: "U",
	})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(gotPath, "/api/v1/internal/"), "path must be versioned: %s", gotPath)
	assert.Equal(t, "/api/v1/internal/tenants/"+tenantID.String()+"/users", gotPath)
}

func TestF8_DeleteUser_PathIsVersionedAndTenantNested(t *testing.T) {
	tenantID, keycloakUserID := uuid.New(), uuid.New()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	require.NoError(t, c.DeleteUser(context.Background(), tenantID, keycloakUserID))
	assert.True(t, strings.HasPrefix(gotPath, "/api/v1/internal/"), "path must be versioned: %s", gotPath)
	assert.Equal(t, "/api/v1/internal/tenants/"+tenantID.String()+"/users/"+keycloakUserID.String(), gotPath)
}

func TestF8_RevokeUserSessions_PathIsVersionedAndTenantNested(t *testing.T) {
	tenantID, keycloakUserID := uuid.New(), uuid.New()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	require.NoError(t, c.RevokeUserSessions(context.Background(), tenantID, keycloakUserID))
	assert.True(t, strings.HasPrefix(gotPath, "/api/v1/internal/"), "path must be versioned: %s", gotPath)
	assert.Equal(t, "/api/v1/internal/tenants/"+tenantID.String()+"/users/"+keycloakUserID.String()+"/revoke-sessions", gotPath)
}

func TestF8_PatchRealmConfig_PathIsVersionedAndTenantNested(t *testing.T) {
	tenantID := uuid.New()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	flag := true
	require.NoError(t, c.PatchRealmConfig(context.Background(), tenantID, port.RealmConfigPatch{LocalAccountsEnabled: &flag}))
	assert.True(t, strings.HasPrefix(gotPath, "/api/v1/internal/"), "path must be versioned: %s", gotPath)
	assert.Equal(t, "/api/v1/internal/tenants/"+tenantID.String()+"/realm-config", gotPath)
}

// ResetMFA (RP-9, LLD §16 OQ-8) — not yet part of port.RealmProvisionerClient
// (no caller until the admin feature is scheduled), but the HTTPClient
// method is fully implemented and tested here so it's ready to wire in.
func TestF8_ResetMFA_PathIsVersionedAndTenantNested(t *testing.T) {
	tenantID, keycloakUserID := uuid.New(), uuid.New()
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	require.NoError(t, c.ResetMFA(context.Background(), tenantID, keycloakUserID))
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.True(t, strings.HasPrefix(gotPath, "/api/v1/internal/"), "path must be versioned: %s", gotPath)
	assert.Equal(t, "/api/v1/internal/tenants/"+tenantID.String()+"/users/"+keycloakUserID.String()+"/mfa-reset", gotPath)
}

func TestF8_ResetMFA_Non2xx_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	err := c.ResetMFA(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
}

func TestF8_ResetMFA_DevFallback_NoBaseURL(t *testing.T) {
	c := NewHTTPClient("", 5*time.Second, slog.Default())
	err := c.ResetMFA(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err, "empty baseURL is dev-fallback, not an error")
}

// ─── Smoke tests (F8 acceptance criteria) ───────────────────────────────
//
// A true smoke test hits a live, deployed Realm Provisioner instance and
// isn't something a unit-test binary can do in CI. These two exercise the
// exact same request/response path end-to-end (build → send → decode)
// against an httptest server standing in for a real RP, asserting the 2xx
// contract the acceptance criteria describe — the closest equivalent
// available without a running RP to point REALM_PROVISIONER_BASE_URL at.
func TestF8_Smoke_CreateInvitedUser_Returns2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"keycloak_user_id": uuid.New()})
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	_, err := c.CreateInvitedUser(context.Background(), port.CreateInvitedUserRequest{
		TenantID: uuid.New(), Email: "smoke@e.com", FullName: "Smoke Test",
	})
	require.NoError(t, err)
}

func TestF8_Smoke_PatchRealmConfig_Returns2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	flag := true
	err := c.PatchRealmConfig(context.Background(), uuid.New(), port.RealmConfigPatch{LocalAccountsEnabled: &flag})
	require.NoError(t, err)
}
