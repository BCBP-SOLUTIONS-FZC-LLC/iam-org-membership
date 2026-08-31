// Handler-layer tests for MembershipHandler.ResetMFA (P-34, §16 OQ-8/F6).
package http

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// ── P34-AUTH-01: plain member → 403 (AUTH-2) ────────────────────────────

func TestResetMFA_PlainMember_Returns403(t *testing.T) {
	h := &MembershipHandler{} // nil svc: proves the role gate fires before the service is touched
	tenant := uuid.New()
	c, w := buildCtx(http.MethodPost, "/", ``, plainMemberCtx(tenant))
	setParams(c, "id", tenant.String(), "user_id", uuid.New().String())
	h.ResetMFA(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── P34-VALID-01: malformed tenant id → 400 ─────────────────────────────

func TestResetMFA_InvalidTenantID_Returns400(t *testing.T) {
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodPost, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "not-a-uuid", "user_id", uuid.New().String())
	h.ResetMFA(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ── P34-HAPPY-01: active member → RP-9 called, 204 ──────────────────────

func TestResetMFA_Success_204(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	mem := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid, Status: domain.MembershipActive, RecordVersion: 1}, nil
	}}
	rpCalled := false
	rp := &happyRPClient{resetMFAFn: func(context.Context, uuid.UUID, uuid.UUID) error {
		rpCalled = true
		return nil
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, rp, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodPost, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", userID.String())
	h.ResetMFA(c)

	assert.Equal(t, http.StatusNoContent, c.Writer.Status(), w.Body.String())
	assert.True(t, rpCalled, "RP-9 must be called on the happy path")
}

// ── P34-404-01: no membership row → 404 member_not_found ────────────────

func TestResetMFA_MemberNotFound_404(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return nil, domain.NewError(domain.ErrMemberNotFound, "no membership")
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodPost, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.ResetMFA(c)

	assertErrorCode(t, w, http.StatusNotFound, "member_not_found")
}

// ── P34-422-01: suspended member → 422 member_not_active ────────────────

func TestResetMFA_MemberNotActive_422(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid, Status: domain.MembershipSuspended, RecordVersion: 1}, nil
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodPost, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.ResetMFA(c)

	assertErrorCode(t, w, http.StatusUnprocessableEntity, "member_not_active")
}

// ── P34-503-01: RP-9 outage → 503 realm_provisioner_unavailable, fail-closed ─

func TestResetMFA_RPOutage_503(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid, Status: domain.MembershipActive, RecordVersion: 1}, nil
	}}
	rp := &happyRPClient{resetMFAFn: func(context.Context, uuid.UUID, uuid.UUID) error {
		return errors.New("rp-9 outage")
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, rp, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodPost, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.ResetMFA(c)

	assertErrorCode(t, w, http.StatusServiceUnavailable, "realm_provisioner_unavailable")
}
