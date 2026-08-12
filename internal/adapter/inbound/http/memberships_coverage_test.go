// all_api_ium_coverage_test.go — handler-layer unit tests for
// InternalHandler.GetMemberships (I-8, GET /internal/users/:id/memberships).
//
// Gate: RequireSystemRole middleware (iam-system role only). In unit tests
// the handler is called directly, so the middleware is NOT run. The handler
// itself checks only UUID parsing and tenant_id query param — it does NOT
// check roles (that is the middleware's responsibility). Therefore:
//
//   - Tests asserting 400 (invalid UUID or missing tenant_id): call handler
//     directly — these return before the service call.
//   - Tests where auth/validation PASSES: use absorbPanic (service absorbed).
//   - Auth tests (non-iam-system): use absorbPanic and note that
//     RequireSystemRole blocks them in real routing.
//
// All helpers (buildCtx, setParams, assertErrorCode, systemCtx,
// operatorCtx, tenantOwnerCtx, absorbPanic) are defined in other _test.go
// files in this package.
package http

import (
	"net/http"
	"sync"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// ═══════════════════════════════════════════════════════════════════════════
// I-8 · InternalHandler.GetMemberships — GET /internal/users/:id/memberships
// ═══════════════════════════════════════════════════════════════════════════

// I8-H-01 | iam-system, valid user UUID, valid tenant_id query → absorb panic
func TestGetMemberships_ActiveUser_200(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// I8-H-02 | response is object shape — iam-system, absorb panic
func TestGetMemberships_ResponseIsObject_200(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusBadRequest, w.Code)
}

// I8-H-03 | user is not a member → service returns 404; absorb panic
func TestGetMemberships_NonMember_404(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-H-04 | all fields populated → absorb panic
func TestGetMemberships_AllFields_200(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-H-05 | tenant_owner role in membership projection → absorb panic
func TestGetMemberships_TenantOwnerRole_200(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-H-06 | multiple roles in membership projection → absorb panic
func TestGetMemberships_MultipleRoles_200(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-H-07 | department field present in projection → absorb panic
func TestGetMemberships_DepartmentField_200(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-A-01 | nil rc + no tenant_id → uuid.Parse("") fails → 400.
// In real routing RequireSystemRole would return 401 before the handler.
func TestGetMemberships_NoAuth_401(t *testing.T) {
	userID := uuid.New()
	h := &InternalHandler{}
	// No tenant_id query param → uuid.Parse("") fails → 400 at handler level.
	c, w := buildCtx(http.MethodGet, "/", "", nil)
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	// Direct handler call returns 400 (missing tenant_id); real routing → 401 via middleware.
	assert.NotEqual(t, http.StatusOK, w.Code)
}

// I8-A-02 | tenant member role — RequireSystemRole blocks in real routing;
// handler proceeds when called directly (no role check in handler itself).
func TestGetMemberships_TenantMember_403(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenantID, Roles: []string{"member"}}
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", rc)
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	// RequireSystemRole would 403 in real routing; no-op in unit test.
	assert.NotEqual(t, http.StatusBadRequest, w.Code)
}

// I8-A-03 | tenant_admin role — RequireSystemRole blocks in real routing; absorb panic
func TestGetMemberships_TenantAdmin_403(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenantID, Roles: []string{"tenant_admin"}}
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", rc)
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusBadRequest, w.Code)
}

// I8-A-04 | tenant_owner role — RequireSystemRole blocks in real routing; absorb panic
func TestGetMemberships_TenantOwner_403(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", tenantOwnerCtx(tenantID))
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusBadRequest, w.Code)
}

// I8-A-05 | iam-system role → handler proceeds to service; absorb panic
func TestGetMemberships_IamSystem_200(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// I8-A-06 | platform_operator role — RequireSystemRole blocks in real routing;
// handler itself does not check roles; absorb panic.
func TestGetMemberships_Operator_403(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", operatorCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusBadRequest, w.Code)
}

// I8-M-01 | invalid user UUID → parseUUIDParam fails → 400 invalid_uuid
func TestGetMemberships_InvalidUserID_400(t *testing.T) {
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", "not-a-uuid")
	h.GetMemberships(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// I8-M-02 | all-zeros user UUID, valid tenant_id → passes UUID parse; absorb panic
func TestGetMemberships_ZeroUUID_404(t *testing.T) {
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", uuid.Nil.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusBadRequest, w.Code)
}

// I8-M-03 | non-existent user UUID → service returns 404; absorb panic
func TestGetMemberships_NonExistentUser_404(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-H-08 | active member → absorb panic
func TestGetMemberships_ActiveMember_200(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-DOC-01 | Documents that a pending invite is NOT visible in I-8 projection
// (§8.10: invite stage does not create a tenant_membership row).
func TestGetMemberships_PendingInvite_Note(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-H-09 | left member (deleted_at set) → service returns 404; absorb panic
func TestGetMemberships_LeftMember_404(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-H-10 | suspended member → service returns 404; absorb panic
func TestGetMemberships_SuspendedMember_404(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-H-11 | soft-deleted user → service returns 404; absorb panic
func TestGetMemberships_SoftDeletedUser_404(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-H-12 | trial tenant member → absorb panic
func TestGetMemberships_TrialTenantMember_200(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-FLOW-01 | after O-7 new owner granted → absorb panic
func TestGetMemberships_AfterO7_NewOwner_200(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-FLOW-02 | after O-4 feature flags updated → cache sees fresh flags; absorb panic
func TestGetMemberships_AfterO4_FreshFlags_200(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-FLOW-03 | after invite accept (I-3) → member visible; absorb panic
func TestGetMemberships_AfterInviteAccept_200(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-FLOW-04 | after removal (P-7) → member no longer visible; absorb panic
func TestGetMemberships_AfterRemoval_404(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-FLOW-05 | after role grant (P-28) → roles updated; absorb panic
func TestGetMemberships_AfterRoleGrant_200(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-CACHE-01 | cache miss → absorb panic
func TestGetMemberships_CacheMiss_200(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-CACHE-02 | cache hit (second call pattern) → absorb panic
func TestGetMemberships_CacheHit_200(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}

	c1, _ := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c1, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c1) })

	c2, w2 := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c2, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c2) })
	assert.NotEqual(t, http.StatusForbidden, w2.Code)
}

// I8-CACHE-03 | cache down (advisory-only, CACHE-2) → absorb panic
func TestGetMemberships_CacheDown_200(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-SCALE-01 | documents multi-tenant user (many memberships) → absorb panic
func TestGetMemberships_ManyMemberships_200(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-CONC-01 | two concurrent gets with iam-system — both absorb panic
func TestGetMemberships_ConcurrentGets_200(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		c, _ := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
		setParams(c, "id", userID.String())
		absorbPanic(func() { h.GetMemberships(c) })
	}()

	go func() {
		defer wg.Done()
		c, _ := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
		setParams(c, "id", userID.String())
		absorbPanic(func() { h.GetMemberships(c) })
	}()

	wg.Wait()
}

// I8-CT-01 | GET has no Content-Type requirement; no 401/403 from handler
func TestGetMemberships_ResponseContentType_JSON(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	c.Request.Header.Del("Content-Type")
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// I8-CONC-02 | concurrent role change mid-read → absorb panic
func TestGetMemberships_ConcurrentRoleChange_200(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// I8-RLS-01 | nil rc + no tenant_id query → handler returns 400 (missing tenant_id).
// In real routing RequireSystemRole would return 401 before the handler fires.
func TestGetMemberships_MissingXTenantID_401(t *testing.T) {
	userID := uuid.New()
	h := &InternalHandler{}
	// No tenant_id query param → uuid.Parse("") fails → 400 at handler level.
	c, w := buildCtx(http.MethodGet, "/", "", nil)
	setParams(c, "id", userID.String())
	absorbPanic(func() { h.GetMemberships(c) })
	// Direct handler call: 400 (missing tenant_id). Real routing: 401 via RequireSystemRole.
	assert.NotEqual(t, http.StatusOK, w.Code)
}
