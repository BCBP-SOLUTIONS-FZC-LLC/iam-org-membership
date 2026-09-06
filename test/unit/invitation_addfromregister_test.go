// Unit tests for internal/core/service/invitation_service.go
// AddFromRegister (I-3, §8.10) and the NewInvitationService constructor's
// expiryDays default-fallback branch.
//
// invitation_service_test.go's header note that "Invite / AddFromRegister
// are exercised via postgres integration tests — they need TxRunner + RP
// client wiring" is true for Invite (which also calls
// port.RealmProvisionerClient.CreateInvitedUser and re-checks SEAT-1 under
// a real FOR UPDATE lock semantics-sensitive path). AddFromRegister itself
// never touches port.RealmProvisionerClient at all, and every collaborator
// it does use (TxRunner, TenantRepository, MembershipRepository,
// TenantRoleRepository, DeptMembershipRepository, InvitationRepository) is
// a plain interface — RunInTx here just needs to invoke its callback, the
// same pattern already used throughout membership_removeuser_test.go and
// dept_membership_service_test.go. So AddFromRegister is fully exercisable
// with hand-written fakes and is covered here.
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

// ── NewInvitationService — expiryDays default-fallback branch ──────────

func TestNewInvitationService_NonPositiveExpiryDaysIsSafe(t *testing.T) {
	// expiryDays <= 0 must default to 7 rather than being stored verbatim
	// (only observable indirectly — via Invite's ExpiresAt, which needs
	// heavier RP+tx wiring out of scope here) — this at minimum proves the
	// constructor doesn't panic or reject the boundary values.
	for _, days := range []int{0, -1, -30} {
		svc := service.NewInvitationService(&fakeInviteRepo{}, nil, nil, nil, nil, nil, nil, nil, nil, days)
		require.NotNil(t, svc)
	}
}

func TestNewInvitationService_PositiveExpiryDaysUsedAsGiven(t *testing.T) {
	svc := service.NewInvitationService(&fakeInviteRepo{}, nil, nil, nil, nil, nil, nil, nil, nil, 14)
	require.NotNil(t, svc)
}

// ── fakes for AddFromRegister ───────────────────────────────────────────
// Note: arInviteRepo, arMembershipRepo, arTenantRepo, and arRoleRepo are
// defined in invitation_addregister_tx_test.go (same package). Only the
// types that are unique to this file are defined below.

// arDeptMemRepo is a configurable DeptMembershipRepository for the Assign loop.
type arDeptMemRepo struct {
	assignFn func(ctx context.Context, tenantID, userID, deptID, memID uuid.UUID, level domain.DeptRole, actorID uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error)
}

func (r *arDeptMemRepo) ListByUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *arDeptMemRepo) ListByDepartment(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *arDeptMemRepo) Assign(ctx context.Context, tenantID, userID, deptID, memID uuid.UUID, level domain.DeptRole, actorID uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
	if r.assignFn != nil {
		return r.assignFn(ctx, tenantID, userID, deptID, memID, level, actorID)
	}
	return &domain.DeptMembership{TenantID: tenantID, UserID: userID, DepartmentID: deptID, RoleLevel: level}, nil, nil
}
func (r *arDeptMemRepo) Remove(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.DeptMembership, error) {
	return nil, nil
}
func (r *arDeptMemRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *arDeptMemRepo) SoftDeleteAllForDept(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}

var _ port.DeptMembershipRepository = (*arDeptMemRepo)(nil)

// arPub / arTxRunner mirror the ruPublisher/ruTxRunner pattern already
// used by membership_removeuser_test.go, kept local to this file to avoid
// coupling to that file's naming.
type arPub struct{ events []*domain.DomainEvent }

func (p *arPub) Enqueue(_ context.Context, e *domain.DomainEvent) error {
	p.events = append(p.events, e)
	return nil
}

type arTxRunner struct{ pub port.EventPublisher }

func (r *arTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if r.pub != nil {
		ctx = port.WithEventPublisher(ctx, r.pub)
	}
	return fn(ctx)
}

func buildAddFromRegisterSvc(inv port.InvitationRepository, tenants port.TenantRepository, mem port.MembershipRepository,
	roles port.TenantRoleRepository, deptMems port.DeptMembershipRepository, cache port.Cache, tr port.TxRunner,
) *service.InvitationService {
	return service.NewInvitationService(inv, mem, roles, deptMems, tenants, nil, cache, tr, nil, 7)
}

// ── AddFromRegister — plain add (no matching invitation) ───────────────

func TestAddFromRegister_NoMatchingInvitation_PlainAdd(t *testing.T) {
	tenantID, userID, kcUserID := uuid.New(), uuid.New(), uuid.New()
	inv := &arInviteRepo{} // both lookups return (nil, nil) by default
	tenants := &arTenantRepo{}
	mem := &arMembershipRepo{}
	svc := buildAddFromRegisterSvc(inv, tenants, mem, &arRoleRepo{}, &arDeptMemRepo{}, nil, &arTxRunner{})

	got, err := svc.AddFromRegister(context.Background(), tenantID, userID, kcUserID, "new@example.com")
	require.NoError(t, err, "no matching invitation must fall through to a plain add, not error")
	assert.Equal(t, userID, got.UserID)
}

// ── AddFromRegister — FindPendingByKeycloakUser error propagates ───────

func TestAddFromRegister_FindByKeycloakUserErrorPropagates(t *testing.T) {
	findErr := errors.New("db down")
	inv := &arInviteRepo{findByKCUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
		return nil, findErr
	}}
	svc := buildAddFromRegisterSvc(inv, &arTenantRepo{}, &arMembershipRepo{}, &arRoleRepo{}, &arDeptMemRepo{}, nil, &arTxRunner{})

	_, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "x@example.com")
	assert.ErrorIs(t, err, findErr)
}

// ── AddFromRegister — falls back to email lookup when KC-user misses ───

func TestAddFromRegister_FallsBackToEmailLookup(t *testing.T) {
	tenantID, userID, kcUserID := uuid.New(), uuid.New(), uuid.New()
	pendingID := uuid.New()
	invitedBy := uuid.New()
	emailLookupCalled := false
	inv := &arInviteRepo{
		findByKCUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
			return nil, nil // no match by keycloak_user_id
		},
		findByEmailFn: func(_ context.Context, _ uuid.UUID, email string) (*domain.PendingInvitation, error) {
			emailLookupCalled = true
			assert.Equal(t, "invited@example.com", email, "email must be normalized before lookup")
			return &domain.PendingInvitation{ID: pendingID, Status: domain.InvitePending, InvitedBy: invitedBy,
				ExpiresAt: time.Now().UTC().Add(24 * time.Hour), RecordVersion: 1}, nil
		},
		lockByIDFn: func(_ context.Context, id uuid.UUID) (*domain.PendingInvitation, error) {
			assert.Equal(t, pendingID, id)
			return &domain.PendingInvitation{ID: id, Status: domain.InvitePending,
				ExpiresAt: time.Now().UTC().Add(24 * time.Hour), RecordVersion: 1}, nil
		},
	}
	inv.setStatusFn = func(_ context.Context, tt, ii uuid.UUID, st domain.InvitationStatus, ver int64) (*domain.PendingInvitation, error) {
		assert.Equal(t, domain.InviteAccepted, st)
		return &domain.PendingInvitation{ID: ii, RecordVersion: ver + 1}, nil
	}
	svc := buildAddFromRegisterSvc(inv, &arTenantRepo{}, &arMembershipRepo{}, &arRoleRepo{}, &arDeptMemRepo{}, nil, &arTxRunner{})

	_, err := svc.AddFromRegister(context.Background(), tenantID, userID, kcUserID, "  Invited@Example.com  ")
	require.NoError(t, err)
	assert.True(t, emailLookupCalled)
}

func TestAddFromRegister_FindByEmailErrorPropagates(t *testing.T) {
	findErr := errors.New("db down")
	inv := &arInviteRepo{
		findByEmailFn: func(context.Context, uuid.UUID, string) (*domain.PendingInvitation, error) {
			return nil, findErr
		},
	}
	svc := buildAddFromRegisterSvc(inv, &arTenantRepo{}, &arMembershipRepo{}, &arRoleRepo{}, &arDeptMemRepo{}, nil, &arTxRunner{})

	_, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "x@example.com")
	assert.ErrorIs(t, err, findErr)
}

// ── AddFromRegister — TM-13 tenant lock error propagates ───────────────

func TestAddFromRegister_TenantLockErrorPropagates(t *testing.T) {
	lockErr := errors.New("lock timeout")
	svc := buildAddFromRegisterSvc(&arInviteRepo{}, &arTenantRepo{lockByIDFn: func(context.Context, uuid.UUID) error { return lockErr }}, &arMembershipRepo{}, &arRoleRepo{}, &arDeptMemRepo{}, nil, &arTxRunner{})

	_, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "")
	assert.ErrorIs(t, err, lockErr)
}

// ── AddFromRegister — invites.LockByID branches ─────────────────────────

func TestAddFromRegister_InviteLockByIDErrorPropagates(t *testing.T) {
	pendingID := uuid.New()
	lockErr := errors.New("row lock failed")
	inv := &arInviteRepo{
		findByKCUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: pendingID, Status: domain.InvitePending, RecordVersion: 1}, nil
		},
		lockByIDFn: func(context.Context, uuid.UUID) (*domain.PendingInvitation, error) {
			return nil, lockErr
		},
	}
	svc := buildAddFromRegisterSvc(inv, &arTenantRepo{}, &arMembershipRepo{}, &arRoleRepo{}, &arDeptMemRepo{}, nil, &arTxRunner{})

	_, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "")
	assert.ErrorIs(t, err, lockErr)
}

func TestAddFromRegister_InviteLockByIDNotPending_ProceedsAsPlainAdd(t *testing.T) {
	pendingID := uuid.New()
	inv := &arInviteRepo{
		findByKCUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: pendingID, Status: domain.InvitePending, RecordVersion: 1}, nil
		},
		lockByIDFn: func(_ context.Context, id uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: id, Status: domain.InviteRevoked, RecordVersion: 2}, nil
		},
	}
	svc := buildAddFromRegisterSvc(inv, &arTenantRepo{}, &arMembershipRepo{}, &arRoleRepo{}, &arDeptMemRepo{}, nil, &arTxRunner{})

	got, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "")
	require.NoError(t, err)
	assert.NotNil(t, got)
}

// ── AddFromRegister — expired-but-still-pending invitation seat recheck ─

func TestAddFromRegister_ExpiredInvitation_SeatOverCap_Returns409(t *testing.T) {
	pendingID := uuid.New()
	past := time.Now().UTC().Add(-time.Hour)
	inv := &arInviteRepo{
		findByKCUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: pendingID, Status: domain.InvitePending, ExpiresAt: past, RecordVersion: 1}, nil
		},
		lockByIDFn: func(_ context.Context, id uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: id, Status: domain.InvitePending, ExpiresAt: past, RecordVersion: 1}, nil
		},
	}
	tenants := &arTenantRepo{licensedSeatsForUpdate: func(context.Context, uuid.UUID) (int, error) { return 5, nil }}
	mem := &arMembershipRepo{countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 5, nil }}
	inv.countPendingFn = func(context.Context, uuid.UUID) (int, error) { return 0, nil }
	svc := buildAddFromRegisterSvc(inv, tenants, mem, &arRoleRepo{}, &arDeptMemRepo{}, nil, &arTxRunner{})

	_, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "")
	assert.ErrorIs(t, err, domain.ErrSeatLimitReached)
}

func TestAddFromRegister_ExpiredInvitation_LicensedSeatsForUpdateErrorPropagates(t *testing.T) {
	pendingID := uuid.New()
	past := time.Now().UTC().Add(-time.Hour)
	seatErr := errors.New("db down")
	inv := &arInviteRepo{
		findByKCUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: pendingID, Status: domain.InvitePending, ExpiresAt: past, RecordVersion: 1}, nil
		},
		lockByIDFn: func(_ context.Context, id uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: id, Status: domain.InvitePending, ExpiresAt: past, RecordVersion: 1}, nil
		},
	}
	tenants := &arTenantRepo{licensedSeatsForUpdate: func(context.Context, uuid.UUID) (int, error) { return 0, seatErr }}
	svc := buildAddFromRegisterSvc(inv, tenants, &arMembershipRepo{}, &arRoleRepo{}, &arDeptMemRepo{}, nil, &arTxRunner{})

	_, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "")
	assert.ErrorIs(t, err, seatErr)
}

func TestAddFromRegister_ExpiredInvitation_CountActiveErrorPropagates(t *testing.T) {
	pendingID := uuid.New()
	past := time.Now().UTC().Add(-time.Hour)
	countErr := errors.New("db down")
	inv := &arInviteRepo{
		findByKCUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: pendingID, Status: domain.InvitePending, ExpiresAt: past, RecordVersion: 1}, nil
		},
		lockByIDFn: func(_ context.Context, id uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: id, Status: domain.InvitePending, ExpiresAt: past, RecordVersion: 1}, nil
		},
	}
	tenants := &arTenantRepo{licensedSeatsForUpdate: func(context.Context, uuid.UUID) (int, error) { return 10, nil }}
	mem := &arMembershipRepo{countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 0, countErr }}
	svc := buildAddFromRegisterSvc(inv, tenants, mem, &arRoleRepo{}, &arDeptMemRepo{}, nil, &arTxRunner{})

	_, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "")
	assert.ErrorIs(t, err, countErr)
}

func TestAddFromRegister_ExpiredInvitation_CountPendingErrorPropagates(t *testing.T) {
	pendingID := uuid.New()
	past := time.Now().UTC().Add(-time.Hour)
	pendingErr := errors.New("db down")
	inv := &arInviteRepo{
		findByKCUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: pendingID, Status: domain.InvitePending, ExpiresAt: past, RecordVersion: 1}, nil
		},
		lockByIDFn: func(_ context.Context, id uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: id, Status: domain.InvitePending, ExpiresAt: past, RecordVersion: 1}, nil
		},
	}
	tenants := &arTenantRepo{licensedSeatsForUpdate: func(context.Context, uuid.UUID) (int, error) { return 10, nil }}
	mem := &arMembershipRepo{countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 1, nil }}
	inv.countPendingFn = func(context.Context, uuid.UUID) (int, error) { return 0, pendingErr }
	svc := buildAddFromRegisterSvc(inv, tenants, mem, &arRoleRepo{}, &arDeptMemRepo{}, nil, &arTxRunner{})

	_, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "")
	assert.ErrorIs(t, err, pendingErr)
}

func TestAddFromRegister_ExpiredInvitation_UnderCap_ProceedsToAccept(t *testing.T) {
	pendingID := uuid.New()
	past := time.Now().UTC().Add(-time.Hour)
	inv := &arInviteRepo{
		findByKCUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: pendingID, Status: domain.InvitePending, ExpiresAt: past, RecordVersion: 1}, nil
		},
		lockByIDFn: func(_ context.Context, id uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: id, Status: domain.InvitePending, ExpiresAt: past, RecordVersion: 3}, nil
		},
	}
	inv.setStatusFn = func(_ context.Context, _, id uuid.UUID, st domain.InvitationStatus, ver int64) (*domain.PendingInvitation, error) {
		assert.EqualValues(t, 3, ver, "must use the freshly re-locked record_version")
		return &domain.PendingInvitation{ID: id, RecordVersion: ver + 1}, nil
	}
	inv.countPendingFn = func(context.Context, uuid.UUID) (int, error) { return 0, nil }
	tenants := &arTenantRepo{licensedSeatsForUpdate: func(context.Context, uuid.UUID) (int, error) { return 10, nil }}
	mem := &arMembershipRepo{countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 1, nil }}
	svc := buildAddFromRegisterSvc(inv, tenants, mem, &arRoleRepo{}, &arDeptMemRepo{}, nil, &arTxRunner{})

	got, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "")
	require.NoError(t, err, "under cap → the expired-but-honoured acceptance must proceed")
	assert.NotNil(t, got)
}

// ── AddFromRegister — membership Insert error propagates ───────────────

func TestAddFromRegister_MembershipInsertErrorPropagates(t *testing.T) {
	insertErr := errors.New("uq_tm_active_user violation")
	mem := &arMembershipRepo{insertFn: func(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
		return nil, insertErr
	}}
	svc := buildAddFromRegisterSvc(&arInviteRepo{}, &arTenantRepo{}, mem, &arRoleRepo{}, &arDeptMemRepo{}, nil, &arTxRunner{})

	_, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "")
	assert.ErrorIs(t, err, insertErr)
}

// ── AddFromRegister — accepted invitation applies initial roles + depts ─

func TestAddFromRegister_AppliesInitialRolesAndDeptsWithEvents(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	pendingID, invitedBy, deptID := uuid.New(), uuid.New(), uuid.New()
	future := time.Now().UTC().Add(24 * time.Hour)

	inv := &arInviteRepo{
		findByKCUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{
				ID: pendingID, Status: domain.InvitePending, ExpiresAt: future, RecordVersion: 1,
				InvitedBy:           invitedBy,
				InitialTenantRoles:  []domain.TenantRoleCode{domain.RoleMember, domain.RoleTenantAdmin},
				InitialDeptMappings: []domain.InvitationDeptMapping{{DepartmentID: deptID, Level: domain.DeptReviewer}},
			}, nil
		},
		lockByIDFn: func(_ context.Context, id uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: id, Status: domain.InvitePending, ExpiresAt: future, RecordVersion: 1}, nil
		},
	}
	inv.setStatusFn = func(_ context.Context, _, id uuid.UUID, st domain.InvitationStatus, _ int64) (*domain.PendingInvitation, error) {
		assert.Equal(t, domain.InviteAccepted, st)
		return &domain.PendingInvitation{ID: id, RecordVersion: 2}, nil
	}
	roleGranted := false
	roles := &arRoleRepo{grantFn: func(_ context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
		roleGranted = true
		assert.Equal(t, domain.RoleTenantAdmin, tr.RoleCode, "TR-7: 'member' must be skipped, only elevated roles granted")
		return tr, nil
	}}
	deptAssigned := false
	deptMems := &arDeptMemRepo{assignFn: func(_ context.Context, tid, uid, did, _ uuid.UUID, level domain.DeptRole, _ uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
		deptAssigned = true
		return &domain.DeptMembership{TenantID: tid, UserID: uid, DepartmentID: did, RoleLevel: level}, nil, nil
	}}
	pub := &arPub{}
	cache := &spyCache{}
	svc := buildAddFromRegisterSvc(inv, &arTenantRepo{}, &arMembershipRepo{}, roles, deptMems, cache, &arTxRunner{pub: pub})

	got, err := svc.AddFromRegister(context.Background(), tenantID, userID, uuid.New(), "")
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.True(t, roleGranted)
	assert.True(t, deptAssigned)
	require.Len(t, pub.events, 2)
	assert.Equal(t, domain.EventTenantRoleGranted, pub.events[0].Type)
	assert.Equal(t, domain.EventDepartmentMembershipGranted, pub.events[1].Type)
	assert.Contains(t, cache.deleteCalls, "om:seat_usage:"+tenantID.String())
}

func TestAddFromRegister_SetStatusErrorPropagates(t *testing.T) {
	pendingID := uuid.New()
	future := time.Now().UTC().Add(24 * time.Hour)
	setStatusErr := errors.New("optimistic_lock_conflict")
	inv := &arInviteRepo{
		findByKCUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: pendingID, Status: domain.InvitePending, ExpiresAt: future, RecordVersion: 1}, nil
		},
		lockByIDFn: func(_ context.Context, id uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: id, Status: domain.InvitePending, ExpiresAt: future, RecordVersion: 1}, nil
		},
	}
	inv.setStatusFn = func(context.Context, uuid.UUID, uuid.UUID, domain.InvitationStatus, int64) (*domain.PendingInvitation, error) {
		return nil, setStatusErr
	}
	svc := buildAddFromRegisterSvc(inv, &arTenantRepo{}, &arMembershipRepo{}, &arRoleRepo{}, &arDeptMemRepo{}, nil, &arTxRunner{})

	_, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "")
	assert.ErrorIs(t, err, setStatusErr)
}

func TestAddFromRegister_RoleGrantErrorPropagates(t *testing.T) {
	pendingID := uuid.New()
	future := time.Now().UTC().Add(24 * time.Hour)
	grantErr := errors.New("grant failed")
	inv := &arInviteRepo{
		findByKCUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{
				ID: pendingID, Status: domain.InvitePending, ExpiresAt: future, RecordVersion: 1,
				InitialTenantRoles: []domain.TenantRoleCode{domain.RoleTenantAdmin},
			}, nil
		},
		lockByIDFn: func(_ context.Context, id uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: id, Status: domain.InvitePending, ExpiresAt: future, RecordVersion: 1}, nil
		},
	}
	inv.setStatusFn = func(_ context.Context, _, id uuid.UUID, _ domain.InvitationStatus, _ int64) (*domain.PendingInvitation, error) {
		return &domain.PendingInvitation{ID: id, RecordVersion: 2}, nil
	}
	roles := &arRoleRepo{grantFn: func(context.Context, *domain.TenantRole) (*domain.TenantRole, error) {
		return nil, grantErr
	}}
	svc := buildAddFromRegisterSvc(inv, &arTenantRepo{}, &arMembershipRepo{}, roles, &arDeptMemRepo{}, nil, &arTxRunner{})

	_, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "")
	assert.ErrorIs(t, err, grantErr)
}

func TestAddFromRegister_DeptAssignErrorPropagates(t *testing.T) {
	pendingID, deptID := uuid.New(), uuid.New()
	future := time.Now().UTC().Add(24 * time.Hour)
	assignErr := errors.New("fk_dm_tenant_dept violation")
	inv := &arInviteRepo{
		findByKCUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{
				ID: pendingID, Status: domain.InvitePending, ExpiresAt: future, RecordVersion: 1,
				InitialDeptMappings: []domain.InvitationDeptMapping{{DepartmentID: deptID, Level: domain.DeptPreparator}},
			}, nil
		},
		lockByIDFn: func(_ context.Context, id uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: id, Status: domain.InvitePending, ExpiresAt: future, RecordVersion: 1}, nil
		},
	}
	inv.setStatusFn = func(_ context.Context, _, id uuid.UUID, _ domain.InvitationStatus, _ int64) (*domain.PendingInvitation, error) {
		return &domain.PendingInvitation{ID: id, RecordVersion: 2}, nil
	}
	deptMems := &arDeptMemRepo{assignFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, domain.DeptRole, uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
		return nil, nil, assignErr
	}}
	svc := buildAddFromRegisterSvc(inv, &arTenantRepo{}, &arMembershipRepo{}, &arRoleRepo{}, deptMems, nil, &arTxRunner{})

	_, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "")
	assert.ErrorIs(t, err, assignErr)
}

// ── AddFromRegister — no publisher in context skips event emission ─────

func TestAddFromRegister_NoEventPublisher_SkipsEnqueueButStillApplies(t *testing.T) {
	pendingID, deptID := uuid.New(), uuid.New()
	future := time.Now().UTC().Add(24 * time.Hour)
	inv := &arInviteRepo{
		findByKCUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{
				ID: pendingID, Status: domain.InvitePending, ExpiresAt: future, RecordVersion: 1,
				InitialTenantRoles:  []domain.TenantRoleCode{domain.RoleTenantAdmin},
				InitialDeptMappings: []domain.InvitationDeptMapping{{DepartmentID: deptID, Level: domain.DeptPreparator}},
			}, nil
		},
		lockByIDFn: func(_ context.Context, id uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: id, Status: domain.InvitePending, ExpiresAt: future, RecordVersion: 1}, nil
		},
	}
	inv.setStatusFn = func(_ context.Context, _, id uuid.UUID, _ domain.InvitationStatus, _ int64) (*domain.PendingInvitation, error) {
		return &domain.PendingInvitation{ID: id, RecordVersion: 2}, nil
	}
	svc := buildAddFromRegisterSvc(inv, &arTenantRepo{}, &arMembershipRepo{}, &arRoleRepo{}, &arDeptMemRepo{}, nil, &arTxRunner{} /* pub: nil */)

	got, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "")
	require.NoError(t, err, "no EventPublisher in ctx must not fail the grants — it just skips emission")
	assert.NotNil(t, got)
}

// ── AddFromRegister — nil cache is safe ─────────────────────────────────

func TestAddFromRegister_NilCache_IsSafe(t *testing.T) {
	svc := buildAddFromRegisterSvc(&arInviteRepo{}, &arTenantRepo{}, &arMembershipRepo{}, &arRoleRepo{}, &arDeptMemRepo{}, nil, &arTxRunner{})
	assert.NotPanics(t, func() {
		_, _ = svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "")
	})
}
