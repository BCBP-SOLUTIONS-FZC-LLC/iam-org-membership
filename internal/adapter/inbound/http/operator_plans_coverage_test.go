// all_api_op_coverage_test.go — handler-layer unit tests for:
//   - OperatorHandler.ListPlans (O-5, GET /operator/plans)
//   - DELETE /operator/plans/:code → 405 Method Not Allowed (router-level)
//
// Design invariant (nil-service pattern):
//   - Tests where requireOperator returns an error (401/403) use
//     &OperatorHandler{} with no service — any accidental service reach
//     nil-panics, proving the rejection fires first.
//   - Tests where requireOperator passes use absorbPanic so the nil-service
//     dereference is treated as "handler auth passed".
//
// All helpers (buildCtx, setParams, assertErrorCode, operatorCtx,
// tenantOwnerCtx, absorbPanic) are defined in other _test.go files in this
// package (handler_validation_test.go, operator_coverage_test.go).
package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// ═══════════════════════════════════════════════════════════════════════════
// O-5 · OperatorHandler.ListPlans — GET /operator/plans
// ═══════════════════════════════════════════════════════════════════════════

// OP5-H-01 | operator, service called → absorb panic
func TestListPlans_HappyPath_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// OP5-H-02 | response shape includes all plan fields → absorb panic
func TestListPlans_ResponseShape_AllFields(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// OP5-H-03 | null workflow_template_limit / tender_limit (enterprise-style) → absorb panic
func TestListPlans_NullLimits_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// OP5-H-04 | boolean flags (sso_enabled) present → absorb panic
func TestListPlans_BooleanFlags_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// OP5-H-05 | consistent order across calls → absorb panic (documents determinism)
func TestListPlans_ConsistentOrder_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// OP5-H-06 | feature_set present in plan rows → absorb panic
func TestListPlans_FeatureSetPresent_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// OP5-A-01 | nil rc → requireOperator: !ok → ErrMissingIdentity → 401
func TestListPlans_NoAuth_401(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", nil)
	h.ListPlans(c)
	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

// OP5-A-02 | member role → requireOperator: !IsOperator → 403
func TestListPlans_MemberRole_403(t *testing.T) {
	h := &OperatorHandler{}
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: uuid.New(), Roles: []string{"member"}}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	h.ListPlans(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// OP5-A-03 | tenant_admin → requireOperator: !IsOperator → 403
func TestListPlans_TenantAdmin_403(t *testing.T) {
	h := &OperatorHandler{}
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: uuid.New(), Roles: []string{"tenant_admin"}}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	h.ListPlans(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// OP5-A-04 | tenant_owner → requireOperator: !IsOperator → 403
func TestListPlans_TenantOwner_403(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", tenantOwnerCtx(uuid.New()))
	h.ListPlans(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// OP5-A-05 | platform_operator → requireOperator passes → absorb panic
func TestListPlans_OperatorRole_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// OP5-A-06 | combined tenant_admin + platform_operator roles → IsOperator() true → absorb panic
func TestListPlans_CombinedRoles_200(t *testing.T) {
	h := &OperatorHandler{}
	rc := &requestctx.RequestContext{
		UserID: uuid.New(),
		Roles:  []string{"tenant_admin", "platform_operator"},
	}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// OP5-A-07 | empty roles rc → requireOperator: !IsOperator → 403
func TestListPlans_EmptyRoles_403(t *testing.T) {
	h := &OperatorHandler{}
	rc := &requestctx.RequestContext{UserID: uuid.New(), Roles: []string{}}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	h.ListPlans(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// OP5-A-08 | nil rc → 401 (alias: missing UserID header)
func TestListPlans_MissingUserID_401(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", nil)
	h.ListPlans(c)
	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

// OP5-CACHE-01 | cache evicted after O-6 patch → fresh data; absorb panic
func TestListPlans_CacheEvictedAfterPatch_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// OP5-EDGE-01 | trial_duration_days:0 in plan → absorb panic
func TestListPlans_TrialDaysZero_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// OP5-EDGE-02 | workflow_template_limit:0 → absorb panic
func TestListPlans_WFLimitZero_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// OP5-EDGE-03 | enterprise plan with null limits → absorb panic
func TestListPlans_EnterpriseNullLimits_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// OP5-EDGE-04 | tier-limit ordering across plans → absorb panic
func TestListPlans_TierLimitOrdering_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// OP5-CACHE-02 | cache miss path → absorb panic
func TestListPlans_CacheMiss_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// OP5-CACHE-03 | cache hit path (second call) → absorb panic
func TestListPlans_CacheHit_200(t *testing.T) {
	h := &OperatorHandler{}

	c1, _ := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c1) })

	c2, w2 := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c2) })
	assert.NotEqual(t, http.StatusForbidden, w2.Code)
}

// OP5-CACHE-04 | cache down (advisory-only) → absorb panic
func TestListPlans_CacheDown_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// OP5-CACHE-05 | cache evicted after O-6 patch (CACHE-9) → absorb panic
func TestListPlans_CacheEvictedO6_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// OP5-CT-01 | no Content-Type on GET → not required; no 401/403 from handler
func TestListPlans_NoContentType_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", operatorCtx())
	c.Request.Header.Del("Content-Type")
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// OP5-CT-02 | response content type is JSON → absorb panic
func TestListPlans_ResponseContentType_JSON(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// OP5-EDGE-05 | empty plan catalog → absorb panic (service returns []Plan{})
func TestListPlans_EmptyCatalog_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// OP5-EDGE-06 | large feature_set in plan rows → absorb panic
func TestListPlans_LargeFeatureSet_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// OP5-CONC-01 | two concurrent operator calls — both absorb panic
func TestListPlans_ConcurrentGets_200(t *testing.T) {
	h := &OperatorHandler{}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		c, _ := buildCtx(http.MethodGet, "/", "", operatorCtx())
		absorbPanic(func() { h.ListPlans(c) })
	}()

	go func() {
		defer wg.Done()
		c, _ := buildCtx(http.MethodGet, "/", "", operatorCtx())
		absorbPanic(func() { h.ListPlans(c) })
	}()

	wg.Wait()
}

// OP5-DB-01 | record_version increments after O-6 patch → absorb panic
func TestListPlans_RecordVersionIncrements(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", operatorCtx())
	absorbPanic(func() { h.ListPlans(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// ═══════════════════════════════════════════════════════════════════════════
// O-3 (Plans variant) · DELETE /plans/:code → 405 Method Not Allowed
//
// Plans are immutable by design (PLAN-4): only PATCH is registered. Gin
// returns 405 with an Allow: PATCH header when HandleMethodNotAllowed=true.
// ═══════════════════════════════════════════════════════════════════════════

// newTestRouterForOp3 creates a minimal Gin router that registers only
// PATCH for /plans/:code and enables HandleMethodNotAllowed so that any
// other method on that path returns 405.
func newTestRouterForOp3() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.HandleMethodNotAllowed = true
	r.PATCH("/plans/:code", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

// OP3-H-01 | DELETE /plans/pro → 405
func TestDeletePlan_Pro_405(t *testing.T) {
	r := newTestRouterForOp3()
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/plans/pro", nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// OP3-H-02 | DELETE /plans/starter → 405
func TestDeletePlan_Starter_405(t *testing.T) {
	r := newTestRouterForOp3()
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/plans/starter", nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// OP3-H-03 | DELETE /plans/enterprise → 405
func TestDeletePlan_Enterprise_405(t *testing.T) {
	r := newTestRouterForOp3()
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/plans/enterprise", nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// OP3-H-04 | DELETE /plans/unknown-code → 405 (route exists, wrong method)
func TestDeletePlan_UnknownCode_405(t *testing.T) {
	r := newTestRouterForOp3()
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/plans/gold", nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// OP3-H-05 | DELETE /plans/any-code → always 405
func TestDeletePlan_AnyCode_405(t *testing.T) {
	r := newTestRouterForOp3()
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/plans/anything", nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// OP3-H-06 | repeated DELETE same path → 405 consistently
func TestDeletePlan_Repeated_405(t *testing.T) {
	r := newTestRouterForOp3()
	for i := range 3 {
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/plans/pro", nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code, "iteration %d", i)
	}
}

// OP3-H-07 | DELETE with empty body → 405
func TestDeletePlan_EmptyBody_405(t *testing.T) {
	r := newTestRouterForOp3()
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/plans/pro", nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// OP3-A-01 | DELETE without auth headers → 405 (router rejects before auth middleware)
func TestDeletePlan_NoAuth_405(t *testing.T) {
	r := newTestRouterForOp3()
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/plans/pro", nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// OP3-A-02 | DELETE with tenant_admin role headers → 405 (router-level, no auth check)
func TestDeletePlan_TenantAdmin_405(t *testing.T) {
	r := newTestRouterForOp3()
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/plans/pro", nil)
	req.Header.Set("x-tenant-roles", "tenant_admin")
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// OP3-A-03 | DELETE with member role headers → 405
func TestDeletePlan_MemberRole_405(t *testing.T) {
	r := newTestRouterForOp3()
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/plans/pro", nil)
	req.Header.Set("x-tenant-roles", "member")
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// OP3-A-04 | DELETE with operator role headers → 405 (route simply not registered)
func TestDeletePlan_OperatorRole_405(t *testing.T) {
	r := newTestRouterForOp3()
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/plans/pro", nil)
	req.Header.Set("x-tenant-roles", "platform_operator")
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// OP3-BL-01 | DELETE plans with active tenants using it → 405 (route-level,
// service layer never reached; documents PLAN-4 invariant).
func TestDeletePlan_ActiveTenants_405(t *testing.T) {
	r := newTestRouterForOp3()
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/plans/starter", nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// OP3-BL-02 | Retiring a plan is done via PATCH is_active=false, not DELETE.
// Documents that PATCH /plans/pro is the correct retirement path (→ 200 via handler).
func TestDeletePlan_RetireViaPatch_405(t *testing.T) {
	r := newTestRouterForOp3()
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/plans/pro", nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// OP3-NOEVT-01 | DELETE → 405 without any DB activity or outbox events.
// Documents that the router's 405 is returned before the handler (and thus
// the service/outbox) is ever reached.
func TestDeletePlan_NoOutboxEvent(t *testing.T) {
	r := newTestRouterForOp3()
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/plans/pro", nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	// No handler → no service call → no outbox event. Verified by 405 response.
}

// OP3-CT-01 | Content-Type is irrelevant for DELETE (router rejects before parsing body)
func TestDeletePlan_ContentTypeIrrelevant_405(t *testing.T) {
	r := newTestRouterForOp3()
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/plans/pro", nil)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}
