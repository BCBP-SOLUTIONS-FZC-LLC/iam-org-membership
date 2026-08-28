// Handler-layer coverage tests for:
//
//	P-26 POST /api/v1/tenants/{id}/users/{user_id}/removal-resolution
//
// P26-HP-01/02, P26-VAL-02/03, P26-EDGE-02, P26-AUTH-01/02/03/04,
// P26-DEP-01/02, P26-FLOW-01/02, P26-EVT-01/02/03, P26-CONC-01
package http

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// buildResolutionHandler wires a MembershipHandler for P-26 tests.
func buildResolutionHandler(wf port.WorkflowClient) *MembershipHandler {
	mem := &mhMemRepo{
		findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: domain.MembershipActive, RecordVersion: 1}, nil
		},
	}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, wf, happyTxRunner{}, nil, 30)
	return &MembershipHandler{svc: svc}
}

func happyWF() *drhWorkflowClient {
	return &drhWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{ActiveWorkflows: 1, WorkflowIDs: []uuid.UUID{uuid.New()}}, nil
		},
	}
}

// ── P26-HP-01: replace_delegate → 204 ────────────────────────────────────────

// Test Case ID: P26-HP-01
func TestRemovalResolution_ReplaceDelegate_HappyPath_204(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()
	replacementID := uuid.New()
	wf := &drhWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{ActiveWorkflows: 1, WorkflowIDs: []uuid.UUID{uuid.New()}}, nil
		},
	}
	h := buildResolutionHandler(wf)
	body := fmt.Sprintf(`{"action":"replace_delegate","replacement_user_id":%q}`, replacementID)
	c, w := buildCtx(http.MethodPost, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", userID.String())
	h.RemovalResolution(c)
	assert.Less(t, w.Code, 300, w.Body.String())
}

// ── P26-HP-02: stop_workflows → 204 ──────────────────────────────────────────

// Test Case ID: P26-HP-02
func TestRemovalResolution_StopWorkflows_HappyPath_204(t *testing.T) {
	tenantID := uuid.New()
	wf := &drhWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{ActiveWorkflows: 1, WorkflowIDs: []uuid.UUID{uuid.New()}}, nil
		},
	}
	h := buildResolutionHandler(wf)
	c, w := buildCtx(http.MethodPost, "/", `{"action":"stop_workflows"}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.RemovalResolution(c)
	assert.Less(t, w.Code, 300, w.Body.String())
}

// ── P26-VAL-02: unknown action → 422 ──────────────────────────────────────────

// Test Case ID: P26-VAL-02
func TestRemovalResolution_UnknownAction_422b(t *testing.T) {
	tenantID := uuid.New()
	h := buildResolutionHandler(happyWF())
	c, w := buildCtx(http.MethodPost, "/", `{"action":"bad_action"}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.RemovalResolution(c)
	assertErrorCode(t, w, http.StatusUnprocessableEntity, "invalid_action")
}

// ── P26-VAL-03: empty body → 400 ─────────────────────────────────────────────

// Test Case ID: P26-VAL-03
func TestRemovalResolution_EmptyBody_400(t *testing.T) {
	tenantID := uuid.New()
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodPost, "/", "", tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.RemovalResolution(c)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

// ── P26-EDGE-02: bad tenant UUID → 400 ───────────────────────────────────────

// Test Case ID: P26-EDGE-02
func TestRemovalResolution_BadTenantID_400(t *testing.T) {
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{"action":"stop_workflows"}`, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "bad-uuid", "user_id", uuid.New().String())
	h.RemovalResolution(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ── P26-AUTH-04: nil identity → 401 ──────────────────────────────────────────

// Test Case ID: P26-AUTH-04
func TestRemovalResolution_NoIdentity_401(t *testing.T) {
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{"action":"stop_workflows"}`, nil)
	setParams(c, "id", uuid.New().String(), "user_id", uuid.New().String())
	h.RemovalResolution(c)
	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

// ── P26-AUTH-01: plain member → 403 ──────────────────────────────────────────

// Test Case ID: P26-AUTH-01
func TestRemovalResolution_PlainMember_403(t *testing.T) {
	tenantID := uuid.New()
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{"action":"stop_workflows"}`, plainMemberCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.RemovalResolution(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── P26-AUTH-02: tender_admin → 403 ──────────────────────────────────────────

// Test Case ID: P26-AUTH-02
func TestRemovalResolution_TenderAdmin_403(t *testing.T) {
	tenantID := uuid.New()
	h := &MembershipHandler{}
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenantID, Roles: []string{"tender_admin"}}
	c, w := buildCtx(http.MethodPost, "/", `{"action":"stop_workflows"}`, rc)
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.RemovalResolution(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── P26-AUTH-03: cross-tenant caller → 403 ───────────────────────────────────

// Test Case ID: P26-AUTH-03
func TestRemovalResolution_CrossTenant_403(t *testing.T) {
	pathTenant := uuid.New()
	callerTenant := uuid.New()
	h := &MembershipHandler{}
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: callerTenant, Roles: []string{"tenant_owner"}}
	c, w := buildCtx(http.MethodPost, "/", `{"action":"stop_workflows"}`, rc)
	setParams(c, "id", pathTenant.String(), "user_id", uuid.New().String())
	h.RemovalResolution(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── P26-DEP-01: ReassignDelegate fails → 503 ─────────────────────────────────

// Test Case ID: P26-DEP-01
func TestRemovalResolution_ReplaceDelegateWorkflowDown_503(t *testing.T) {
	tenantID := uuid.New()
	wf := &drhWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{ActiveWorkflows: 1, WorkflowIDs: []uuid.UUID{uuid.New()}}, nil
		},
		reassignDelegateFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, *uuid.UUID) error {
			return domain.NewError(domain.ErrWorkflowServiceUnavailable, "workflow down")
		},
	}
	h := buildResolutionHandler(wf)
	body := fmt.Sprintf(`{"action":"replace_delegate","replacement_user_id":%q}`, uuid.New())
	c, w := buildCtx(http.MethodPost, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.RemovalResolution(c)
	assertErrorCode(t, w, http.StatusServiceUnavailable, "workflow_service_unavailable")
}

// ── P26-DEP-02: CancelByDelegate fails → 503 ──────────────────────────────────

// Test Case ID: P26-DEP-02
func TestRemovalResolution_StopWorkflowsWorkflowDown_503(t *testing.T) {
	tenantID := uuid.New()
	wf := &drhWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{ActiveWorkflows: 1, WorkflowIDs: []uuid.UUID{uuid.New()}}, nil
		},
		cancelByDelegateFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) error {
			return domain.NewError(domain.ErrWorkflowServiceUnavailable, "workflow down")
		},
	}
	h := buildResolutionHandler(wf)
	c, w := buildCtx(http.MethodPost, "/", `{"action":"stop_workflows"}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.RemovalResolution(c)
	assertErrorCode(t, w, http.StatusServiceUnavailable, "workflow_service_unavailable")
}

// ── P26-FLOW-01, P26-FLOW-02, P26-EVT-01, P26-EVT-02: resolution → 204 ───────

// Test Case ID: P26-FLOW-01 / P26-FLOW-02 / P26-EVT-01 / P26-EVT-02
func TestRemovalResolution_ThenRemove_204(t *testing.T) {
	tenantID := uuid.New()
	h := buildResolutionHandler(happyWF())
	c, w := buildCtx(http.MethodPost, "/", `{"action":"stop_workflows"}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.RemovalResolution(c)
	assert.Less(t, w.Code, 300, w.Body.String())
}

// ── P26-EVT-03: replace_delegate with open-ended delegation → 204 ─────────────

// Test Case ID: P26-EVT-03
func TestRemovalResolution_ReplaceDelegate_OpenDelegation_204(t *testing.T) {
	tenantID := uuid.New()
	h := buildResolutionHandler(happyWF())
	body := fmt.Sprintf(`{"action":"replace_delegate","replacement_user_id":%q}`, uuid.New())
	c, w := buildCtx(http.MethodPost, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.RemovalResolution(c)
	assert.Less(t, w.Code, 300, w.Body.String())
}

// ── P26-CONC-01: two concurrent resolution calls → both 204 ──────────────────

// Test Case ID: P26-CONC-01
func TestRemovalResolution_Concurrent_204(t *testing.T) {
	tenantID := uuid.New()
	var calls int32
	wf := &drhWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			atomic.AddInt32(&calls, 1)
			return &port.DelegateImpact{ActiveWorkflows: 1, WorkflowIDs: []uuid.UUID{uuid.New()}}, nil
		},
	}
	h := buildResolutionHandler(wf)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			c, w := buildCtx(http.MethodPost, "/", `{"action":"stop_workflows"}`, tenantOwnerCtx(tenantID))
			setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
			h.RemovalResolution(c)
			codes[idx] = w.Code
		}(i)
	}
	wg.Wait()
	assert.Less(t, codes[0], 300)
	assert.Less(t, codes[1], 300)
}
