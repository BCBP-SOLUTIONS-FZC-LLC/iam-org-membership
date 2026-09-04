// Handler-layer coverage tests for MembershipHandler branches not already
// exercised by membership_happy_test.go / p7_p8_p26_coverage_test.go:
// List's custom-limit/service-error/next_cursor branches, SeatUsage's
// overage fields, Remove's malformed-param + full happy/error paths,
// ResetMFA's malformed user_id, RemovalResolution's malformed tenant id,
// and requireTenderAdminOrHigher's cross-tenant branch.
package http

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// P4-M-06: a valid custom limit (?limit=10) is honored (hits the `limit = n`
// assignment branch, distinct from both the default and the invalid-limit path).
func TestMembershipList_CustomLimit_200(t *testing.T) {
	tenantID := uuid.New()
	var gotLimit int
	mem := &mhMemRepo{listFn: func(_ context.Context, _ uuid.UUID, _ *domain.MembershipListCursor, limit int) (*domain.MembershipListPage, error) {
		gotLimit = limit
		return &domain.MembershipListPage{}, nil
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	c.Request.URL.RawQuery = "limit=10"
	h.List(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 10, gotLimit)
}

// P4-ERR-01: service.List returns an error → propagated through HandleError.
func TestMembershipList_ServiceError(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{listFn: func(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
		return nil, domain.NewError(domain.ErrTenantNotFound, "tenant not found")
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.List(c)

	assertErrorCode(t, w, http.StatusNotFound, "tenant_not_found")
}

// P4-H-05: page.NextCursor non-nil → response carries a base64 next_cursor.
func TestMembershipList_NextCursorPresent(t *testing.T) {
	tenantID := uuid.New()
	nextID := uuid.New()
	mem := &mhMemRepo{listFn: func(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
		return &domain.MembershipListPage{
			NextCursor: &domain.MembershipListCursor{ID: nextID},
		}, nil
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.List(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "next_cursor")
}

// P27-H-02: overage_since and grace_ends_at surface when set on the projection.
func TestMembershipSeatUsage_OverageFieldsPresent(t *testing.T) {
	tenantID := uuid.New()
	overageSince := time.Now().UTC().Add(-24 * time.Hour)
	tenants := &happyTenantRepo{findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
		return &domain.Tenant{ID: id, LicensedSeats: 5, OverageSince: &overageSince}, nil
	}}
	mem := &mhMemRepo{countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 6, nil }}
	inv := &iahInviteRepo{countPendingFn: func(context.Context, uuid.UUID) (int, error) { return 0, nil }}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, tenants, inv, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.SeatUsage(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"over_cap":true`)
	assert.Contains(t, w.Body.String(), "overage_since")
	assert.Contains(t, w.Body.String(), "grace_ends_at")
}

// P8-M-05: malformed tenant id path param → 400 invalid_uuid.
func TestMembershipRemove_InvalidTenantID_400(t *testing.T) {
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "not-a-uuid", "user_id", uuid.New().String())
	h.Remove(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// P8-H-01: full happy path — owner removes a plain member → 204.
func TestMembershipRemove_FullHappyPath_204(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	mem := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid, Status: domain.MembershipActive, RecordVersion: 1}, nil
	}}
	wf := &drhWorkflowClient{getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
		return &port.DelegateImpact{ActiveWorkflows: 0}, nil
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, wf, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", userID.String())
	h.Remove(c)

	assert.Equal(t, http.StatusNoContent, c.Writer.Status(), w.Body.String())
}

// P8-ERR-01: workflow gate blocks removal → 409 workflow_resolution_required.
func TestMembershipRemove_WorkflowGateBlocks_409(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	wf := &drhWorkflowClient{getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
		return &port.DelegateImpact{ActiveWorkflows: 1, WorkflowIDs: []uuid.UUID{uuid.New()}}, nil
	}}
	svc := service.NewMembershipService(&mhMemRepo{}, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, wf, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", userID.String())
	h.Remove(c)

	assertErrorCode(t, w, http.StatusConflict, "workflow_resolution_required")
}

// P34-M-02: malformed user_id path param on ResetMFA → 400 invalid_uuid.
func TestResetMFA_InvalidUserID_400(t *testing.T) {
	h := &MembershipHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodPost, "/", ``, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "user_id", "not-a-uuid")
	h.ResetMFA(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// P26-M-06: malformed tenant id path param on RemovalResolution → 400 invalid_uuid.
func TestRemovalResolution_InvalidTenantID_400(t *testing.T) {
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{"action":"stop_workflows"}`, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "not-a-uuid", "user_id", uuid.New().String())
	h.RemovalResolution(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// MW-TAOH-01: requireTenderAdminOrHigher's cross-tenant branch (delegates to
// requireSameTenantMember, whose error must propagate unchanged).
func TestRequireTenderAdminOrHigher_RejectsCrossTenant(t *testing.T) {
	tenantA, tenantB := uuid.New(), uuid.New()
	rc := &requestctx.RequestContext{TenantID: tenantB, Roles: []string{"tenant_owner"}}
	c, _ := newTestContext(rc)

	err := requireTenderAdminOrHigher(c, tenantA)
	require.Error(t, err)
	de, ok := err.(*domain.DomainError)
	require.True(t, ok)
	assert.Equal(t, "insufficient_role", de.Code)
}
