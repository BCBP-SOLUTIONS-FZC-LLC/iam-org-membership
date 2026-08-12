// Handler-layer unit tests for:
//
//	P-7  PATCH /api/v1/tenants/{id}/members/{user_id}     (MembershipHandler.Patch)
//	P-8  DELETE /api/v1/tenants/{id}/members/{user_id}    (MembershipHandler.Remove)
//	P-26 POST /api/v1/tenants/{id}/users/{user_id}/removal-resolution
//
// Complements handler_missing_test.go (early-return matrix) with happy-path,
// service-error, and auth branches that require wiring a service.
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

// plainMemberCtx returns a RequestContext for same-tenant caller without admin role.
func plainMemberCtx(tenantID uuid.UUID) *requestctx.RequestContext {
	return &requestctx.RequestContext{
		UserID:   uuid.New(),
		TenantID: tenantID,
		Roles:    []string{"member"},
	}
}

// ── P-7 MembershipHandler.Patch ──────────────────────────────────────────────

// P7-M-01: owner suspends active member → 200 (workflow nil → no advisory block).
func TestMembershipPatch_SuspendSuccess_200(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()
	mem := &mhMemRepo{setStatusFn: func(_ context.Context, tid, uid uuid.UUID, s domain.MembershipStatus, ver int64) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: s, RecordVersion: ver + 1}, nil
	}}
	// workflow=nil → WFI-13 advisory block is skipped (no advisory in response).
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, nil, nil, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, nil, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodPatch, "/", `{"status":"suspended","record_version":1}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", userID.String())
	h.Patch(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"status":"suspended"`)
}

// P7-M-02: owner reactivates member → 200, no delegate_impact field.
func TestMembershipPatch_ReactivateSuccess_200(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()
	mem := &mhMemRepo{setStatusFn: func(_ context.Context, tid, uid uuid.UUID, s domain.MembershipStatus, ver int64) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: s, RecordVersion: ver + 1}, nil
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, nil, nil, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, nil, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodPatch, "/", `{"status":"active","record_version":2}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", userID.String())
	h.Patch(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"status":"active"`)
	assert.NotContains(t, w.Body.String(), "delegate_impact")
}

// P7-M-03: tenant_admin can suspend (AUTH-2).
func TestMembershipPatch_TenantAdminCanSuspend_200(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{setStatusFn: func(_ context.Context, tid, uid uuid.UUID, s domain.MembershipStatus, ver int64) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: s, RecordVersion: ver + 1}, nil
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, nil, nil, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, nil, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodPatch, "/", `{"status":"suspended","record_version":1}`, tenantAdminCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.Patch(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// P7-M-04: plain member same tenant, no admin role → 403 insufficient_role.
func TestMembershipPatch_NonAdminSameTenant_403(t *testing.T) {
	tenantID := uuid.New()
	h := &MembershipHandler{}

	c, w := buildCtx(http.MethodPatch, "/", `{"status":"suspended","record_version":1}`, plainMemberCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.Patch(c)

	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// P7-M-12: repo returns optimistic_lock_conflict → 409 (CONC-4).
func TestMembershipPatch_OptimisticLockConflict_409(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
		return nil, domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict")
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, nil, nil, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, nil, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodPatch, "/", `{"status":"suspended","record_version":99}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.Patch(c)

	assertErrorCode(t, w, http.StatusConflict, "optimistic_lock_conflict")
}

// P7-M-13: user not a tenant member → 404 member_not_found.
func TestMembershipPatch_MemberNotFound_404(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
		return nil, domain.NewError(domain.ErrMemberNotFound, "member not found")
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, nil, nil, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, nil, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodPatch, "/", `{"status":"suspended","record_version":1}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.Patch(c)

	assertErrorCode(t, w, http.StatusNotFound, "member_not_found")
}

// P7-M-14: WFI-13 advisory present when workflow client reports active_workflows > 0.
func TestMembershipPatch_SuspendWithActiveWorkflows_200AdvisoryPresent(t *testing.T) {
	tenantID := uuid.New()
	wfID := uuid.New()
	mem := &mhMemRepo{setStatusFn: func(_ context.Context, tid, uid uuid.UUID, s domain.MembershipStatus, ver int64) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: s, RecordVersion: ver + 1}, nil
	}}
	wf := &drhWorkflowClient{getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
		return &port.DelegateImpact{ActiveWorkflows: 2, WorkflowIDs: []uuid.UUID{wfID, uuid.New()}}, nil
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, nil, nil, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, wf, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodPatch, "/", `{"status":"suspended","record_version":1}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.Patch(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "delegate_impact")
	assert.Contains(t, w.Body.String(), `"active_workflows":2`)
	assert.Contains(t, w.Body.String(), `"checked":true`)
}

// P7-M-15 (WFI-13 fail-open): workflow client errors on suspend → 200 with
// checked=false advisory. A security freeze must not be blockable by Workflow
// unavailability (§8.8.5).
func TestMembershipPatch_SuspendWorkflowClientError_200FailOpen(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{setStatusFn: func(_ context.Context, tid, uid uuid.UUID, s domain.MembershipStatus, ver int64) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: s, RecordVersion: ver + 1}, nil
	}}
	wf := &drhWorkflowClient{getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
		return nil, errors.New("workflow service down")
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, nil, nil, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, wf, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodPatch, "/", `{"status":"suspended","record_version":1}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.Patch(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"checked":false`)
}

// P7-AUTH-04: tender_admin role → 403 (AUTH-2 excludes tender_admin).
func TestMembershipPatch_TenderAdminForbidden_403(t *testing.T) {
	tenantID := uuid.New()
	h := &MembershipHandler{}

	tenderAdminRC := &requestctx.RequestContext{
		UserID:   uuid.New(),
		TenantID: tenantID,
		Roles:    []string{"tender_admin"},
	}
	c, w := buildCtx(http.MethodPatch, "/", `{"status":"suspended","record_version":1}`, tenderAdminRC)
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.Patch(c)

	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// P7-NF-02: suspend a soft-deleted (left) user → 404 member_not_found.
// SetStatus WHERE deleted_at IS NULL returns 0 rows → probeMembership → 404.
func TestMembershipPatch_SuspendLeftUser_404(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
		return nil, domain.NewError(domain.ErrMemberNotFound, "member not found")
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, nil, nil, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, nil, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodPatch, "/", `{"status":"suspended","record_version":1}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.Patch(c)

	assertErrorCode(t, w, http.StatusNotFound, "member_not_found")
}

// ── P-8 MembershipHandler.Remove (handler-layer auth gate only) ──────────────
// Full service-layer happy-path / workflow-gate / last-owner scenarios are in
// test/unit/membership_removeuser_test.go.

// P8-M-04: plain member same tenant → 403.
func TestMembershipRemove_NonAdminSameTenant_403(t *testing.T) {
	tenantID := uuid.New()
	h := &MembershipHandler{}

	c, w := buildCtx(http.MethodDelete, "/", ``, plainMemberCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.Remove(c)

	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── P-26 MembershipHandler.RemovalResolution ─────────────────────────────────

// P26-M-01: replace_delegate with replacement_user_id → 204.
// Uses c.Writer.Status() — gin only flushes status via WriteHeader on body
// write; 204 has no body so w.Code stays at 200 in the recorder.
func TestRemovalResolution_ReplaceDelegate_204(t *testing.T) {
	tenantID := uuid.New()
	replacementID := uuid.New()
	wf := &drhWorkflowClient{reassignDelegateFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, *uuid.UUID) error {
		return nil
	}}
	mem := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: domain.MembershipActive}, nil
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, nil, nil, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, wf, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	body := `{"action":"replace_delegate","replacement_user_id":"` + replacementID.String() + `"}`
	c, _ := buildCtx(http.MethodPost, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.RemovalResolution(c)

	assert.Equal(t, http.StatusNoContent, c.Writer.Status())
}

// P26-M-02: stop_workflows → 204.
func TestRemovalResolution_StopWorkflows_204(t *testing.T) {
	tenantID := uuid.New()
	wf := &drhWorkflowClient{cancelByDelegateFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) error {
		return nil
	}}
	svc := service.NewMembershipService(&mhMemRepo{}, &mhRoleRepo{}, &drhDeptMemRepo{}, nil, nil, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, wf, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, _ := buildCtx(http.MethodPost, "/", `{"action":"stop_workflows"}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.RemovalResolution(c)

	assert.Equal(t, http.StatusNoContent, c.Writer.Status())
}

// P26-M-03: unknown action → 422 invalid_action (Bug B-9 fixed).
// ErrInvalidAction now maps to 422 per LLD §17. Requires a non-nil workflow
// client — service returns 503 before the switch if workflow is nil.
func TestRemovalResolution_UnknownAction_422(t *testing.T) {
	tenantID := uuid.New()
	wf := &drhWorkflowClient{}
	svc := service.NewMembershipService(&mhMemRepo{}, &mhRoleRepo{}, &drhDeptMemRepo{}, nil, nil, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, wf, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodPost, "/", `{"action":"delete_everything"}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.RemovalResolution(c)

	assert.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "invalid_action")
}

// P26-M-04: non-admin caller → 403.
func TestRemovalResolution_NonAdminSameTenant_403(t *testing.T) {
	tenantID := uuid.New()
	h := &MembershipHandler{}

	c, w := buildCtx(http.MethodPost, "/", `{"action":"stop_workflows"}`, plainMemberCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.RemovalResolution(c)

	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// P26-M-05: replace_delegate with nil replacement_user_id → 400 (ErrValidation).
// LLD §8.8.3 names this "invalid_replacement" but the code returns ErrValidation
// (missing field is a 400, not a 422 precondition failure).
func TestRemovalResolution_ReplaceDelegate_MissingReplacement_400(t *testing.T) {
	tenantID := uuid.New()
	wf := &drhWorkflowClient{}
	svc := service.NewMembershipService(&mhMemRepo{}, &mhRoleRepo{}, &drhDeptMemRepo{}, nil, nil, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, wf, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodPost, "/", `{"action":"replace_delegate"}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.RemovalResolution(c)

	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}
