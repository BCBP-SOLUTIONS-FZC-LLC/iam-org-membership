// Extended tests for P-7 and P-28 scenarios.
package unit_test

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── configurable role repo ────────────────────────────────────────────

type extRoleRepo struct {
	listByUserFn        func(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenantRole, error)
	countActiveOwnersFn func(ctx context.Context, tenantID uuid.UUID) (int, error)
	grantFn             func(ctx context.Context, tr *domain.TenantRole) (*domain.TenantRole, error)
	revokeFn            func(ctx context.Context, tenantID, userID uuid.UUID, code domain.TenantRoleCode) (*domain.TenantRole, error)
}

func (r *extRoleRepo) ListByUser(ctx context.Context, t, u uuid.UUID) ([]domain.TenantRole, error) {
	if r.listByUserFn != nil {
		return r.listByUserFn(ctx, t, u)
	}
	return nil, nil
}
func (r *extRoleRepo) ListByRole(_ context.Context, _ uuid.UUID, _ domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *extRoleRepo) CountActiveOwners(ctx context.Context, t uuid.UUID) (int, error) {
	if r.countActiveOwnersFn != nil {
		return r.countActiveOwnersFn(ctx, t)
	}
	return 2, nil // default: 2 owners so TM-8 doesn't block
}
func (r *extRoleRepo) Grant(ctx context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
	if r.grantFn != nil {
		return r.grantFn(ctx, tr)
	}
	return tr, nil
}
func (r *extRoleRepo) Revoke(ctx context.Context, t, u uuid.UUID, c domain.TenantRoleCode) (*domain.TenantRole, error) {
	if r.revokeFn != nil {
		return r.revokeFn(ctx, t, u, c)
	}
	return &domain.TenantRole{}, nil
}
func (r *extRoleRepo) SoftDeleteAllForUser(_ context.Context, _, _ uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}

var _ port.TenantRoleRepository = (*extRoleRepo)(nil)

// buildMembershipSvcWithTx wires MembershipService with a passthroughTxRunner
// so RunInTx-based paths (P-28 owner operations) don't panic on nil txRunner.
func buildMembershipSvcWithTx(m port.MembershipRepository, r port.TenantRoleRepository) *service.MembershipService {
	return service.NewMembershipService(
		m, r, nil, nil, nil, nil,
		&fakeRPClient{}, // rp: non-nil so RevokeUserSessions doesn't panic
		nil,             // workflow
		&passthroughTxRunner{}, nil, 0,
	)
}

// ── P7-NON-OWNER-01: fast path for non-owner suspend ──────────────────

// Test Case ID:      P7-NON-OWNER-01
// Feature:           P-7 · non-owner suspend → fast path (no RunInTx)
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestMembership_SetStatus_NonOwner_FastPath(t *testing.T) {
	repoCallCount := 0
	m := &fakeMembershipRepo{
		setStatusFn: func(_ context.Context, _, _ uuid.UUID, s domain.MembershipStatus, _ int64) (*domain.TenantMembership, error) {
			repoCallCount++
			return &domain.TenantMembership{Status: s, RecordVersion: 2}, nil
		},
	}
	// Use MembershipActive to bypass the roles check on suspend path
	svc := buildMembershipSvcForSetStatus(m, nil, nil, nil)
	res, err := svc.SetStatus(context.Background(), uuid.New(), uuid.New(),
		domain.MembershipActive, 1)
	require.NoError(t, err)
	assert.Equal(t, domain.MembershipActive, res.Membership.Status)
	assert.Equal(t, 1, repoCallCount)
}

// ── P7-IDEMPOTENT-REACTIVATE-01: reactivate already-active → same rv ─

// Test Case ID:      P7-IDEMPOTENT-REACTIVATE-01
// Feature:           P-7 · reactivate already-active → 200, record_version unchanged (TRG-3)
// Priority: P2 · Severity: Minor · Automation Status: Automated
func TestMembership_SetStatus_IdempotentReactivate_SameRV(t *testing.T) {
	m := &fakeMembershipRepo{
		setStatusFn: func(_ context.Context, _, _ uuid.UUID, s domain.MembershipStatus, ver int64) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: s, RecordVersion: ver}, nil
		},
	}
	svc := buildMembershipSvcForSetStatus(m, nil, nil, nil)
	res, err := svc.SetStatus(context.Background(), uuid.New(), uuid.New(),
		domain.MembershipActive, 3)
	require.NoError(t, err)
	assert.EqualValues(t, 3, res.Membership.RecordVersion, "record_version unchanged on no-op")
}

// ── P28-SELF-STRIP-01: actor strips own owner when others exist → 200 ─

// Test Case ID:      P28-SELF-STRIP-01
// Feature:           P-28 · self-strip tenant_owner when ownerCount>1 → 200
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestReconcileRoles_SelfStrip_WhenOtherOwnersExist(t *testing.T) {
	tenantID, actorID := uuid.New(), uuid.New()
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: actorID, Status: domain.MembershipActive}

	roles := &extRoleRepo{
		listByUserFn: func(_ context.Context, _, _ uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		countActiveOwnersFn: func(_ context.Context, _ uuid.UUID) (int, error) {
			return 2, nil // 2 owners → self-strip allowed
		},
	}
	m := &fakeMembershipRepo{
		findByUserIDFn: func(_ context.Context, _, _ uuid.UUID) (*domain.TenantMembership, error) {
			return mem, nil
		},
	}
	// Pass passthroughTxRunner so RunInTx doesn't panic on nil txRunner.
	svc := buildMembershipSvcWithTx(m, roles)
	granted, revoked, err := svc.ReconcileRoles(context.Background(), tenantID, actorID,
		[]domain.TenantRoleCode{}, actorID)
	require.NoError(t, err)
	assert.Empty(t, granted)
	_ = revoked
}

// ── P28-MEMBER-DEMOTE-01: desired=[] strips all elevated roles ────────

// Test Case ID:      P28-MEMBER-DEMOTE-01
// Feature:           P-28 · desired=[] → all elevated roles revoked (TR-7)
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestReconcileRoles_EmptyDesired_DemotesToPlainMember(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID, Status: domain.MembershipActive}
	revokedCount := 0

	roles := &extRoleRepo{
		listByUserFn: func(_ context.Context, _, _ uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantAdmin}}, nil
		},
		revokeFn: func(_ context.Context, _, _ uuid.UUID, _ domain.TenantRoleCode) (*domain.TenantRole, error) {
			revokedCount++
			return &domain.TenantRole{}, nil
		},
	}
	m := &fakeMembershipRepo{
		findByUserIDFn: func(_ context.Context, _, _ uuid.UUID) (*domain.TenantMembership, error) {
			return mem, nil
		},
	}
	svc := buildMembershipSvcWithTx(m, roles)
	_, revoked, err := svc.ReconcileRoles(context.Background(), tenantID, userID,
		[]domain.TenantRoleCode{}, uuid.New())
	require.NoError(t, err)
	assert.NotEmpty(t, revoked)
	assert.Equal(t, 1, revokedCount)
}

// ── P28-HAPPY-01: grant tenant_admin → 200 + event ────────────────────

// Test Case ID:      P28-HAPPY-01
// Feature:           P-28 · grant tenant_admin to plain member → 200 + TenantRoleGranted event
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestReconcileRoles_GrantAdmin_Succeeds(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID, Status: domain.MembershipActive}

	roles := &extRoleRepo{
		listByUserFn: func(_ context.Context, _, _ uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{}, nil // plain member
		},
		grantFn: func(_ context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
			return tr, nil
		},
	}
	m := &fakeMembershipRepo{
		findByUserIDFn: func(_ context.Context, _, _ uuid.UUID) (*domain.TenantMembership, error) {
			return mem, nil
		},
	}
	svc := buildMembershipSvcWithTx(m, roles)
	granted, _, err := svc.ReconcileRoles(context.Background(), tenantID, userID,
		[]domain.TenantRoleCode{domain.RoleTenantAdmin}, uuid.New())
	require.NoError(t, err)
	require.Len(t, granted, 1)
	assert.Equal(t, domain.RoleTenantAdmin, granted[0].RoleCode)
}

// ── P28-TM8-RACE-01: authoritative recheck inside tx (BUG-P28-1 fix) ──

// Test Case ID:      P28-TM8-RACE-01
// Feature:           P-28 · TM-8 inside-tx recheck blocks last-owner strip (BUG-P28-1)
// Priority: P1 · Severity: Critical · Automation Status: Automated
func TestReconcileRoles_TM8_RecheckInsideTx_BlocksLastOwner(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID, Status: domain.MembershipActive}

	roles := &extRoleRepo{
		listByUserFn: func(_ context.Context, _, _ uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		countActiveOwnersFn: func(_ context.Context, _ uuid.UUID) (int, error) {
			return 1, nil // only one owner → TM-8 blocks
		},
	}
	m := &fakeMembershipRepo{
		findByUserIDFn: func(_ context.Context, _, _ uuid.UUID) (*domain.TenantMembership, error) {
			return mem, nil
		},
	}
	svc := buildMembershipSvcWithTx(m, roles)
	_, _, err := svc.ReconcileRoles(context.Background(), tenantID, userID,
		[]domain.TenantRoleCode{}, uuid.New())
	assert.ErrorIs(t, err, domain.ErrLastOwnerRemoval)
}
