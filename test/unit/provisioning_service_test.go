// Unit tests for internal/core/service/provisioning_service.go
// SetMembershipStatus (I-4). SetRealmFields (I-2) uses the raw pool
// directly and is exercised in test/postgres; TrialSignup / DeleteMember
// are covered by the postgres integration suite.
package unit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildProvisioningSvc wires only the collaborators SetMembershipStatus
// uses (the membership repo); every other dependency stays nil.
func buildProvisioningSvc(m *fakeMembershipRepo) *service.ProvisioningService {
	return service.NewProvisioningService(
		nil, m, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
}

func buildProvisioningSvcWithCache(m *fakeMembershipRepo, cache port.Cache) *service.ProvisioningService {
	return service.NewProvisioningService(
		nil, m, nil, nil, nil, nil, nil, nil, nil, cache, nil,
	)
}

// ── SetMembershipStatus (I-4) ─────────────────────────────────────────

func TestProvisioning_SetMembershipStatus_DelegatesToRepoWithGivenArgs(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	m := &fakeMembershipRepo{
		setStatusFn: func(_ context.Context, tt, uu uuid.UUID, st domain.MembershipStatus, ver int64) (*domain.TenantMembership, error) {
			assert.Equal(t, tenantID, tt)
			assert.Equal(t, userID, uu)
			assert.Equal(t, domain.MembershipSuspended, st)
			assert.EqualValues(t, 4, ver)
			return &domain.TenantMembership{ID: uuid.New(), TenantID: tt, UserID: uu, Status: st, RecordVersion: ver + 1}, nil
		},
	}
	svc := buildProvisioningSvc(m)

	got, err := svc.SetMembershipStatus(context.Background(), tenantID, userID, domain.MembershipSuspended, 4)
	require.NoError(t, err)
	assert.Equal(t, domain.MembershipSuspended, got.Status)
	assert.EqualValues(t, 5, got.RecordVersion, "record_version returned as-bumped by repo")
}

func TestProvisioning_SetMembershipStatus_PropagatesRepoError(t *testing.T) {
	repoErr := errors.New("optimistic_lock_conflict")
	m := &fakeMembershipRepo{
		setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
			return nil, repoErr
		},
	}
	svc := buildProvisioningSvc(m)

	_, err := svc.SetMembershipStatus(context.Background(), uuid.New(), uuid.New(), domain.MembershipActive, 1)
	assert.ErrorIs(t, err, repoErr)
}

func TestProvisioning_SetMembershipStatus_Success_InvalidatesMemberCaches(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	m := &fakeMembershipRepo{
		setStatusFn: func(_ context.Context, tt, uu uuid.UUID, st domain.MembershipStatus, ver int64) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), TenantID: tt, UserID: uu, Status: st, RecordVersion: ver + 1}, nil
		},
	}
	cache := &spyCache{}
	svc := buildProvisioningSvcWithCache(m, cache)

	_, err := svc.SetMembershipStatus(context.Background(), tenantID, userID, domain.MembershipActive, 1)
	require.NoError(t, err)
	assert.Contains(t, cache.deleteCalls, "om:memberships:"+tenantID.String()+":"+userID.String())
	assert.Contains(t, cache.deleteCalls, "om:members:"+tenantID.String()+":50")
}

// DEC-2 / TM-12: I-4 suspending the last active tenant_owner still succeeds and sets ownerless_since.
func TestProvisioning_SetMembershipStatus_SuspendLastActiveOwner_MarksOwnerless(t *testing.T) {
	mem := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: domain.MembershipActive, RecordVersion: 1}, nil
		},
		setStatusFn: func(_ context.Context, tt, uu uuid.UUID, st domain.MembershipStatus, ver int64) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{TenantID: tt, UserID: uu, Status: st, RecordVersion: ver + 1}, nil
		},
	}
	roles := ownerRoleListRepo(t, nil, func(context.Context, uuid.UUID) (int, error) { return 0, nil })
	markCalled := false
	tenants := &invTenantRepo{markOwnerlessIfUnsetFn: func(context.Context, uuid.UUID) (bool, error) {
		markCalled = true
		return true, nil
	}}
	svc := buildDeleteMemberSvcFull(mem, roles, &ruDeptMemRepo{}, tenants, &arTxRunner{})

	got, err := svc.SetMembershipStatus(context.Background(), uuid.New(), uuid.New(), domain.MembershipSuspended, 1)
	require.NoError(t, err, "I-4 is never refused")
	assert.Equal(t, domain.MembershipSuspended, got.Status)
	assert.True(t, markCalled, "TM-12: 0 active owners after the suspend → MarkOwnerlessIfUnset")
}

// ── Constructor smoke — every ctor field set, no panic on nil deps ────

func TestProvisioning_NewProvisioningService_ReturnsNonNil(t *testing.T) {
	svc := service.NewProvisioningService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	assert.NotNil(t, svc, "constructor must not fail on nil collaborators — production wiring supplies them")
}

// ── DeleteMember without a WorkflowClient wired ──────────────────────────
// The I-5 delegate-impact gate (§8.8 WFI-1) is optional wiring
// (ProvisioningService.WithWorkflow); production always wires it — see
// provisioning_deletemember_gate_test.go for the gated behaviour. With no
// client wired, DeleteMember runs the plain cascade.
//
// This test wires up the minimum fakes needed for DeleteMember to succeed
// (plain non-owner member with no dept memberships or delegations) and
// verifies it returns nil.

func TestProvisioning_DeleteMember_WorkflowNotRequired(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()

	mem := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), UserID: userID, RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
	}
	roles := &ruRoleRepo{
		// Non-owner → wasOwner=false, TM-12 ownerless escalation skipped.
		listByUserFn:           func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		countActiveOwnersFn:    func(context.Context, uuid.UUID) (int, error) { return 3, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
	}
	deptMems := &ruDeptMemRepo{
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) { return nil, nil },
	}

	// No WithWorkflow: the I-5 gate is skipped.
	svc := service.NewProvisioningService(
		nil, mem, roles, deptMems, nil, nil, nil, nil,
		&passthroughTxRunner{}, nil, nil,
	)

	err := svc.DeleteMember(context.Background(), tenantID, userID)
	assert.NoError(t, err, "I-5 DeleteMember must succeed when no workflow client is wired")
}
