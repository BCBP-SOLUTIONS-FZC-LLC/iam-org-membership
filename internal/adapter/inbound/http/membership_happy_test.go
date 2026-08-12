// Phase 19 — Handler happy-path coverage for MembershipHandler and the
// internal-lane subset that can be wired without a real *pgcommon.Pool
// (i.e. does NOT go through AuthZ/Operator/Provisioning services, which
// carry a Pool field for their SEAT-1 FOR UPDATE + cache-through paths).
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

// ── LOCAL fakes reused from tenant_delegation_happy_test.go
//     (happyTenantRepo, happyMembershipRepo, happyCacheStub, happyRPClient,
//      happyDelegationRepo, happyUPClient, happyTxRunner)
// ── LOCAL fakes reused from invite_acl_gm_happy_test.go
//     (iahInviteRepo, iahACLRepo)
// ── LOCAL fakes reused from dept_role_happy_test.go
//     (drhDeptMemRepo, drhTenantDeptRepo, drhWorkflowClient, drhLabelRepo)

// ── MembershipHandler.List (P-4) — happy path ─────────────────────────

func TestMembershipList_Success_200(t *testing.T) {
	tenantID := uuid.New()
	userA := uuid.New()
	mem := &mhMemRepo{listFn: func(_ context.Context, tid uuid.UUID, _ *domain.MembershipListCursor, _ int) (*domain.MembershipListPage, error) {
		return &domain.MembershipListPage{
			Items: []domain.MembershipListItem{
				{Membership: domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: userA, Status: domain.MembershipActive, RecordVersion: 1}},
			},
		}, nil
	}}
	roles := &mhRoleRepo{listByUserFn: func(_ context.Context, tid, uid uuid.UUID) ([]domain.TenantRole, error) {
		return []domain.TenantRole{{TenantID: tid, UserID: uid, RoleCode: domain.RoleTenantAdmin}}, nil
	}}
	svc := service.NewMembershipService(mem, roles, &drhDeptMemRepo{}, nil, nil, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.List(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), userA.String())
}

func TestMembershipList_InvalidLimit(t *testing.T) {
	tenantID := uuid.New()
	svc := service.NewMembershipService(&mhMemRepo{}, &mhRoleRepo{}, &drhDeptMemRepo{}, nil, nil, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	c.Request.URL.RawQuery = "limit=99999"
	h.List(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestMembershipList_InvalidCursor(t *testing.T) {
	tenantID := uuid.New()
	svc := service.NewMembershipService(&mhMemRepo{}, &mhRoleRepo{}, &drhDeptMemRepo{}, nil, nil, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	c.Request.URL.RawQuery = "cursor=not*valid*base64"
	h.List(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ── MembershipHandler.Get (P-5) — happy path + not-found ─────────────

func TestMembershipGet_Success_200(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()
	mem := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid, Status: domain.MembershipActive, RecordVersion: 1}, nil
	}}
	roles := &mhRoleRepo{listByUserFn: func(_ context.Context, tid, uid uuid.UUID) ([]domain.TenantRole, error) {
		return []domain.TenantRole{{TenantID: tid, UserID: uid, RoleCode: domain.RoleTenantAdmin}}, nil
	}}
	svc := service.NewMembershipService(mem, roles, &drhDeptMemRepo{}, nil, nil, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", userID.String())
	h.Get(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestMembershipGet_NotFound(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()
	mem := &mhMemRepo{findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return nil, domain.NewError(domain.ErrMemberNotFound, "no membership")
	}}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, nil, nil, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", userID.String())
	h.Get(c)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ── MembershipHandler.SeatUsage (P-27) — happy path ──────────────────

func TestMembershipSeatUsage_Success_200(t *testing.T) {
	tenantID := uuid.New()
	tenants := &happyTenantRepo{findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
		return &domain.Tenant{ID: id, LicensedSeats: 10}, nil
	}}
	mem := &mhMemRepo{countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 5, nil }}
	inv := &iahInviteRepo{countPendingFn: func(context.Context, uuid.UUID) (int, error) { return 2, nil }}
	svc := service.NewMembershipService(mem, &mhRoleRepo{}, &drhDeptMemRepo{}, nil, nil, tenants, inv, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.SeatUsage(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"active_users":5`)
	assert.Contains(t, w.Body.String(), `"pending_invitations":2`)
	assert.Contains(t, w.Body.String(), `"licensed_seats":10`)
	assert.Contains(t, w.Body.String(), `"over_cap":false`)
}

func TestMembershipSeatUsage_TenantNotFound(t *testing.T) {
	tenantID := uuid.New()
	tenants := &happyTenantRepo{findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
		return nil, domain.NewError(domain.ErrTenantNotFound, "tenant not found")
	}}
	svc := service.NewMembershipService(&mhMemRepo{}, &mhRoleRepo{}, &drhDeptMemRepo{}, nil, nil, tenants, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.SeatUsage(c)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ── local fakes with prefix "mh" ─────────────────────────────────────

type mhMemRepo struct {
	listFn        func(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error)
	findByUserFn  func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error)
	countActiveFn func(context.Context, uuid.UUID) (int, error)
	setStatusFn   func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error)
}

func (f *mhMemRepo) List(ctx context.Context, tid uuid.UUID, cur *domain.MembershipListCursor, limit int) (*domain.MembershipListPage, error) {
	if f.listFn != nil {
		return f.listFn(ctx, tid, cur, limit)
	}
	return &domain.MembershipListPage{}, nil
}
func (f *mhMemRepo) FindByUserID(ctx context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
	if f.findByUserFn != nil {
		return f.findByUserFn(ctx, tid, uid)
	}
	return nil, errors.New("not implemented")
}
func (f *mhMemRepo) Insert(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
	return nil, nil
}
func (f *mhMemRepo) SetStatus(ctx context.Context, tid, uid uuid.UUID, s domain.MembershipStatus, ver int64) (*domain.TenantMembership, error) {
	if f.setStatusFn != nil {
		return f.setStatusFn(ctx, tid, uid, s, ver)
	}
	return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid, Status: s, RecordVersion: ver + 1}, nil
}
func (f *mhMemRepo) SoftDelete(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil }
func (f *mhMemRepo) CountActive(ctx context.Context, tid uuid.UUID) (int, error) {
	if f.countActiveFn != nil {
		return f.countActiveFn(ctx, tid)
	}
	return 0, nil
}

type mhRoleRepo struct {
	listByUserFn func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error)
}

func (f *mhRoleRepo) ListByUser(ctx context.Context, tid, uid uuid.UUID) ([]domain.TenantRole, error) {
	if f.listByUserFn != nil {
		return f.listByUserFn(ctx, tid, uid)
	}
	return nil, nil
}
func (f *mhRoleRepo) ListByRole(context.Context, uuid.UUID, domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (f *mhRoleRepo) CountActiveOwners(context.Context, uuid.UUID) (int, error) {
	return 0, nil
}
func (f *mhRoleRepo) Grant(context.Context, *domain.TenantRole) (*domain.TenantRole, error) {
	return nil, nil
}
func (f *mhRoleRepo) Revoke(context.Context, uuid.UUID, uuid.UUID, domain.TenantRoleCode) (*domain.TenantRole, error) {
	return nil, nil
}
func (f *mhRoleRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}
