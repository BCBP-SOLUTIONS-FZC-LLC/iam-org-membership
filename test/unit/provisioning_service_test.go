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

// ── Constructor smoke — every ctor field set, no panic on nil deps ────

func TestProvisioning_NewProvisioningService_ReturnsNonNil(t *testing.T) {
	svc := service.NewProvisioningService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	assert.NotNil(t, svc, "constructor must not fail on nil collaborators — production wiring supplies them")
}

// ── I5-DEP-01: DeleteMember has no WorkflowClient dependency ─────────────
// LLD WFI-1 documents that the WFI-3 pre-check is present only in
// MembershipService.RemoveUser (P-8). ProvisioningService.DeleteMember (I-5)
// performs a direct cascade without any workflow availability check, so the
// Workflow service being unavailable is irrelevant on this path.
//
// This test wires up the minimum fakes needed for DeleteMember to succeed
// (plain non-owner member with no dept memberships or delegations) and
// verifies it returns nil — no workflow client is needed.

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

	// NewProvisioningService has no WorkflowClient parameter — the WFI-3
	// pre-check is architecturally absent from the I-5 path.
	svc := service.NewProvisioningService(
		nil, mem, roles, deptMems, nil, nil, nil, nil,
		&passthroughTxRunner{}, nil, nil,
	)

	err := svc.DeleteMember(context.Background(), tenantID, userID)
	assert.NoError(t, err, "I-5 DeleteMember must succeed without any workflow service dependency")
}
