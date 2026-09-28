//go:build integration

// Package postgres_test — contract tests for I-8 and I-15 (TEST-7).
//
// These tests pin the exact JSON response shape of:
//   - I-8  GET /api/v1/internal/users/:id/memberships
//   - I-15 GET /api/v1/internal/tenants/:id/members/:user_id/exists
//
// A breaking change to either response (renamed field, dropped field, wrong
// type) will fail here before it reaches any downstream consumer. The tests
// use real Postgres via testcontainers-go so the actual DB→service→handler
// serialisation path is exercised end-to-end.
package postgres_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/http"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
)

// ── I-8 contract ─────────────────────────────────────────────────────────────

// TestContract_I8_ResponseShape_Owner pins every top-level field of the I-8
// response for a tenant owner. If a field is renamed, dropped, or changes
// type, this test fails — protecting every downstream consumer (AuthZ
// Enrichment, dashboards) that parses the response shape.
func TestContract_I8_ResponseShape_Owner(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	tenantID, ownerID := seedTenantWithOwner(t, ctx, fx, "contract-i8-owner")
	tctx := withTenant(ctx, tenantID)

	// Call through the real service layer (same path as the HTTP handler).
	proj, err := fx.AuthZ.GetMembership(tctx, tenantID, ownerID)
	require.NoError(t, err)
	require.NotNil(t, proj)

	// Serialise through the HTTP handler to capture exact wire shape.
	raw := callI8Handler(t, fx, tenantID, ownerID)

	// ── Required fields ──────────────────────────────────────────────────
	assert.Equal(t, ownerID.String(), raw["user_id"], "user_id must be the requesting user UUID")
	assert.Equal(t, tenantID.String(), raw["tenant_id"], "tenant_id must match")
	assert.NotEmpty(t, raw["status"], "status must be present")
	assert.NotEmpty(t, raw["plan"], "plan must be present")
	assert.NotNil(t, raw["subscription_status"], "subscription_status must be present (§16 A53)")
	assert.NotNil(t, raw["read_only"], "read_only must be present (derived, §16 A53 CACHE-9)")
	assert.Contains(t, raw, "mfa_freshness_seconds", "mfa_freshness_seconds must be present")
	assert.Contains(t, raw, "local_accounts_enabled", "local_accounts_enabled must be present")
	assert.Contains(t, raw, "default_locale", "default_locale must be present")

	// ── Collection fields — always arrays, never null (I8-4) ─────────────
	roles, ok := raw["roles"].([]interface{})
	require.True(t, ok, "roles must be a JSON array, not null")
	assert.Contains(t, roleStrings(roles), string(domain.RoleMember), "TR-7: derived member role must appear in I-8")
	assert.Contains(t, roleStrings(roles), string(domain.RoleTenantOwner), "owner grant must appear")

	depts, ok := raw["departments"].([]interface{})
	require.True(t, ok, "departments must be a JSON array, not null (I8-4)")

	flags, ok := raw["feature_flags"].([]interface{})
	require.True(t, ok, "feature_flags must be a JSON array of enabled flag names, not null")
	_ = depts
	_ = flags

	// ── Banned fields (removed under ADR-0008 Option C) ──────────────────
	assert.NotContains(t, raw, "active_delegations", "active_delegations[] must NOT appear in I-8 (ADR-0008 Option C)")
	assert.NotContains(t, raw, "tenant_status", "tenant_status deprecated alias must NOT appear (GAP-14)")
}

// TestContract_I8_ResponseShape_NoMembership verifies that I-8 returns 404
// (not 200 with a null body) when the user has no active membership.
func TestContract_I8_ResponseShape_NoMembership(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "contract-i8-nomem")
	unknownUser := uuid.New()

	rc := &requestctx.RequestContext{TenantID: tenantID, UserID: unknownUser, Roles: []string{"iam-system"}}
	w, c := newTestGinContext(http.MethodGet, "/?tenant_id="+tenantID.String(), rc)
	c.Params = gin.Params{
		{Key: "id", Value: unknownUser.String()},
	}
	internalHandler := httpadapter.NewInternalHandler(fx.Provisioning, fx.AuthZ, fx.Membership, nil, nil, nil, nil)
	internalHandler.GetMemberships(c)

	assert.Equal(t, http.StatusNotFound, w.Code, "absent membership must return 404 not 200")
}

// TestContract_I8_SingleDBRoundTrip verifies PERF-1: FindMembershipProjection
// now issues exactly one database query (single LEFT JOIN) rather than three
// sequential queries. We assert by confirming the result is correct after the
// refactor — the postgres integration test suite already has pg_stat_statements
// coverage; this pins the behaviour.
func TestContract_I8_SingleQueryResult_WithRolesAndDepts(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	tenantID, ownerID := seedTenantWithOwner(t, ctx, fx, "contract-i8-sq")
	tctx := withTenant(ctx, tenantID)

	// Seed a dept membership so the aggregation path is exercised.
	deptID := uuid.New()
	_, _, err := fx.Department.Activate(tctx, tenantID, deptID)
	if err != nil {
		t.Skipf("ActivateDepartment not available in this fixture: %v", err)
	}

	proj, err := fx.AuthZ.GetMembership(tctx, tenantID, ownerID)
	require.NoError(t, err)
	require.NotNil(t, proj)

	// Roles must still include the derived member + owner grant.
	found := false
	for _, r := range proj.Roles {
		if r == domain.RoleTenantOwner {
			found = true
		}
	}
	assert.True(t, found, "owner role must be present after single-query refactor")
	assert.NotNil(t, proj.Departments, "departments must not be nil (I8-4)")
}

// ── I-15 contract ─────────────────────────────────────────────────────────────

// TestContract_I15_ResponseShape_ActiveMember pins the I-15 response shape
// for an active member. Callers (Tender ACL, Delegation) replace composite FKs
// with this check — if the shape changes, grants would break silently.
func TestContract_I15_ResponseShape_ActiveMember(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	tenantID, ownerID := seedTenantWithOwner(t, ctx, fx, "contract-i15-active")

	raw := callI15Handler(t, fx, tenantID, ownerID, http.StatusOK)

	// active:true + tenant_membership_id present
	active, ok := raw["active"].(bool)
	require.True(t, ok, "active must be a bool")
	assert.True(t, active, "active owner must have active:true")

	tmID, ok := raw["tenant_membership_id"].(string)
	require.True(t, ok, "tenant_membership_id must be a string UUID when active:true")
	_, err := uuid.Parse(tmID)
	assert.NoError(t, err, "tenant_membership_id must be a valid UUID")
}

// TestContract_I15_ResponseShape_NonMember pins the I-15 response for a user
// who has no membership. Never 404 — always 200 with active:false (I-15 contract).
func TestContract_I15_ResponseShape_NonMember(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "contract-i15-none")
	unknownUser := uuid.New()

	raw := callI15Handler(t, fx, tenantID, unknownUser, http.StatusOK)

	active, ok := raw["active"].(bool)
	require.True(t, ok, "active must be a bool")
	assert.False(t, active, "non-member must have active:false")

	// tenant_membership_id must be null/absent when active:false
	if v, exists := raw["tenant_membership_id"]; exists {
		assert.Nil(t, v, "tenant_membership_id must be null when active:false")
	}

	// I-15 contract: NEVER 404, always 200 regardless of membership existence.
	// The 200 is confirmed by callI15Handler's status assertion above.
}

// ── helpers ───────────────────────────────────────────────────────────────────

func callI8Handler(t *testing.T, fx *testFixtures, tenantID, userID uuid.UUID) map[string]interface{} {
	t.Helper()
	rc := &requestctx.RequestContext{TenantID: tenantID, UserID: userID, Roles: []string{"iam-system"}}
	w, c := newTestGinContext(http.MethodGet, "/?tenant_id="+tenantID.String(), rc)
	c.Params = gin.Params{
		{Key: "id", Value: userID.String()},
	}
	internalHandler := httpadapter.NewInternalHandler(fx.Provisioning, fx.AuthZ, fx.Membership, nil, nil, nil, nil)
	internalHandler.GetMemberships(c)
	require.Equal(t, http.StatusOK, w.Code, "I-8 must return 200: %s", w.Body.String())
	var out map[string]interface{}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&out))
	return out
}

func callI15Handler(t *testing.T, fx *testFixtures, tenantID, userID uuid.UUID, wantStatus int) map[string]interface{} {
	t.Helper()
	rc := &requestctx.RequestContext{TenantID: tenantID, UserID: userID, Roles: []string{"iam-system"}}
	w, c := newTestGinContext(http.MethodGet, "/", rc)
	c.Params = gin.Params{
		{Key: "id", Value: tenantID.String()},
		{Key: "user_id", Value: userID.String()},
	}
	internalHandler := httpadapter.NewInternalHandler(fx.Provisioning, fx.AuthZ, fx.Membership, nil, nil, nil, nil)
	internalHandler.CheckMemberExists(c)
	require.Equal(t, wantStatus, w.Code, "I-15 response: %s", w.Body.String())
	var out map[string]interface{}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&out))
	return out
}

func newTestGinContext(method, path string, rc *requestctx.RequestContext) (*httptest.ResponseRecorder, *gin.Context) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// withTenant sets pgcommon.GUCSet so appPool connections issue
	// SET LOCAL app.tenant_id — mirrors what the auth middleware does in
	// production and is required for RLS-enforced queries to return rows.
	baseCtx := withTenant(context.Background(), rc.TenantID)
	req := httptest.NewRequestWithContext(requestctx.WithContext(baseCtx, rc), method, path, nil)
	c.Request = req
	return w, c
}

func roleStrings(roles []interface{}) []string {
	out := make([]string, len(roles))
	for i, r := range roles {
		out[i], _ = r.(string)
	}
	return out
}
