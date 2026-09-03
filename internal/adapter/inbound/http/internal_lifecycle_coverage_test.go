// Handler-layer coverage tests for:
//
//	I-4 PATCH /api/v1/internal/tenants/{id}/members/{user_id}
//	I-5 DELETE /api/v1/internal/tenants/{id}/members/{user_id}
//
// I4-HP-01/02/03, I4-VAL-02, I4-AUTH-01/02, I4-BL-01, I4-SEC-02, I4-EDGE-02, I4-EVT-01, I4-BL-02
// I5-HP-03, I5-BL-02, I5-EDGE-01, I5-AUTH-01, I5-SEC-01, I5-EDGE-02, I5-EVT-01
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
	"github.com/stretchr/testify/assert"
)

// buildProvisioningForI4 wires a minimal ProvisioningService for I-4 tests.
func buildProvisioningForI4(mem *mhMemRepo) *service.ProvisioningService {
	return service.NewProvisioningService(nil, mem, nil, nil, nil, nil, nil, nil, nil, happyCacheStub{}, nil)
}

// buildProvisioningForI5 wires a minimal ProvisioningService for I-5 tests.
func buildProvisioningForI5(txr port.TxRunner) *service.ProvisioningService {
	return service.NewProvisioningService(nil, &mhMemRepo{
		findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: domain.MembershipActive, RecordVersion: 1}, nil
		},
	}, &mhRoleRepo{}, &drhDeptMemRepo{}, nil, nil, nil, nil, txr, happyCacheStub{}, nil)
}

// i5ErrTxRunner wraps passthroughTxRunner and makes RunInTx return a DB error.
type i5ErrTxRunner struct{}

func (i5ErrTxRunner) RunInTx(_ context.Context, _ func(context.Context) error) error {
	return domain.NewError(domain.ErrDBUnavailable, "pool exhausted")
}

// ── I4-HP-01: PATCH status=active → 200 ──────────────────────────────────────

// Test Case ID: I4-HP-01
func TestPatchMemberLifecycle_Active_200(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{setStatusFn: func(_ context.Context, tid, uid uuid.UUID, s domain.MembershipStatus, ver int64) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: s, RecordVersion: ver + 1}, nil
	}}
	h := &InternalHandler{provisioning: buildProvisioningForI4(mem)}

	c, w := buildCtx(http.MethodPatch, "/", `{"status":"active","record_version":1}`, iamSystemCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.PatchMemberLifecycle(c)

	assert.Less(t, w.Code, 300, w.Body.String())
	assert.Contains(t, w.Body.String(), `"status":"active"`)
}

// ── I4-HP-02: PATCH status=suspended → 200 ───────────────────────────────────

// Test Case ID: I4-HP-02
func TestPatchMemberLifecycle_Suspended_200(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{setStatusFn: func(_ context.Context, tid, uid uuid.UUID, s domain.MembershipStatus, ver int64) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: s, RecordVersion: ver + 1}, nil
	}}
	h := &InternalHandler{provisioning: buildProvisioningForI4(mem)}

	c, w := buildCtx(http.MethodPatch, "/", `{"status":"suspended","record_version":2}`, iamSystemCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.PatchMemberLifecycle(c)

	assert.Less(t, w.Code, 300, w.Body.String())
	assert.Contains(t, w.Body.String(), `"status":"suspended"`)
}

// ── I4-HP-03: PATCH status=left → 200 ────────────────────────────────────────

// Test Case ID: I4-HP-03
func TestPatchMemberLifecycle_Left_200(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{setStatusFn: func(_ context.Context, tid, uid uuid.UUID, s domain.MembershipStatus, ver int64) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: s, RecordVersion: ver + 1}, nil
	}}
	h := &InternalHandler{provisioning: buildProvisioningForI4(mem)}

	c, w := buildCtx(http.MethodPatch, "/", `{"status":"left","record_version":3}`, iamSystemCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.PatchMemberLifecycle(c)

	assert.Less(t, w.Code, 300, w.Body.String())
}

// ── I4-VAL-02: bad tenant_id → 400 ───────────────────────────────────────────

// Test Case ID: I4-VAL-02
func TestPatchMemberLifecycle_BadTenantID_400(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"status":"active","record_version":1}`, iamSystemCtx(uuid.New()))
	setParams(c, "id", "bad-uuid", "user_id", uuid.New().String())
	h.PatchMemberLifecycle(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ── I4-AUTH-01: iam-system is allowed ────────────────────────────────────────

// Test Case ID: I4-AUTH-01
func TestPatchMemberLifecycle_SystemCallerAllowed_200(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{setStatusFn: func(_ context.Context, tid, uid uuid.UUID, s domain.MembershipStatus, ver int64) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: s, RecordVersion: ver + 1}, nil
	}}
	h := &InternalHandler{provisioning: buildProvisioningForI4(mem)}
	c, w := buildCtx(http.MethodPatch, "/", `{"status":"active","record_version":1}`, iamSystemCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.PatchMemberLifecycle(c)
	// iam-system is the only allowed caller — other callers blocked by middleware.
	assert.Less(t, w.Code, 300, w.Body.String())
}

// ── I4-AUTH-02: regular member caller (middleware blocks) → 403 ───────────────

// Test Case ID: I4-AUTH-02
func TestPatchMemberLifecycle_RegularMember_403(t *testing.T) {
	// RequireSystemRole middleware gates this route.
	mw := RequireSystemRole()
	tenantID := uuid.New()
	c, w := buildCtx(http.MethodPatch, "/", `{"status":"active","record_version":1}`, plainMemberCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	mw(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── I4-BL-01: member not found → 404 ─────────────────────────────────────────

// Test Case ID: I4-BL-01
func TestPatchMemberLifecycle_MemberNotFound_404(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
		return nil, domain.NewError(domain.ErrMemberNotFound, "member not found")
	}}
	h := &InternalHandler{provisioning: buildProvisioningForI4(mem)}

	c, w := buildCtx(http.MethodPatch, "/", `{"status":"active","record_version":1}`, iamSystemCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.PatchMemberLifecycle(c)
	assertErrorCode(t, w, http.StatusNotFound, "member_not_found")
}

// ── I4-SEC-02: stale record_version → 409 ────────────────────────────────────

// Test Case ID: I4-SEC-02
func TestPatchMemberLifecycle_StaleRV_409(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
		return nil, domain.NewError(domain.ErrOptimisticLockConflict, "stale").WithDetails(map[string]any{"record_version": int64(5)})
	}}
	h := &InternalHandler{provisioning: buildProvisioningForI4(mem)}

	c, w := buildCtx(http.MethodPatch, "/", `{"status":"active","record_version":1}`, iamSystemCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.PatchMemberLifecycle(c)
	assertErrorCode(t, w, http.StatusConflict, "optimistic_lock_conflict")
}

// ── I4-EDGE-02: invalid status value → 400 ───────────────────────────────────

// Test Case ID: I4-EDGE-02
func TestPatchMemberLifecycle_InvalidStatus_400(t *testing.T) {
	tenantID := uuid.New()
	h := &InternalHandler{provisioning: buildProvisioningForI4(&mhMemRepo{})}
	c, w := buildCtx(http.MethodPatch, "/", `{"status":"unknown","record_version":1}`, iamSystemCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.PatchMemberLifecycle(c)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

// ── I4-EVT-01 / I4-BL-02: cache evicted on success ───────────────────────────

// Test Case ID: I4-EVT-01 / I4-BL-02
func TestPatchMemberLifecycle_CacheEvicted(t *testing.T) {
	tenantID := uuid.New()
	deleted := false
	cache := &spyCacheForI4{onDelete: func() { deleted = true }}
	mem := &mhMemRepo{setStatusFn: func(_ context.Context, tid, uid uuid.UUID, s domain.MembershipStatus, ver int64) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: s, RecordVersion: ver + 1}, nil
	}}
	svc := service.NewProvisioningService(nil, mem, nil, nil, nil, nil, nil, nil, nil, cache, nil)
	h := &InternalHandler{provisioning: svc}

	c, w := buildCtx(http.MethodPatch, "/", `{"status":"suspended","record_version":1}`, iamSystemCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.PatchMemberLifecycle(c)

	assert.Less(t, w.Code, 300, w.Body.String())
	assert.True(t, deleted, "cache.Delete must be called after successful status update")
}

type spyCacheForI4 struct {
	happyCacheStub
	onDelete func()
}

func (c *spyCacheForI4) Delete(_ context.Context, _ ...string) error {
	if c.onDelete != nil {
		c.onDelete()
	}
	return nil
}

// ════════════════════════════════════════════════════════════════════════
// I-5 DELETE /api/v1/internal/tenants/:id/members/:user_id
// ════════════════════════════════════════════════════════════════════════

// ── I5-HP-03: successful delete → 200 ────────────────────────────────────────

// Test Case ID: I5-HP-03
func TestDeleteMemberInternal_HappyPath_200(t *testing.T) {
	tenantID := uuid.New()
	h := &InternalHandler{provisioning: buildProvisioningForI5(happyTxRunner{})}
	c, w := buildCtx(http.MethodDelete, "/", "", iamSystemCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.DeleteMember(c)
	assert.Less(t, w.Code, 300, w.Body.String())
}

// ── I5-BL-02 / I5-EDGE-01: member already deleted → idempotent 200 ────────────

// Test Case ID: I5-BL-02 / I5-EDGE-01
func TestDeleteMemberInternal_IdempotentNotFound_200(t *testing.T) {
	tenantID := uuid.New()
	// FindByUserID returns ErrMemberNotFound → DeleteMember returns nil (idempotent).
	mem := &mhMemRepo{
		findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return nil, domain.NewError(domain.ErrMemberNotFound, "already deleted")
		},
	}
	svc := service.NewProvisioningService(nil, mem, &mhRoleRepo{}, nil, nil, nil, nil, nil, happyTxRunner{}, happyCacheStub{}, nil)
	h := &InternalHandler{provisioning: svc}
	c, w := buildCtx(http.MethodDelete, "/", "", iamSystemCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.DeleteMember(c)
	assert.Less(t, w.Code, 300, w.Body.String())
}

// ── I5-AUTH-01: nil identity → 403 ───────────────────────────────────────────

// Test Case ID: I5-AUTH-01
func TestDeleteMemberInternal_NoIdentity_403(t *testing.T) {
	mw := RequireSystemRole()
	c, w := buildCtx(http.MethodDelete, "/", "", nil)
	setParams(c, "id", uuid.New().String(), "user_id", uuid.New().String())
	mw(c)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}

// ── I5-SEC-01: non-system caller → 403 ───────────────────────────────────────

// Test Case ID: I5-SEC-01
func TestDeleteMemberInternal_NonSystemCaller_403(t *testing.T) {
	mw := RequireSystemRole()
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenantID, Roles: []string{"tenant_owner"}}
	c, w := buildCtx(http.MethodDelete, "/", "", rc)
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	mw(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── I5-EDGE-02: DB error → 503 ────────────────────────────────────────────────

// Test Case ID: I5-EDGE-02
func TestDeleteMemberInternal_DBError_503(t *testing.T) {
	tenantID := uuid.New()
	h := &InternalHandler{provisioning: buildProvisioningForI5(i5ErrTxRunner{})}
	c, w := buildCtx(http.MethodDelete, "/", "", iamSystemCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.DeleteMember(c)
	assertErrorCode(t, w, http.StatusServiceUnavailable, "db_unavailable")
}
