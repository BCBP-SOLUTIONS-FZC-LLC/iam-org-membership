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
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildProvisioningSvc wires only the collaborators SetMembershipStatus
// uses (the membership repo); every other dependency stays nil.
func buildProvisioningSvc(m *fakeMembershipRepo) *service.ProvisioningService {
	return service.NewProvisioningService(
		nil, nil, m, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
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

// ── Constructor smoke — every ctor field set, no panic on nil deps ────

func TestProvisioning_NewProvisioningService_ReturnsNonNil(t *testing.T) {
	svc := service.NewProvisioningService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	assert.NotNil(t, svc, "constructor must not fail on nil collaborators — production wiring supplies them")
}
