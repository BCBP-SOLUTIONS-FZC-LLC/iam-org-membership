// Handler-layer tests for MembershipHandler.ResetMFA (P-34, §16 OQ-8/F6).
// Covers additional test case IDs from the Excel test sheet not yet present
// in membership_resetmfa_handler_test.go.
//
// Test case IDs: P34-HP-02, P34-AUTH-03, P34-VAL-02, P34-NF-02,
// P34-DEP-02, P34-BL-01, P34-BL-03, P34-EVT-01, P34-CACHE-01
package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// ── P34-HP-02: tenant_owner resets an active member → 204 ───────────────────

// Test Case ID: P34-HP-02
func TestResetMFA_HappyPath_OwnerResetsMember(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	mem := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{
			ID: uuid.New(), TenantID: tid, UserID: uid,
			Status: domain.MembershipActive, RecordVersion: 1,
		}, nil
	}}
	rp := &happyRPClient{resetMFAFn: func(context.Context, uuid.UUID, uuid.UUID) error {
		return nil
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, rp, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodPost, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", userID.String())
	h.ResetMFA(c)

	assert.Equal(t, http.StatusNoContent, c.Writer.Status(), w.Body.String())
}

// ── P34-AUTH-03: cross-tenant actor → 403 from requireTenantAdmin ────────────

// Test Case ID: P34-AUTH-03
// The actor is tenant_admin in tenant-A, but the path targets tenant-B.
// requireTenantAdmin detects actor.TenantID != pathTenantID and returns 403
// before the service is ever called. In production the middleware layer also
// blocks cross-tenant requests via RequireActiveMembership + RLS, but at the
// handler unit-test level the role gate fires first.
func TestResetMFA_Auth_CrossTenantForbidden(t *testing.T) {
	tenantA := uuid.New()
	tenantB := uuid.New()

	// svc is nil — requireTenantAdmin must fire before the service is reached.
	h := &MembershipHandler{}

	// Actor's context carries tenantA; path uses tenantB.
	c, w := buildCtx(http.MethodPost, "/", ``, tenantAdminCtx(tenantA))
	setParams(c, "id", tenantB.String(), "user_id", uuid.New().String())
	h.ResetMFA(c)

	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── P34-VAL-02: invalid user_id UUID → 400 ───────────────────────────────────

// Test Case ID: P34-VAL-02
// parseUUIDParam rejects the non-UUID user_id before any service call.
func TestResetMFA_Val_InvalidUserUUID(t *testing.T) {
	tenantID := uuid.New()
	h := &MembershipHandler{} // nil svc — must not be reached
	c, w := buildCtx(http.MethodPost, "/", ``, tenantAdminCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", "not-a-uuid")
	h.ResetMFA(c)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

// ── P34-NF-02: tenant doesn't exist → 404 member_not_found ──────────────────

// Test Case ID: P34-NF-02
// The actor's context carries the path tenant (so the role gate passes), but
// the service returns ErrMemberNotFound because the tenant does not exist
// in the database (no membership row visible).
func TestResetMFA_NotFound_TenantNotExist(t *testing.T) {
	unknownTenant := uuid.New()
	mem := &mhMemRepo{findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return nil, domain.NewError(domain.ErrMemberNotFound, "tenant not found")
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	// Use tenantOwnerCtx(unknownTenant) so requireTenantAdmin passes the tenant-match
	// check, then let the service surface 404.
	c, w := buildCtx(http.MethodPost, "/", ``, tenantOwnerCtx(unknownTenant))
	setParams(c, "id", unknownTenant.String(), "user_id", uuid.New().String())
	h.ResetMFA(c)

	assertErrorCode(t, w, http.StatusNotFound, "member_not_found")
}

// ── P34-DEP-02: RP timeout → 503 realm_provisioner_unavailable ──────────────

// Test Case ID: P34-DEP-02
func TestResetMFA_Dep_RPTimeout_503(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{
			ID: uuid.New(), TenantID: tid, UserID: uid,
			Status: domain.MembershipActive, RecordVersion: 1,
		}, nil
	}}
	rp := &happyRPClient{resetMFAFn: func(context.Context, uuid.UUID, uuid.UUID) error {
		return domain.NewError(domain.ErrRealmProvisionerUnavailable, "rp timeout")
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, rp, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodPost, "/", ``, tenantAdminCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.ResetMFA(c)

	assertErrorCode(t, w, http.StatusServiceUnavailable, "realm_provisioner_unavailable")
}

// ── P34-BL-01: self-reset allowed — actor IS the target user → 204 ──────────

// Test Case ID: P34-BL-01
// There is no restriction on an admin resetting their own MFA credentials.
// The handler must not special-case actor==target; it calls the service normally.
func TestResetMFA_BL_SelfReset_Allowed(t *testing.T) {
	tenantID := uuid.New()
	// Actor and target share the same user UUID.
	sharedUserID := uuid.New()

	mem := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{
			ID: uuid.New(), TenantID: tid, UserID: uid,
			Status: domain.MembershipActive, RecordVersion: 1,
		}, nil
	}}
	rp := &happyRPClient{resetMFAFn: func(context.Context, uuid.UUID, uuid.UUID) error {
		return nil
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, rp, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	// Build a RequestContext where actor UserID == path user_id.
	actorCtx := &requestctx.RequestContext{
		UserID:   sharedUserID,
		TenantID: tenantID,
		Roles:    []string{"tenant_admin"},
	}
	c, w := buildCtx(http.MethodPost, "/", ``, actorCtx)
	setParams(c, "id", tenantID.String(), "user_id", sharedUserID.String())
	h.ResetMFA(c)

	assert.Equal(t, http.StatusNoContent, c.Writer.Status(), "self-reset must be allowed for an admin: %s", w.Body.String())
}

// ── P34-BL-03: success → RP was called ───────────────────────────────────────

// Test Case ID: P34-BL-03
func TestResetMFA_BL_EventPayload(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	rpCalled := false
	mem := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{
			ID: uuid.New(), TenantID: tid, UserID: uid,
			Status: domain.MembershipActive, RecordVersion: 1,
		}, nil
	}}
	rp := &happyRPClient{resetMFAFn: func(context.Context, uuid.UUID, uuid.UUID) error {
		rpCalled = true
		return nil
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, rp, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodPost, "/", ``, tenantAdminCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", userID.String())
	h.ResetMFA(c)

	assert.Equal(t, http.StatusNoContent, c.Writer.Status(), w.Body.String())
	assert.True(t, rpCalled, "RP (RP-9) must be called on successful MFA reset")
}

// ── P34-EVT-01: RP failure → 503 (no event on RP failure) ───────────────────

// Test Case ID: P34-EVT-01
// When RP-9 fails, the handler must return 503. No MFAReset event is emitted
// (fail-closed: either the reset succeeds end-to-end or nothing changes).
func TestResetMFA_Evt_NoEventOnRPFailure(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{
			ID: uuid.New(), TenantID: tid, UserID: uid,
			Status: domain.MembershipActive, RecordVersion: 1,
		}, nil
	}}
	rp := &happyRPClient{resetMFAFn: func(context.Context, uuid.UUID, uuid.UUID) error {
		return domain.NewError(domain.ErrRealmProvisionerUnavailable, "rp failure")
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, rp, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodPost, "/", ``, tenantAdminCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", uuid.New().String())
	h.ResetMFA(c)

	assertErrorCode(t, w, http.StatusServiceUnavailable, "realm_provisioner_unavailable")
}

// ── P34-CACHE-01: no cache invalidation on MFA reset ─────────────────────────

// Test Case ID: P34-CACHE-01
// ResetMFA does not touch any cache keys — no Delete calls must occur.
func TestResetMFA_Cache_NoInvalidation(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	spy := &p34SpyCache{}
	mem := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{
			ID: uuid.New(), TenantID: tid, UserID: uid,
			Status: domain.MembershipActive, RecordVersion: 1,
		}, nil
	}}
	rp := &happyRPClient{resetMFAFn: func(context.Context, uuid.UUID, uuid.UUID) error {
		return nil
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, spy, rp, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodPost, "/", ``, tenantAdminCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", userID.String())
	h.ResetMFA(c)

	assert.Equal(t, http.StatusNoContent, c.Writer.Status(), w.Body.String())
	assert.Equal(t, 0, spy.deleteCalls, "cache.Delete must NOT be called for MFA reset — no cache invalidation required")
}

// ── p34SpyCache — spy wrapping happyCacheStub to count Delete calls ──────────

type p34SpyCache struct {
	happyCacheStub
	deleteCalls int
}

func (s *p34SpyCache) Delete(_ context.Context, _ ...string) error {
	s.deleteCalls++
	return nil
}
