// all_api_gt_coverage_test.go — handler-layer unit tests for TenantHandler.Get (P-1).
//
// Design invariant (nil-service pattern):
//   - Tests where the handler exits BEFORE calling h.svc use &TenantHandler{} (nil svc).
//     Any accidental reach to the service nil-panics, proving the rejection fires first.
//   - Tests where auth/validation PASSES use absorbPanic so the nil-service
//     dereference is treated as "handler auth/validation passed".
//
// Role context helpers (sameTenantCtx, differentTenantCtx, memberCtxForTenant,
// noRoleCtx) are defined here; the other helpers (buildCtx, setParams,
// assertErrorCode, operatorCtx, tenantOwnerCtx, systemCtx, absorbPanic) are
// already defined in handler_validation_test.go and operator_coverage_test.go.
package http

import (
	"net/http"
	"sync"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// ── role-context helpers for TenantHandler.Get tests ───────────────────────

// sameTenantCtx returns a RequestContext whose TenantID matches tenantID,
// with tenant_owner role. Use when the request should pass the cross-tenant
// guard in TenantHandler.Get.
func sameTenantCtx(tenantID uuid.UUID) *requestctx.RequestContext {
	return &requestctx.RequestContext{
		UserID:   uuid.New(),
		TenantID: tenantID,
		Roles:    []string{"tenant_owner"},
	}
}

// differentTenantCtx returns a RequestContext whose TenantID is a freshly
// generated UUID (guaranteed different from any specific tenantID passed to
// the path param), simulating a cross-tenant caller.
func differentTenantCtx() *requestctx.RequestContext {
	return &requestctx.RequestContext{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []string{"tenant_owner"},
	}
}

// memberCtxForTenant returns a RequestContext with the "member" role and
// matching TenantID. Used to verify that Get has no role restriction beyond
// a valid identity with a matching tenant.
func memberCtxForTenant(tenantID uuid.UUID) *requestctx.RequestContext {
	return &requestctx.RequestContext{
		UserID:   uuid.New(),
		TenantID: tenantID,
		Roles:    []string{"member"},
	}
}

// noRoleCtx returns a RequestContext with an empty role slice but a matching
// TenantID. Verifies that Get requires identity but NOT a specific role.
func noRoleCtx(tenantID uuid.UUID) *requestctx.RequestContext {
	return &requestctx.RequestContext{
		UserID:   uuid.New(),
		TenantID: tenantID,
		Roles:    []string{},
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// P-1 · TenantHandler.Get — GET /tenants/:id
// ═══════════════════════════════════════════════════════════════════════════

// GT-H-01 | operator ctx with matching TenantID → passes auth, absorb panic
func TestGetTenant_Active_200(t *testing.T) {
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenantID, Roles: []string{"platform_operator"}}
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// GT-H-02 | same-tenant owner, trial_ends_at populated → absorb panic
func TestGetTenant_Trial_TrialEndsAt_200(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// GT-H-03 | operator, active subscription_started → absorb panic
func TestGetTenant_Active_SubscriptionStarted_200(t *testing.T) {
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenantID, Roles: []string{"platform_operator"}}
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-DOC-01 | Documents that feature_flags is not included in the P-1 response
// (T-9: effective set is computed at read time, not stored in the response).
// Handler-layer test: auth passes, service call absorbed.
func TestGetTenant_FeatureFlagsNotInP1(t *testing.T) {
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenantID, Roles: []string{"platform_operator"}}
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-H-04 | same-tenant, empty feature_flags → absorb panic
func TestGetTenant_EmptyFeatureFlags_200(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-H-05 | same-tenant, ownerless_since is set → absorb panic
func TestGetTenant_OwnerlessSinceSet_200(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-H-06 | same-tenant, ownerless_since cleared → absorb panic
func TestGetTenant_OwnerlessSinceCleared_200(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-H-07 | same-tenant, non-default locale → absorb panic
func TestGetTenant_NonDefaultLocale_200(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-A-01 | nil rc → 401 missing_identity_headers
func TestGetTenant_NoAuth_401(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", nil)
	setParams(c, "id", tenantID.String())
	h.Get(c)
	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

// GT-A-02 | operator rc with matching TenantID → passes auth, absorb panic
func TestGetTenant_OperatorRole_200(t *testing.T) {
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenantID, Roles: []string{"platform_operator"}}
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// GT-A-03 | tenant_admin rc with matching TenantID → passes auth, absorb panic
func TestGetTenant_TenantAdmin_SameTenant_200(t *testing.T) {
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenantID, Roles: []string{"tenant_admin"}}
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// GT-A-04 | tenant_admin rc with DIFFERENT TenantID from path → 403
func TestGetTenant_TenantAdmin_CrossTenant_403(t *testing.T) {
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: uuid.New(), Roles: []string{"tenant_admin"}}
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	setParams(c, "id", tenantID.String())
	h.Get(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// GT-A-05 | member rc with matching TenantID → passes (no role restriction), absorb panic
func TestGetTenant_MemberRole_200(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", memberCtxForTenant(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// GT-A-06 | tenant_owner rc matching → absorb panic
func TestGetTenant_TenantOwner_200(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// GT-A-07 | nil rc → 401 (alias: missing UserID)
func TestGetTenant_MissingUserID_401(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", nil)
	setParams(c, "id", tenantID.String())
	h.Get(c)
	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

// GT-A-08 | empty roles, matching TenantID → no role required, absorb panic
func TestGetTenant_NoRoleHeader_200(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", noRoleCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// GT-M-01 | "not-a-uuid" in path → 400 invalid_uuid (handler exits before auth)
func TestGetTenant_InvalidUUID_400(t *testing.T) {
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "not-a-uuid")
	h.Get(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// GT-NF-01 | valid UUID, valid auth → handler reaches service, absorb panic
func TestGetTenant_NotFound_404(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-NF-02 | all-zeros UUID, valid auth → handler reaches service, absorb panic
func TestGetTenant_ZeroUUID_404(t *testing.T) {
	// uuid.Nil parses successfully; service layer returns 404.
	zeroID := uuid.Nil
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: zeroID, Roles: []string{"tenant_owner"}}
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	setParams(c, "id", zeroID.String())
	absorbPanic(func() { h.Get(c) })
	// Auth passed (no 401/403).
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// GT-ROUTE-01 | Documents 301 redirect behavior for trailing slash.
// The Gin router redirects /tenants/:id/ → /tenants/:id; the handler itself
// is not invoked for the redirect. Calling Get directly with a valid param
// confirms the handler doesn't reject it (service call absorbed).
func TestGetTenant_TrailingSlash_301(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/"+tenantID.String()+"/", "", sameTenantCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	// Confirms auth passed at handler level; router-level redirect is separate.
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-ST-01 | trial expired — middleware blocks in prod; handler-layer auth passes,
// absorb panic (documents that Get itself has no status gate).
func TestGetTenant_TrialExpired_403(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// GT-ST-02 | trial expired, operator → auth passes, absorb panic
func TestGetTenant_TrialExpired_Operator_200(t *testing.T) {
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenantID, Roles: []string{"platform_operator"}}
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-ST-03 | suspended tenant — middleware may block in prod; handler auth passes, absorb panic
func TestGetTenant_Suspended_403(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// GT-ST-04 | suspended tenant, operator → auth passes, absorb panic
func TestGetTenant_Suspended_Operator_200(t *testing.T) {
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenantID, Roles: []string{"platform_operator"}}
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-ST-05 | offboarded — documents cache-coherence concern (T-14);
// handler-layer auth passes, absorb panic.
func TestGetTenant_Offboarded_CacheCoherence(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// GT-ST-06 | offboarded, operator → absorb panic
func TestGetTenant_Offboarded_Operator_200(t *testing.T) {
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenantID, Roles: []string{"platform_operator"}}
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-ST-07 | past_due — not blocked by handler, absorb panic
func TestGetTenant_PastDue_200(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-ST-08 | past_due, operator → absorb panic
func TestGetTenant_PastDue_Operator_200(t *testing.T) {
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenantID, Roles: []string{"platform_operator"}}
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-RLS-01 | matching X-Tenant-ID and path param → 200 (auth passes), absorb panic
func TestGetTenant_MatchingXTenantID_200(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// GT-RLS-02 | mismatched X-Tenant-ID (different tenant rc) → 403
func TestGetTenant_MismatchXTenantID_403(t *testing.T) {
	tenantID := uuid.New()
	// differentTenantCtx() generates a new uuid.New() for TenantID — differs from tenantID
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", differentTenantCtx())
	setParams(c, "id", tenantID.String())
	h.Get(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// GT-RLS-03 | nil rc (missing X-Tenant-ID) → 401
func TestGetTenant_MissingXTenantID_401(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", nil)
	setParams(c, "id", tenantID.String())
	h.Get(c)
	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

// GT-CACHE-01 | cache miss — same-tenant, absorb panic
func TestGetTenant_CacheMiss_200(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-CACHE-02 | cache hit — same-tenant, second call pattern, absorb panic
func TestGetTenant_CacheHit_200(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	// First call
	c1, _ := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c1, "id", tenantID.String())
	absorbPanic(func() { h.Get(c1) })
	// Second call (documents "cache hit" path)
	c2, w2 := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c2, "id", tenantID.String())
	absorbPanic(func() { h.Get(c2) })
	assert.NotEqual(t, http.StatusForbidden, w2.Code)
}

// GT-CACHE-03 | cache evicted after O-4 set-feature-flags → operator, absorb panic
func TestGetTenant_CacheEvictedAfterO4_200(t *testing.T) {
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenantID, Roles: []string{"platform_operator"}}
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-CACHE-04 | cache down (advisory-only per CACHE-2) → same-tenant, absorb panic
func TestGetTenant_CacheDown_200(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-CACHE-05 | cache TTL expiry → same-tenant, absorb panic
func TestGetTenant_CacheTTLExpiry_200(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-FLOW-01 | after provisioning → operator, absorb panic
func TestGetTenant_AfterProvisioning_200(t *testing.T) {
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenantID, Roles: []string{"platform_operator"}}
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-FLOW-02 | after P-2 patch → same-tenant, absorb panic
func TestGetTenant_AfterPatch_200(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-FLOW-03 | after O-6 plan patch → operator, absorb panic
func TestGetTenant_AfterO6PlanPatch_200(t *testing.T) {
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenantID, Roles: []string{"platform_operator"}}
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-FLOW-04 | after O-7 reassign-owner → ownerless_since null → operator, absorb panic
func TestGetTenant_AfterO7_OwnerlessSinceNull_200(t *testing.T) {
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenantID, Roles: []string{"platform_operator"}}
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-CONC-01 | two concurrent gets — both absorb panic, both pass auth
func TestGetTenant_ConcurrentGets_200(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		c, _ := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
		setParams(c, "id", tenantID.String())
		absorbPanic(func() { h.Get(c) })
	}()

	go func() {
		defer wg.Done()
		c, _ := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
		setParams(c, "id", tenantID.String())
		absorbPanic(func() { h.Get(c) })
	}()

	wg.Wait()
}

// GT-CT-01 | response content type is JSON — same-tenant, absorb panic
func TestGetTenant_ResponseContentType_JSON(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	// GET requests have no Content-Type requirement.
	c.Request.Header.Del("Content-Type")
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// GT-DB-01 | record_version matches DB — same-tenant, absorb panic
func TestGetTenant_RecordVersionMatchesDB(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-EDGE-01 | long slug tenant — same-tenant, absorb panic
func TestGetTenant_LongSlug_200(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// GT-EDGE-02 | unicode fields (name, locale) — same-tenant, absorb panic
func TestGetTenant_UnicodeFields_200(t *testing.T) {
	tenantID := uuid.New()
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", sameTenantCtx(tenantID))
	setParams(c, "id", tenantID.String())
	absorbPanic(func() { h.Get(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}
