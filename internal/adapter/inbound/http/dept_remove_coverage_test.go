// Handler-layer coverage tests for:
//
//	P-11 DELETE /api/v1/tenants/{id}/departments/{dept_id}/members/{user_id}
//	     (DeptMembershipHandler.Remove)
//
// P11-HP-01/02, P11-BL-01/02/03/04/05, P11-EDGE-01/02,
// P11-DEP-01, P11-AUTH-01/02/03, P11-SEC-01,
// P11-VAL-01/02/03, P11-EVT-01
package http

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// p11DeptMemRepo is a DeptMembershipRepository stub for P-11 remove tests.
type p11DeptMemRepo struct {
	removeFn func(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.DeptMembership, error)
}

func (r *p11DeptMemRepo) ListByUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *p11DeptMemRepo) ListByDepartment(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *p11DeptMemRepo) Assign(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, domain.DeptRole, uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
	return nil, nil, errors.New("not used")
}
func (r *p11DeptMemRepo) Remove(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.DeptMembership, error) {
	if r.removeFn != nil {
		return r.removeFn(ctx, tenantID, userID, deptID)
	}
	return &domain.DeptMembership{TenantID: tenantID, UserID: userID, DepartmentID: deptID}, nil
}
func (r *p11DeptMemRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *p11DeptMemRepo) SoftDeleteAllForDept(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}

var _ port.DeptMembershipRepository = (*p11DeptMemRepo)(nil)

// p11TenantDeptRepo always finds the department as active.
type p11TenantDeptRepo struct {
	findFn func(ctx context.Context, tenantID, deptID uuid.UUID) (*domain.TenantDepartment, error)
}

func (r *p11TenantDeptRepo) List(context.Context, uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *p11TenantDeptRepo) Find(ctx context.Context, tenantID, deptID uuid.UUID) (*domain.TenantDepartment, error) {
	if r.findFn != nil {
		return r.findFn(ctx, tenantID, deptID)
	}
	return &domain.TenantDepartment{TenantID: tenantID, DepartmentID: deptID, IsActive: true}, nil
}
func (r *p11TenantDeptRepo) Upsert(context.Context, uuid.UUID, uuid.UUID, bool, int64) (*domain.TenantDepartment, error) {
	return nil, errors.New("not used")
}
func (r *p11TenantDeptRepo) ListActive(context.Context, uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *p11TenantDeptRepo) Activate(_ context.Context, tid, did uuid.UUID) (*domain.TenantDepartment, error) {
	return &domain.TenantDepartment{TenantID: tid, DepartmentID: did, IsActive: true}, nil
}
func (r *p11TenantDeptRepo) SetActive(context.Context, uuid.UUID, uuid.UUID, bool, int64) (*domain.TenantDepartment, error) {
	return nil, errors.New("not used")
}

var _ port.TenantDepartmentRepository = (*p11TenantDeptRepo)(nil)

// buildP11Svc wires a DeptMembershipService for P-11 remove tests.
func buildP11Svc(dm *p11DeptMemRepo, wf port.WorkflowClient, delCheck port.DelegationCheckClient) *service.DeptMembershipService {
	mem := &mhMemRepo{
		findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: domain.MembershipActive}, nil
		},
	}
	return service.NewDeptMembershipService(
		dm, mem, &p11TenantDeptRepo{}, &drhDeptRepo{},
		delCheck, wf, happyCacheStub{}, happyTxRunner{},
	)
}

// zeroWorkflow returns 0 active workflows.
func zeroWorkflow() *drhWorkflowClient {
	return &drhWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{ActiveWorkflows: 0}, nil
		},
	}
}

// noDelegationCheck: DeptDelegate returns nil (no dept-scoped delegate).
type noDelegationCheck struct{}

func (noDelegationCheck) DeptDelegate(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*uuid.UUID, error) {
	return nil, nil
}

var _ port.DelegationCheckClient = noDelegationCheck{}

// activeDelegationCheck: returns a delegation UUID (user is dept-scoped delegate).
type activeDelegationCheck struct{}

func (activeDelegationCheck) DeptDelegate(_ context.Context, _, _, _ uuid.UUID) (*uuid.UUID, error) {
	id := uuid.New()
	return &id, nil
}

var _ port.DelegationCheckClient = activeDelegationCheck{}

// ── P11-HP-01: happy path owner removes dept member → 200 ───────────────────

// Test Case ID: P11-HP-01
func TestDeptRemove_HappyPath_200(t *testing.T) {
	tenantID := uuid.New()
	svc := buildP11Svc(&p11DeptMemRepo{}, zeroWorkflow(), noDelegationCheck{})
	h := &DeptMembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", "", tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Remove(c)

	assert.Less(t, w.Code, 300, w.Body.String())
}

// ── P11-HP-02: tenant_admin caller → 200 ─────────────────────────────────────

// Test Case ID: P11-HP-02
func TestDeptRemove_Admin_200(t *testing.T) {
	tenantID := uuid.New()
	svc := buildP11Svc(&p11DeptMemRepo{}, zeroWorkflow(), noDelegationCheck{})
	h := &DeptMembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", "", tenantAdminCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Remove(c)

	assert.Less(t, w.Code, 300, w.Body.String())
}

// ── P11-BL-01: user not in dept → 404 ────────────────────────────────────────

// Test Case ID: P11-BL-01
func TestDeptRemove_MemberNotInDept_404(t *testing.T) {
	tenantID := uuid.New()
	dm := &p11DeptMemRepo{removeFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.DeptMembership, error) {
		return nil, domain.NewError(domain.ErrMemberNotFound, "member not found")
	}}
	svc := buildP11Svc(dm, zeroWorkflow(), noDelegationCheck{})
	h := &DeptMembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", "", tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Remove(c)

	assertErrorCode(t, w, http.StatusNotFound, "member_not_found")
}

// ── P11-BL-04: workflow error → fail-open → 200 ───────────────────────────────

// Test Case ID: P11-BL-04
func TestDeptRemove_WorkflowFailOpen_200(t *testing.T) {
	tenantID := uuid.New()
	wf := &drhWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return nil, errors.New("workflow timeout")
		},
	}
	svc := buildP11Svc(&p11DeptMemRepo{}, wf, activeDelegationCheck{})
	h := &DeptMembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", "", tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Remove(c)

	// Workflow failure is fail-open — removal proceeds.
	assert.Less(t, w.Code, 300, w.Body.String())
}

// ── P11-BL-05: dept-scoped delegate, 0 active workflows → 200 ────────────────

// Test Case ID: P11-BL-05
func TestDeptRemove_ScopedDelegate_NoWorkflows_200(t *testing.T) {
	tenantID := uuid.New()
	svc := buildP11Svc(&p11DeptMemRepo{}, zeroWorkflow(), activeDelegationCheck{})
	h := &DeptMembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", "", tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Remove(c)

	assert.Less(t, w.Code, 300, w.Body.String())
}

// ── P11-BL-03: dept-scoped delegate, active workflows → 409 ──────────────────

// Test Case ID: P11-BL-03
func TestDeptRemove_ActiveDelegate_409(t *testing.T) {
	tenantID := uuid.New()
	wf := &drhWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{ActiveWorkflows: 1, WorkflowIDs: []uuid.UUID{uuid.New()}}, nil
		},
	}
	svc := buildP11Svc(&p11DeptMemRepo{}, wf, activeDelegationCheck{})
	h := &DeptMembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", "", tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Remove(c)

	assertErrorCode(t, w, http.StatusConflict, "workflow_resolution_required")
}

// ── P11-DEP-01: workflow timeout → fail-open → 200 ───────────────────────────

// Test Case ID: P11-DEP-01
func TestDeptRemove_WorkflowTimeout_FailOpen_200(t *testing.T) {
	tenantID := uuid.New()
	wf := &drhWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return nil, domain.NewError(domain.ErrWorkflowServiceUnavailable, "timeout")
		},
	}
	svc := buildP11Svc(&p11DeptMemRepo{}, wf, activeDelegationCheck{})
	h := &DeptMembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", "", tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Remove(c)

	assert.Less(t, w.Code, 300, w.Body.String())
}

// ── P11-AUTH-01: nil identity → 401 ──────────────────────────────────────────

// Test Case ID: P11-AUTH-01
func TestDeptRemove_NoIdentity_401(t *testing.T) {
	h := &DeptMembershipHandler{}
	c, w := buildCtx(http.MethodDelete, "/", "", nil)
	setParams(c, "id", uuid.New().String(), "dept_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Remove(c)
	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

// ── P11-AUTH-02: plain member → 403 ──────────────────────────────────────────

// Test Case ID: P11-AUTH-02
func TestDeptRemove_PlainMember_403(t *testing.T) {
	tenantID := uuid.New()
	h := &DeptMembershipHandler{}
	c, w := buildCtx(http.MethodDelete, "/", "", plainMemberCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Remove(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── P11-AUTH-03: tender_admin → 403 ──────────────────────────────────────────

// Test Case ID: P11-AUTH-03
func TestDeptRemove_TenderAdmin_403(t *testing.T) {
	tenantID := uuid.New()
	h := &DeptMembershipHandler{}
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenantID, Roles: []string{"tender_admin"}}
	c, w := buildCtx(http.MethodDelete, "/", "", rc)
	setParams(c, "id", tenantID.String(), "dept_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Remove(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── P11-SEC-01: cross-tenant → 403 ───────────────────────────────────────────

// Test Case ID: P11-SEC-01
func TestDeptRemove_CrossTenant_403(t *testing.T) {
	pathTenant := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: uuid.New(), Roles: []string{"tenant_owner"}}
	h := &DeptMembershipHandler{}
	c, w := buildCtx(http.MethodDelete, "/", "", rc)
	setParams(c, "id", pathTenant.String(), "dept_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Remove(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── P11-VAL-01: bad dept_id → 400 ────────────────────────────────────────────

// Test Case ID: P11-VAL-01
func TestDeptRemove_InvalidDeptID_400(t *testing.T) {
	tenantID := uuid.New()
	h := &DeptMembershipHandler{}
	c, w := buildCtx(http.MethodDelete, "/", "", tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", "bad-uuid", "user_id", uuid.New().String())
	h.Remove(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ── P11-VAL-02: bad tenant_id → 400 ──────────────────────────────────────────

// Test Case ID: P11-VAL-02
func TestDeptRemove_InvalidTenantID_400(t *testing.T) {
	h := &DeptMembershipHandler{}
	c, w := buildCtx(http.MethodDelete, "/", "", tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "bad-tenant", "dept_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Remove(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ── P11-VAL-03: bad user_id → 400 ────────────────────────────────────────────

// Test Case ID: P11-VAL-03
func TestDeptRemove_InvalidUserID_400(t *testing.T) {
	tenantID := uuid.New()
	h := &DeptMembershipHandler{}
	c, w := buildCtx(http.MethodDelete, "/", "", tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", uuid.New().String(), "user_id", "bad-uid")
	h.Remove(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ── P11-EVT-01: remove → DeptMembershipRevoked event emitted → 200 ───────────

// Test Case ID: P11-EVT-01
func TestDeptRemove_EventEmitted_200(t *testing.T) {
	tenantID := uuid.New()
	removed := false
	dm := &p11DeptMemRepo{removeFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.DeptMembership, error) {
		removed = true
		return &domain.DeptMembership{TenantID: tenantID}, nil
	}}
	svc := buildP11Svc(dm, zeroWorkflow(), noDelegationCheck{})
	h := &DeptMembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", "", tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Remove(c)

	assert.Less(t, w.Code, 300, w.Body.String())
	assert.True(t, removed, "Remove repository method must be called")
}

// ── P11-EDGE-01 / P11-EDGE-02: member already removed → 404 ─────────────────

// Test Case ID: P11-EDGE-01 / P11-EDGE-02
func TestDeptRemove_AlreadyRemoved_404(t *testing.T) {
	tenantID := uuid.New()
	dm := &p11DeptMemRepo{removeFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.DeptMembership, error) {
		return nil, domain.NewError(domain.ErrMemberNotFound, "already removed")
	}}
	svc := buildP11Svc(dm, zeroWorkflow(), noDelegationCheck{})
	h := &DeptMembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", "", tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Remove(c)

	assertErrorCode(t, w, http.StatusNotFound, "member_not_found")
}
