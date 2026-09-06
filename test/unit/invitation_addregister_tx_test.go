// invitation_addregister_tx_test.go covers the AddFromRegister transaction
// body branches that were uncovered at 69.2%:
//
//   - tenants.LockByID error propagates from RunInTx (line 354)
//   - invites.LockByID returns nil (row vanished) → plain add (line 369)
//   - invites.LockByID returns status != InvitePending → plain add (line 371)
//   - memberships.Insert error propagates (line 402-406)
//   - plain add succeeds: pending == nil → no role grants (line 337-350 + 402-408)
//   - pending != nil → SetStatus + role grants applied (lines 411-455)
//   - cache eviction after tx: Delete(cacheKeySeatUsage) (lines 462-464)
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

// ── stubs ─────────────────────────────────────────────────────────────────────

// arInviteRepo is an InvitationRepository for AddFromRegister tests with
// configurable fns for the methods the tx body calls.
type arInviteRepo struct {
	port.InvitationRepositoryNoop
	findByKCUserFn        func(ctx context.Context, tenantID, kcUserID uuid.UUID) (*domain.PendingInvitation, error)
	findByEmailFn         func(ctx context.Context, tenantID uuid.UUID, email string) (*domain.PendingInvitation, error)
	lockByIDFn            func(ctx context.Context, id uuid.UUID) (*domain.PendingInvitation, error)
	setStatusFn           func(ctx context.Context, tenantID, id uuid.UUID, status domain.InvitationStatus, ver int64) (*domain.PendingInvitation, error)
	countPendingFn        func(ctx context.Context, tenantID uuid.UUID) (int, error)
	mostRecentAtFn        func(ctx context.Context, tenantID uuid.UUID, email string) (time.Time, error)
	countWindowFn         func(ctx context.Context, tenantID uuid.UUID, since time.Time) (int, error)
	setKCCleanupPendingFn func(ctx context.Context, tenantID, id uuid.UUID, pending bool, ver int64) error
}

func (r *arInviteRepo) FindPendingByKeycloakUser(ctx context.Context, tenantID, kcUserID uuid.UUID) (*domain.PendingInvitation, error) {
	if r.findByKCUserFn != nil {
		return r.findByKCUserFn(ctx, tenantID, kcUserID)
	}
	return nil, nil
}
func (r *arInviteRepo) FindPendingByEmail(ctx context.Context, tenantID uuid.UUID, email string) (*domain.PendingInvitation, error) {
	if r.findByEmailFn != nil {
		return r.findByEmailFn(ctx, tenantID, email)
	}
	return nil, nil
}
func (r *arInviteRepo) LockByID(ctx context.Context, id uuid.UUID) (*domain.PendingInvitation, error) {
	if r.lockByIDFn != nil {
		return r.lockByIDFn(ctx, id)
	}
	return nil, nil
}
func (r *arInviteRepo) SetStatus(ctx context.Context, tenantID, id uuid.UUID, status domain.InvitationStatus, ver int64) (*domain.PendingInvitation, error) {
	if r.setStatusFn != nil {
		return r.setStatusFn(ctx, tenantID, id, status, ver)
	}
	return &domain.PendingInvitation{ID: id, Status: status, RecordVersion: ver + 1}, nil
}
func (r *arInviteRepo) CountPending(ctx context.Context, tenantID uuid.UUID) (int, error) {
	if r.countPendingFn != nil {
		return r.countPendingFn(ctx, tenantID)
	}
	return 0, nil
}
func (r *arInviteRepo) MostRecentCreatedAt(ctx context.Context, tenantID uuid.UUID, email string) (time.Time, error) {
	if r.mostRecentAtFn != nil {
		return r.mostRecentAtFn(ctx, tenantID, email)
	}
	return time.Time{}, nil
}
func (r *arInviteRepo) CountCreatedInWindow(ctx context.Context, tenantID uuid.UUID, since time.Time) (int, error) {
	if r.countWindowFn != nil {
		return r.countWindowFn(ctx, tenantID, since)
	}
	return 0, nil
}
func (r *arInviteRepo) List(context.Context, uuid.UUID) ([]domain.PendingInvitation, error) {
	return nil, nil
}
func (r *arInviteRepo) FindByID(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
	return nil, nil
}
func (r *arInviteRepo) Insert(context.Context, *domain.PendingInvitation) (*domain.PendingInvitation, error) {
	return nil, nil
}
func (r *arInviteRepo) SetKeycloakUserID(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int64) error {
	return nil
}
func (r *arInviteRepo) SetKCCleanupPending(ctx context.Context, tenantID, id uuid.UUID, pending bool, ver int64) error {
	if r.setKCCleanupPendingFn != nil {
		return r.setKCCleanupPendingFn(ctx, tenantID, id, pending, ver)
	}
	return nil
}
func (r *arInviteRepo) ListExpiring(context.Context, time.Time, int) ([]domain.PendingInvitation, error) {
	return nil, nil
}
func (r *arInviteRepo) ListPendingKCCleanup(context.Context, int) ([]domain.PendingInvitation, error) {
	return nil, nil
}

var _ port.InvitationRepository = (*arInviteRepo)(nil)

// arMembershipRepo is a MembershipRepository for AddFromRegister tests.
type arMembershipRepo struct {
	insertFn      func(ctx context.Context, m *domain.TenantMembership) (*domain.TenantMembership, error)
	countActiveFn func(ctx context.Context, tenantID uuid.UUID) (int, error)
}

func (r *arMembershipRepo) List(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
	return nil, nil
}
func (r *arMembershipRepo) FindByUserID(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *arMembershipRepo) Insert(ctx context.Context, m *domain.TenantMembership) (*domain.TenantMembership, error) {
	if r.insertFn != nil {
		return r.insertFn(ctx, m)
	}
	return &domain.TenantMembership{ID: uuid.New(), TenantID: m.TenantID, UserID: m.UserID, Status: m.Status}, nil
}
func (r *arMembershipRepo) SetStatus(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *arMembershipRepo) SoftDelete(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil }
func (r *arMembershipRepo) CountActive(ctx context.Context, tenantID uuid.UUID) (int, error) {
	if r.countActiveFn != nil {
		return r.countActiveFn(ctx, tenantID)
	}
	return 0, nil
}
func (r *arMembershipRepo) ListActiveUserIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

var _ port.MembershipRepository = (*arMembershipRepo)(nil)

// arTenantRepo is a TenantRepository stub for AddFromRegister tests.
type arTenantRepo struct {
	port.TenantRepositoryNoop
	lockByIDFn             func(ctx context.Context, id uuid.UUID) error
	licensedSeatsForUpdate func(ctx context.Context, id uuid.UUID) (int, error)
}

func (r *arTenantRepo) LockByID(ctx context.Context, id uuid.UUID) error {
	if r.lockByIDFn != nil {
		return r.lockByIDFn(ctx, id)
	}
	return nil
}
func (r *arTenantRepo) LicensedSeatsForUpdate(ctx context.Context, id uuid.UUID) (int, error) {
	if r.licensedSeatsForUpdate != nil {
		return r.licensedSeatsForUpdate(ctx, id)
	}
	return 100, nil
}

var _ port.TenantRepository = (*arTenantRepo)(nil)

// arRoleRepo is a TenantRoleRepository stub that records Grant calls.
type arRoleRepo struct {
	grantFn     func(ctx context.Context, tr *domain.TenantRole) (*domain.TenantRole, error)
	grantCalled bool
}

func (r *arRoleRepo) ListByUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *arRoleRepo) ListByRole(context.Context, uuid.UUID, domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *arRoleRepo) CountActiveOwners(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (r *arRoleRepo) Grant(ctx context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
	r.grantCalled = true
	if r.grantFn != nil {
		return r.grantFn(ctx, tr)
	}
	return &domain.TenantRole{ID: uuid.New(), RoleCode: tr.RoleCode}, nil
}
func (r *arRoleRepo) Revoke(context.Context, uuid.UUID, uuid.UUID, domain.TenantRoleCode) (*domain.TenantRole, error) {
	return nil, nil
}
func (r *arRoleRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}

var _ port.TenantRoleRepository = (*arRoleRepo)(nil)

// arCache is a spy Cache for AddFromRegister cache-eviction tests.
type arCache struct {
	deletedKeys []string
}

func (c *arCache) Get(_ context.Context, _ string) ([]byte, error) { return nil, nil }
func (c *arCache) MGet(_ context.Context, k []string) ([][]byte, error) {
	return make([][]byte, len(k)), nil
}
func (c *arCache) Set(_ context.Context, _ string, _ []byte, _ time.Duration) error { return nil }
func (c *arCache) SetNX(_ context.Context, _ string, _ []byte, _ time.Duration) (bool, error) {
	return true, nil
}
func (c *arCache) Delete(_ context.Context, keys ...string) error {
	c.deletedKeys = append(c.deletedKeys, keys...)
	return nil
}
func (c *arCache) Health(_ context.Context) error { return nil }
func (c *arCache) Close() error                   { return nil }

var _ port.Cache = (*arCache)(nil)

// buildARSvc wires an InvitationService with the minimum collaborators for
// AddFromRegister tests.
func buildARSvc(
	inv port.InvitationRepository,
	mem port.MembershipRepository,
	roles port.TenantRoleRepository,
	t port.TenantRepository,
	cache port.Cache,
) *service.InvitationService {
	return service.NewInvitationService(inv, mem, roles, nil, t, nil, cache, &passthroughTxRunner{}, nil, 7)
}

// ── test cases ────────────────────────────────────────────────────────────────

// TestAddFromRegister_TenantLockByIDError_Propagates verifies that when
// tenants.LockByID fails inside RunInTx, the error propagates from
// AddFromRegister (line 354).
func TestAddFromRegister_TenantLockByIDError_Propagates(t *testing.T) {
	lockErr := errors.New("tenant_lock_error")
	tenants := &arTenantRepo{
		lockByIDFn: func(_ context.Context, _ uuid.UUID) error {
			return lockErr
		},
	}
	inv := &arInviteRepo{} // FindPendingByKeycloakUser = nil (no pending)
	svc := buildARSvc(inv, &arMembershipRepo{}, nil, tenants, nil)

	_, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "user@example.com")
	require.Error(t, err)
	assert.ErrorIs(t, err, lockErr, "tenants.LockByID error must propagate from RunInTx")
}

// TestAddFromRegister_PlainAdd_NoPending_InsertsAndReturnsMembership verifies
// the plain-add path (lines 337-408): when neither keycloak user ID nor email
// matches a pending invitation, Insert is called and the new membership is returned.
func TestAddFromRegister_PlainAdd_NoPending_InsertsAndReturnsMembership(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()

	insertCalled := false
	mem := &arMembershipRepo{
		insertFn: func(_ context.Context, m *domain.TenantMembership) (*domain.TenantMembership, error) {
			insertCalled = true
			return &domain.TenantMembership{
				ID: uuid.New(), TenantID: m.TenantID, UserID: m.UserID, Status: m.Status,
			}, nil
		},
	}
	inv := &arInviteRepo{} // no pending invitations
	svc := buildARSvc(inv, mem, nil, &arTenantRepo{}, nil)

	got, err := svc.AddFromRegister(context.Background(), tenantID, userID, uuid.New(), "")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, tenantID, got.TenantID)
	assert.True(t, insertCalled, "plain-add must call memberships.Insert")
}

// TestAddFromRegister_MembershipInsertError_Propagates verifies that when
// memberships.Insert fails inside RunInTx, the error propagates (line 402-406).
func TestAddFromRegister_MembershipInsertError_Propagates(t *testing.T) {
	insertErr := errors.New("membership_insert_error")
	mem := &arMembershipRepo{
		insertFn: func(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
			return nil, insertErr
		},
	}
	inv := &arInviteRepo{}
	svc := buildARSvc(inv, mem, nil, &arTenantRepo{}, nil)

	_, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "")
	require.Error(t, err)
	assert.ErrorIs(t, err, insertErr)
}

// TestAddFromRegister_InviteLockByIDReturnsNil_ProceedsAsPlainAdd verifies
// the "row vanished" branch (line 369): when invites.LockByID returns (nil,nil)
// inside the tx, pending is set to nil and Insert is called as a plain add.
func TestAddFromRegister_InviteLockByIDReturnsNil_ProceedsAsPlainAdd(t *testing.T) {
	tenantID := uuid.New()
	pendingID := uuid.New()
	insertCalled := false

	inv := &arInviteRepo{
		findByKCUserFn: func(_ context.Context, _, _ uuid.UUID) (*domain.PendingInvitation, error) {
			// Return a pending invite so LockByID will be called
			return &domain.PendingInvitation{
				ID:       pendingID,
				TenantID: tenantID,
				Email:    "user@example.com",
				Status:   domain.InvitePending,
			}, nil
		},
		lockByIDFn: func(_ context.Context, _ uuid.UUID) (*domain.PendingInvitation, error) {
			return nil, nil // row vanished between outer lookup and tx lock
		},
	}
	mem := &arMembershipRepo{
		insertFn: func(_ context.Context, m *domain.TenantMembership) (*domain.TenantMembership, error) {
			insertCalled = true
			return &domain.TenantMembership{ID: uuid.New(), TenantID: m.TenantID, UserID: m.UserID}, nil
		},
	}
	svc := buildARSvc(inv, mem, nil, &arTenantRepo{}, nil)

	got, err := svc.AddFromRegister(context.Background(), tenantID, uuid.New(), uuid.New(), "user@example.com")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, insertCalled, "when locked row vanishes, plain add must still call Insert")
}

// TestAddFromRegister_InviteLockByIDStatusFlipped_ProceedsAsPlainAdd verifies
// the "status flipped under us" branch (line 371): when locked.Status is no
// longer InvitePending (e.g. the invite was revoked concurrently), the code
// falls through to plain add.
func TestAddFromRegister_InviteLockByIDStatusFlipped_ProceedsAsPlainAdd(t *testing.T) {
	pendingID := uuid.New()

	inv := &arInviteRepo{
		findByKCUserFn: func(_ context.Context, _, _ uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{
				ID:     pendingID,
				Status: domain.InvitePending,
			}, nil
		},
		lockByIDFn: func(_ context.Context, _ uuid.UUID) (*domain.PendingInvitation, error) {
			// Simulate concurrent revocation: status is now revoked
			return &domain.PendingInvitation{
				ID:     pendingID,
				Status: domain.InviteRevoked, // no longer pending
			}, nil
		},
	}
	mem := &arMembershipRepo{}
	svc := buildARSvc(inv, mem, nil, &arTenantRepo{}, nil)

	got, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "")
	require.NoError(t, err)
	require.NotNil(t, got, "status-flipped path must still complete as plain add")
}

// TestAddFromRegister_PendingFound_SetsStatusAndAppliesRoleGrants verifies the
// pending != nil happy path (lines 411-455): when a pending invitation is found
// and locked, SetStatus transitions it to accepted and role grants are applied.
func TestAddFromRegister_PendingFound_SetsStatusAndAppliesRoleGrants(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()
	inviterID := uuid.New()
	pendingID := uuid.New()

	setStatusCalled := false
	inv := &arInviteRepo{
		findByKCUserFn: func(_ context.Context, _, _ uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{
				ID:                 pendingID,
				TenantID:           tenantID,
				Email:              "user@example.com",
				Status:             domain.InvitePending,
				InvitedBy:          inviterID,
				InitialTenantRoles: []domain.TenantRoleCode{domain.RoleTenantAdmin},
			}, nil
		},
		lockByIDFn: func(_ context.Context, _ uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{
				ID:                 pendingID,
				TenantID:           tenantID,
				Email:              "user@example.com",
				Status:             domain.InvitePending,
				InvitedBy:          inviterID,
				InitialTenantRoles: []domain.TenantRoleCode{domain.RoleTenantAdmin},
				RecordVersion:      1,
			}, nil
		},
		setStatusFn: func(_ context.Context, _, _ uuid.UUID, status domain.InvitationStatus, _ int64) (*domain.PendingInvitation, error) {
			setStatusCalled = true
			assert.Equal(t, domain.InviteAccepted, status, "pending invitation must be accepted")
			return &domain.PendingInvitation{ID: pendingID, Status: domain.InviteAccepted, RecordVersion: 2}, nil
		},
	}
	roles := &arRoleRepo{}
	mem := &arMembershipRepo{}
	svc := buildARSvc(inv, mem, roles, &arTenantRepo{}, nil)

	got, err := svc.AddFromRegister(context.Background(), tenantID, userID, uuid.New(), "user@example.com")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, setStatusCalled, "SetStatus must be called to accept the pending invitation")
	assert.True(t, roles.grantCalled, "initial role grants from the invitation must be applied")
}

// TestAddFromRegister_CacheEviction_DeletesSeatUsageKey verifies that after a
// successful AddFromRegister (lines 462-464), the seat-usage cache key is evicted.
func TestAddFromRegister_CacheEviction_DeletesSeatUsageKey(t *testing.T) {
	tenantID := uuid.New()
	cache := &arCache{}

	inv := &arInviteRepo{}
	mem := &arMembershipRepo{}
	svc := buildARSvc(inv, mem, nil, &arTenantRepo{}, cache)

	_, err := svc.AddFromRegister(context.Background(), tenantID, uuid.New(), uuid.New(), "")
	require.NoError(t, err)

	expectedKey := "om:seat_usage:" + tenantID.String()
	assert.Contains(t, cache.deletedKeys, expectedKey,
		"AddFromRegister must evict the seat_usage cache key after successful insert")
}

// TestAddFromRegister_InviteLockByIDError_Propagates verifies that when
// invites.LockByID fails inside the tx, the error propagates (line 364).
func TestAddFromRegister_InviteLockByIDError_Propagates(t *testing.T) {
	pendingID := uuid.New()
	lockErr := errors.New("invitation_lock_error")

	inv := &arInviteRepo{
		findByKCUserFn: func(_ context.Context, _, _ uuid.UUID) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: pendingID, Status: domain.InvitePending}, nil
		},
		lockByIDFn: func(_ context.Context, _ uuid.UUID) (*domain.PendingInvitation, error) {
			return nil, lockErr
		},
	}
	svc := buildARSvc(inv, &arMembershipRepo{}, nil, &arTenantRepo{}, nil)

	_, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "")
	require.Error(t, err)
	assert.ErrorIs(t, err, lockErr, "invites.LockByID error must propagate from AddFromRegister")
}
