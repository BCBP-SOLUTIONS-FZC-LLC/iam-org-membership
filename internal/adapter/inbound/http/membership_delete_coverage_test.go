// Handler-layer coverage tests for:
//
//	P-8  DELETE /api/v1/tenants/{id}/members/{user_id}  (MembershipHandler.Remove)
//
// P8-HP-01, P8-BL-01, P8-EDGE-01, P8-DEP-01, P8-DEP-02,
// P8-AUTH-01, P8-AUTH-02, P8-AUTH-03, P8-AUTH-04,
// P8-BL-03, P8-VAL-01, P8-VAL-02, P8-SEC-01
package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

// p8FakeTx stubs the pgx.Tx interface for P-8 RemoveUser tests.
// Exec returns success for the TM-13 SELECT FOR UPDATE; all other methods no-op.
type p8FakeTx struct{ pgx.Tx }

func (t *p8FakeTx) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

// p8TxRunner injects a p8FakeTx so pgadapterTxFromContext succeeds.
type p8TxRunner struct{}

func (p8TxRunner) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	return fn(service.WithTx(ctx, &p8FakeTx{}))
}

var _ port.TxRunner = p8TxRunner{}

// buildRemoveSvc wires a MembershipService for P-8 Remove tests.
// wf may be nil (skips delegate-impact check path).
func buildRemoveSvc(mem *mhMemRepo, wf port.WorkflowClient, rp port.RealmProvisionerClient) *service.MembershipService {
	if rp == nil {
		rp = &happyRPClient{}
	}
	roles := &mhRoleRepo{}
	return service.NewMembershipService(
		mem, roles, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{},
		happyCacheStub{}, rp, wf, p8TxRunner{}, nil, 30,
	)
}

// ── P8-HP-01: happy path — 0 active workflows → 204 ─────────────────────────

// Test Case ID: P8-HP-01
func TestMembershipDelete_HappyPath_NoWorkflows_204(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()
	mem := &mhMemRepo{
		findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: domain.MembershipActive, RecordVersion: 1}, nil
		},
	}
	wf := &drhWorkflowClient{getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
		return &port.DelegateImpact{ActiveWorkflows: 0, WorkflowIDs: nil}, nil
	}}
	svc := buildRemoveSvc(mem, wf, nil)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", "", tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", userID.String())
	h.Remove(c)

	assert.Less(t, w.Code, 300, w.Body.String())
}

// ── P8-BL-01: active workflows → 409 workflow_resolution_required ────────────

// Test Case ID: P8-BL-01
func TestMembershipDelete_HasDelegatedWorkflows_409(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{
		findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: domain.MembershipActive, RecordVersion: 1}, nil
		},
	}
	wf := &drhWorkflowClient{getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
		return &port.DelegateImpact{ActiveWorkflows: 2, WorkflowIDs: []uuid.UUID{uuid.New(), uuid.New()}}, nil
	}}
	svc := buildRemoveSvc(mem, wf, nil)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", "", tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.Remove(c)

	assertErrorCode(t, w, http.StatusConflict, "workflow_resolution_required")
}

// ── P8-EDGE-01: actor == target → still 204 ──────────────────────────────────

// Test Case ID: P8-EDGE-01
func TestMembershipDelete_SelfRemoval_204(t *testing.T) {
	tenantID := uuid.New()
	actorID := uuid.New()
	mem := &mhMemRepo{
		findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: domain.MembershipActive, RecordVersion: 1}, nil
		},
	}
	wf := &drhWorkflowClient{getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
		return &port.DelegateImpact{ActiveWorkflows: 0}, nil
	}}
	svc := buildRemoveSvc(mem, wf, nil)
	h := &MembershipHandler{svc: svc}

	rc := &requestctx.RequestContext{UserID: actorID, TenantID: tenantID, Roles: []string{"tenant_owner"}}
	c, w := buildCtx(http.MethodDelete, "/", "", rc)
	setParams(c, "id", tenantID.String(), "user_id", actorID.String())
	h.Remove(c)

	assert.Less(t, w.Code, 300, w.Body.String())
}

// ── P8-DEP-01: workflow service timeout → 503 ────────────────────────────────

// Test Case ID: P8-DEP-01
func TestMembershipDelete_WorkflowServiceTimeout_503(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{
		findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: domain.MembershipActive, RecordVersion: 1}, nil
		},
	}
	wf := &drhWorkflowClient{getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
		return nil, domain.NewError(domain.ErrWorkflowServiceUnavailable, "timeout")
	}}
	svc := buildRemoveSvc(mem, wf, nil)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", "", tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.Remove(c)

	assertErrorCode(t, w, http.StatusServiceUnavailable, "workflow_service_unavailable")
}

// ── P8-DEP-02: RP.RevokeUserSessions fails → still 204 (fail-open AUTH-8) ───

// Test Case ID: P8-DEP-02
func TestMembershipDelete_RPRevokeFailOpen_204(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{
		findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: domain.MembershipActive, RecordVersion: 1}, nil
		},
	}
	wf := &drhWorkflowClient{getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
		return &port.DelegateImpact{ActiveWorkflows: 0}, nil
	}}
	rp := &happyRPClient{revokeUserSessionsFn: func(context.Context, uuid.UUID, uuid.UUID) error {
		return domain.NewError(domain.ErrRealmProvisionerUnavailable, "rp down")
	}}
	svc := buildRemoveSvc(mem, wf, rp)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", "", tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.Remove(c)

	// AUTH-8: RP revoke is best-effort; failure must not block removal.
	assert.Less(t, w.Code, 300, w.Body.String())
}

// ── P8-AUTH-01: no identity → 401 ────────────────────────────────────────────

// Test Case ID: P8-AUTH-01
func TestMembershipDelete_NoIdentity_401(t *testing.T) {
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodDelete, "/", "", nil)
	setParams(c, "id", uuid.New().String(), "user_id", uuid.New().String())
	h.Remove(c)
	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

// ── P8-AUTH-02: plain member → 403 ───────────────────────────────────────────

// Test Case ID: P8-AUTH-02
func TestMembershipDelete_NonAdmin_403(t *testing.T) {
	tenantID := uuid.New()
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodDelete, "/", "", plainMemberCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.Remove(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── P8-AUTH-03 / P8-SEC-01: cross-tenant caller → 403 ───────────────────────

// Test Case ID: P8-AUTH-03 / P8-SEC-01
func TestMembershipDelete_CrossTenant_403(t *testing.T) {
	pathTenantID := uuid.New()
	callerTenantID := uuid.New()
	h := &MembershipHandler{}
	rc := &requestctx.RequestContext{
		UserID:   uuid.New(),
		TenantID: callerTenantID,
		Roles:    []string{"tenant_owner"},
	}
	c, w := buildCtx(http.MethodDelete, "/", "", rc)
	setParams(c, "id", pathTenantID.String(), "user_id", uuid.New().String())
	h.Remove(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── P8-AUTH-04: tender_admin → 403 ───────────────────────────────────────────

// Test Case ID: P8-AUTH-04
func TestMembershipDelete_TenderAdminBlocked_403(t *testing.T) {
	tenantID := uuid.New()
	h := &MembershipHandler{}
	rc := &requestctx.RequestContext{
		UserID:   uuid.New(),
		TenantID: tenantID,
		Roles:    []string{"tender_admin"},
	}
	c, w := buildCtx(http.MethodDelete, "/", "", rc)
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.Remove(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── P8-BL-03: member not found → 404 ─────────────────────────────────────────

// Test Case ID: P8-BL-03
func TestMembershipDelete_MemberNotFound_404(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{
		findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return nil, domain.NewError(domain.ErrMemberNotFound, "member not found")
		},
	}
	wf := &drhWorkflowClient{getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
		return &port.DelegateImpact{ActiveWorkflows: 0}, nil
	}}
	svc := buildRemoveSvc(mem, wf, nil)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", "", tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.Remove(c)

	assertErrorCode(t, w, http.StatusNotFound, "member_not_found")
}

// ── P8-VAL-01: invalid user_id → 400 ─────────────────────────────────────────

// Test Case ID: P8-VAL-01
func TestMembershipDelete_InvalidUserID_400(t *testing.T) {
	tenantID := uuid.New()
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodDelete, "/", "", tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", "not-a-uuid")
	h.Remove(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ── P8-VAL-02: invalid tenant_id → 400 ───────────────────────────────────────

// Test Case ID: P8-VAL-02
func TestMembershipDelete_InvalidTenantID_400(t *testing.T) {
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodDelete, "/", "", tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "bad-tenant", "user_id", uuid.New().String())
	h.Remove(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}
