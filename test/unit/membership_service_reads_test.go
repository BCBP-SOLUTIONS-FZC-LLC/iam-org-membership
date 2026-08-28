// Unit tests for internal/core/service/membership_service.go read-side
// methods: List (P-4), Get (P-5), SeatUsage (P-27). RemoveUser,
// RemovalResolution, ValidateAndEmitAssigneeOverride, SetStatus require
// heavy TxRunner + Workflow + RP wiring and are covered by postgres
// integration tests.
package unit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── TenantRoleRepository stub ──────────────────────────────────────────

type fakeRoleRepo struct {
	listByUserFn func(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenantRole, error)
}

func (f *fakeRoleRepo) ListByUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenantRole, error) {
	return f.listByUserFn(ctx, tenantID, userID)
}
func (f *fakeRoleRepo) ListByRole(context.Context, uuid.UUID, domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (f *fakeRoleRepo) CountActiveOwners(context.Context, uuid.UUID) (int, error) {
	return 0, nil
}
func (f *fakeRoleRepo) Grant(context.Context, *domain.TenantRole) (*domain.TenantRole, error) {
	return nil, nil
}
func (f *fakeRoleRepo) Revoke(context.Context, uuid.UUID, uuid.UUID, domain.TenantRoleCode) (*domain.TenantRole, error) {
	return nil, nil
}
func (f *fakeRoleRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}

var _ port.TenantRoleRepository = (*fakeRoleRepo)(nil)

// ── TenantRepository stub ──────────────────────────────────────────────

type fakeTenantRepo struct {
	findByIDFn func(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)
}

func (f *fakeTenantRepo) FindByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return f.findByIDFn(ctx, id)
}

func (f *fakeTenantRepo) FindByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return f.FindByID(ctx, id)
}

func (f *fakeTenantRepo) Update(context.Context, uuid.UUID, *domain.TenantPatch) (*domain.Tenant, error) {
	return nil, nil
}
func (f *fakeTenantRepo) SetRealmSyncPending(context.Context, uuid.UUID) error { return nil }
func (f *fakeTenantRepo) Insert(context.Context, *domain.Tenant) (*domain.Tenant, bool, error) {
	return nil, false, nil
}

var _ port.TenantRepository = (*fakeTenantRepo)(nil)

// ── Extended MembershipRepository stub (adds List + CountActive) ───────

type fakeMemRepoFull struct {
	listFn        func(ctx context.Context, tenantID uuid.UUID, cursor *domain.MembershipListCursor, limit int) (*domain.MembershipListPage, error)
	findByUserFn  func(ctx context.Context, tenantID, userID uuid.UUID) (*domain.TenantMembership, error)
	countActiveFn func(ctx context.Context, tenantID uuid.UUID) (int, error)
}

func (f *fakeMemRepoFull) List(ctx context.Context, tenantID uuid.UUID, cursor *domain.MembershipListCursor, limit int) (*domain.MembershipListPage, error) {
	return f.listFn(ctx, tenantID, cursor, limit)
}
func (f *fakeMemRepoFull) FindByUserID(ctx context.Context, tenantID, userID uuid.UUID) (*domain.TenantMembership, error) {
	return f.findByUserFn(ctx, tenantID, userID)
}
func (f *fakeMemRepoFull) Insert(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
	return nil, nil
}
func (f *fakeMemRepoFull) SetStatus(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
	return nil, nil
}
func (f *fakeMemRepoFull) SoftDelete(context.Context, uuid.UUID, uuid.UUID, int64) error {
	return nil
}
func (f *fakeMemRepoFull) CountActive(ctx context.Context, tenantID uuid.UUID) (int, error) {
	return f.countActiveFn(ctx, tenantID)
}
func (f *fakeMemRepoFull) ListActiveUserIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

var _ port.MembershipRepository = (*fakeMemRepoFull)(nil)

// ── DeptMembershipRepository stub with ListByUser ──────────────────────

type fakeDeptMemListByUser struct {
	listByUserFn func(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.DeptMembership, error)
}

func (f *fakeDeptMemListByUser) ListByUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.DeptMembership, error) {
	return f.listByUserFn(ctx, tenantID, userID)
}
func (f *fakeDeptMemListByUser) ListByDepartment(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (f *fakeDeptMemListByUser) Assign(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, domain.DeptRole, uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
	return nil, nil, nil
}
func (f *fakeDeptMemListByUser) Remove(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.DeptMembership, error) {
	return nil, nil
}
func (f *fakeDeptMemListByUser) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (f *fakeDeptMemListByUser) SoftDeleteAllForDept(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}

var _ port.DeptMembershipRepository = (*fakeDeptMemListByUser)(nil)

// buildMembershipSvc wires MembershipService with the collaborators used
// by List/Get/SeatUsage. Others (delegations, rp, workflow, txRunner) stay
// nil.
func buildMembershipSvc(m port.MembershipRepository, r port.TenantRoleRepository, dm port.DeptMembershipRepository, t port.TenantRepository, inv port.InvitationRepository, cache port.Cache) *service.MembershipService {
	return service.NewMembershipService(
		m, r, dm, t, inv,
		cache, nil, nil, nil, nil, 30,
	)
}

// ── List (P-4) ─────────────────────────────────────────────────────────

func TestMembership_List_HydratesTenantRolesWithDerivedMember(t *testing.T) {
	tenantID := uuid.New()
	userA, userB := uuid.New(), uuid.New()
	page := &domain.MembershipListPage{
		Items: []domain.MembershipListItem{
			{Membership: domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userA}},
			{Membership: domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userB}},
		},
	}
	m := &fakeMemRepoFull{
		listFn: func(_ context.Context, tt uuid.UUID, _ *domain.MembershipListCursor, _ int) (*domain.MembershipListPage, error) {
			assert.Equal(t, tenantID, tt)
			return page, nil
		},
	}
	r := &fakeRoleRepo{
		listByUserFn: func(_ context.Context, _, uid uuid.UUID) ([]domain.TenantRole, error) {
			// userA is a tenant_admin; userB has no elevated role.
			if uid == userA {
				return []domain.TenantRole{{RoleCode: domain.RoleTenantAdmin}}, nil
			}
			return nil, nil
		},
	}
	dm := &fakeDeptMemListByUser{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) { return nil, nil },
	}
	svc := buildMembershipSvc(m, r, dm, nil, nil, nil)

	got, err := svc.List(context.Background(), tenantID, nil, 50)
	require.NoError(t, err)
	require.Len(t, got.Items, 2)
	// Every user gets the derived 'member' role prepended (TR-7).
	assert.Equal(t, []domain.TenantRoleCode{domain.RoleMember, domain.RoleTenantAdmin}, got.Items[0].TenantRoles)
	assert.Equal(t, []domain.TenantRoleCode{domain.RoleMember}, got.Items[1].TenantRoles)
}

func TestMembership_List_ListRepoErrorShortCircuits(t *testing.T) {
	repoErr := errors.New("db down")
	roleCalled := false
	m := &fakeMemRepoFull{
		listFn: func(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
			return nil, repoErr
		},
	}
	r := &fakeRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			roleCalled = true
			return nil, nil
		},
	}
	_, err := buildMembershipSvc(m, r, nil, nil, nil, nil).List(context.Background(), uuid.New(), nil, 50)
	assert.ErrorIs(t, err, repoErr)
	assert.False(t, roleCalled, "roles must not be queried when the primary List failed")
}

func TestMembership_List_RolesFetchErrorAborts(t *testing.T) {
	page := &domain.MembershipListPage{
		Items: []domain.MembershipListItem{{Membership: domain.TenantMembership{UserID: uuid.New()}}},
	}
	rolesErr := errors.New("roles denied")
	m := &fakeMemRepoFull{
		listFn: func(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
			return page, nil
		},
	}
	r := &fakeRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, rolesErr
		},
	}
	_, err := buildMembershipSvc(m, r, nil, nil, nil, nil).List(context.Background(), uuid.New(), nil, 50)
	assert.ErrorIs(t, err, rolesErr)
}

// ── Get (P-5) ──────────────────────────────────────────────────────────

func TestMembership_Get_ComposesRolesAndDeptMemberships(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	deptA, deptB := uuid.New(), uuid.New()
	m := &fakeMemRepoFull{
		findByUserFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
			assert.Equal(t, tenantID, tt)
			assert.Equal(t, userID, uu)
			return &domain.TenantMembership{ID: uuid.New(), TenantID: tt, UserID: uu, Status: domain.MembershipActive}, nil
		},
	}
	r := &fakeRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenderAdmin}}, nil
		},
	}
	dm := &fakeDeptMemListByUser{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			return []domain.DeptMembership{
				{DepartmentID: deptA, RoleLevel: domain.DeptApprover},
				{DepartmentID: deptB, RoleLevel: domain.DeptReviewer},
			}, nil
		},
	}
	svc := buildMembershipSvc(m, r, dm, nil, nil, nil)

	got, err := svc.Get(context.Background(), tenantID, userID)
	require.NoError(t, err)
	assert.Equal(t, []domain.TenantRoleCode{domain.RoleMember, domain.RoleTenderAdmin}, got.TenantRoles)
	require.Len(t, got.Departments, 2)
	assert.Equal(t, deptA, got.Departments[0].DepartmentID)
	assert.Equal(t, domain.DeptApprover, got.Departments[0].RoleLevel)
	assert.Equal(t, deptB, got.Departments[1].DepartmentID)
}

func TestMembership_Get_MembershipNotFoundSurfaces(t *testing.T) {
	m := &fakeMemRepoFull{
		findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return nil, domain.NewError(domain.ErrMemberNotFound, "no such member")
		},
	}
	_, err := buildMembershipSvc(m, nil, nil, nil, nil, nil).Get(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrMemberNotFound)
}

func TestMembership_Get_RolesFetchErrorAborts(t *testing.T) {
	m := &fakeMemRepoFull{
		findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New()}, nil
		},
	}
	rolesErr := errors.New("roles fetch failed")
	r := &fakeRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, rolesErr
		},
	}
	_, err := buildMembershipSvc(m, r, nil, nil, nil, nil).Get(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, rolesErr)
}

func TestMembership_Get_DeptFetchErrorAborts(t *testing.T) {
	m := &fakeMemRepoFull{
		findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New()}, nil
		},
	}
	r := &fakeRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil
		},
	}
	deptErr := errors.New("dept fetch failed")
	dm := &fakeDeptMemListByUser{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			return nil, deptErr
		},
	}
	_, err := buildMembershipSvc(m, r, dm, nil, nil, nil).Get(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, deptErr)
}

// ── CheckActiveMembership (ADR-0007 Wave 3 Phase 3 — backs the new GET
// /tenants/:id/members/:user_id/exists route iam-tender-acl depends on) ──
//
// Deliberately a thin FindByUserID wrapper (see the method's own doc
// comment): these tests confirm it does NOT touch roles/dept memberships
// at all, unlike Get() — a nil roles/dept repo must never be dereferenced.

func TestMembership_CheckActiveMembership_ActiveMember_ReturnsMembership(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	membershipID := uuid.New()
	m := &fakeMemRepoFull{
		findByUserFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
			assert.Equal(t, tenantID, tt)
			assert.Equal(t, userID, uu)
			return &domain.TenantMembership{ID: membershipID, TenantID: tt, UserID: uu, Status: domain.MembershipActive}, nil
		},
	}
	got, err := buildMembershipSvc(m, nil, nil, nil, nil, nil).CheckActiveMembership(context.Background(), tenantID, userID)
	require.NoError(t, err)
	assert.Equal(t, membershipID, got.ID)
	assert.Equal(t, domain.MembershipActive, got.Status)
}

func TestMembership_CheckActiveMembership_SuspendedMember_ReturnsMembershipWithNonActiveStatus(t *testing.T) {
	// The service returns the membership as-is, whatever its status —
	// the caller (CheckMemberExists handler) is responsible for the
	// active-vs-not-active decision, not this method.
	m := &fakeMemRepoFull{
		findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), Status: domain.MembershipSuspended}, nil
		},
	}
	got, err := buildMembershipSvc(m, nil, nil, nil, nil, nil).CheckActiveMembership(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Equal(t, domain.MembershipSuspended, got.Status)
}

func TestMembership_CheckActiveMembership_NotFoundSurfaces(t *testing.T) {
	m := &fakeMemRepoFull{
		findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return nil, domain.NewError(domain.ErrMemberNotFound, "no such member")
		},
	}
	_, err := buildMembershipSvc(m, nil, nil, nil, nil, nil).CheckActiveMembership(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrMemberNotFound)
}

func TestMembership_CheckActiveMembership_RepoErrorSurfaces(t *testing.T) {
	repoErr := errors.New("db down")
	m := &fakeMemRepoFull{
		findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return nil, repoErr
		},
	}
	_, err := buildMembershipSvc(m, nil, nil, nil, nil, nil).CheckActiveMembership(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, repoErr)
}

// ── SeatUsage (P-27) ───────────────────────────────────────────────────

func TestMembership_SeatUsage_UnderCap(t *testing.T) {
	tenantID := uuid.New()
	tenants := &fakeTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			assert.Equal(t, tenantID, id)
			return &domain.Tenant{ID: id, LicensedSeats: 10}, nil
		},
	}
	m := &fakeMemRepoFull{
		countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 3, nil },
	}
	inv := &fakeInviteRepo{
		countPendingFn: func(context.Context, uuid.UUID) (int, error) { return 2, nil },
	}
	// Non-nil cache exercises the cache parameter on the builder — the
	// service ignores it in SeatUsage but the wiring must accept it.
	svc := buildMembershipSvc(m, nil, nil, tenants, inv, &spyCache{})

	got, err := svc.SeatUsage(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, 3, got.ActiveUsers)
	assert.Equal(t, 2, got.PendingInvitations)
	assert.Equal(t, 10, got.LicensedSeats)
	assert.False(t, got.OverCap, "3+2 = 5 <= 10")
	assert.Nil(t, got.OverageSince)
	assert.Nil(t, got.GraceEndsAt)
}

func TestMembership_SeatUsage_AtCapExactBoundaryIsOver(t *testing.T) {
	// SEAT-3: OverCap := active + pending >= licensed_seats. At equality, IS overage.
	tenants := &fakeTenantRepo{
		findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{LicensedSeats: 5}, nil
		},
	}
	m := &fakeMemRepoFull{countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 3, nil }}
	inv := &fakeInviteRepo{countPendingFn: func(context.Context, uuid.UUID) (int, error) { return 2, nil }}
	svc := buildMembershipSvc(m, nil, nil, tenants, inv, nil)

	got, err := svc.SeatUsage(context.Background(), uuid.New())
	require.NoError(t, err)
	assert.True(t, got.OverCap, "SEAT-3: at-cap (active+pending==licensed) is overage")
}

func TestMembership_SeatUsage_OverCapComputesGraceEndsAt(t *testing.T) {
	overageSince := time.Now().Add(-24 * time.Hour)
	tenants := &fakeTenantRepo{
		findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{LicensedSeats: 5, OverageSince: &overageSince}, nil
		},
	}
	m := &fakeMemRepoFull{countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 4, nil }}
	inv := &fakeInviteRepo{countPendingFn: func(context.Context, uuid.UUID) (int, error) { return 3, nil }}

	// seatOverageDays baked into buildMembershipSvc is 30.
	svc := buildMembershipSvc(m, nil, nil, tenants, inv, nil)

	got, err := svc.SeatUsage(context.Background(), uuid.New())
	require.NoError(t, err)
	assert.True(t, got.OverCap)
	require.NotNil(t, got.GraceEndsAt)
	expected := overageSince.Add(30 * 24 * time.Hour)
	assert.WithinDuration(t, expected, *got.GraceEndsAt, time.Second)
}

func TestMembership_SeatUsage_TenantFindErrorSurfaces(t *testing.T) {
	notFound := errors.New("tenant not found")
	tenants := &fakeTenantRepo{
		findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
			return nil, notFound
		},
	}
	svc := buildMembershipSvc(nil, nil, nil, tenants, nil, nil)

	_, err := svc.SeatUsage(context.Background(), uuid.New())
	assert.ErrorIs(t, err, notFound)
}

func TestMembership_SeatUsage_CountActiveErrorSurfaces(t *testing.T) {
	tenants := &fakeTenantRepo{
		findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{LicensedSeats: 10}, nil
		},
	}
	countErr := errors.New("count active failed")
	m := &fakeMemRepoFull{countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 0, countErr }}
	svc := buildMembershipSvc(m, nil, nil, tenants, nil, nil)

	_, err := svc.SeatUsage(context.Background(), uuid.New())
	assert.ErrorIs(t, err, countErr)
}

func TestMembership_SeatUsage_CountPendingErrorSurfaces(t *testing.T) {
	tenants := &fakeTenantRepo{
		findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{LicensedSeats: 10}, nil
		},
	}
	m := &fakeMemRepoFull{countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 3, nil }}
	pendingErr := errors.New("count pending failed")
	inv := &fakeInviteRepo{countPendingFn: func(context.Context, uuid.UUID) (int, error) { return 0, pendingErr }}
	svc := buildMembershipSvc(m, nil, nil, tenants, inv, nil)

	_, err := svc.SeatUsage(context.Background(), uuid.New())
	assert.ErrorIs(t, err, pendingErr)
}
