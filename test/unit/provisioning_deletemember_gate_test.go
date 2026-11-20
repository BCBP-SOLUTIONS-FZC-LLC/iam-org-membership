package unit_test

// I-5 (Keycloak-deletion path): no Workflow delegate-impact gate (DEC-1);
// the cascade runs for an existing member, and a retried delete for a gone
// member stays an idempotent no-op (TM-12).

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
)

// i5GateFixture returns a provisioning service whose cascade records whether
// it ran, plus a pointer to that flag.
func i5GateFixture(t *testing.T, memberExists bool) (*service.ProvisioningService, *bool) {
	t.Helper()
	cascaded := false
	mem := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			if !memberExists {
				return nil, domain.ErrMemberNotFound
			}
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { cascaded = true; return nil },
	}
	roles := &ruRoleRepo{
		listByUserFn:        func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) { return 1, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			cascaded = true
			return nil, nil
		},
	}
	deptMems := &ruDeptMemRepo{
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) { return nil, nil },
	}
	svc := service.NewProvisioningService(nil, mem, roles, deptMems, nil, nil, nil, nil,
		&passthroughTxRunner{}, nil, nil)
	return svc, &cascaded
}

func TestI5Gate_NoImpact_Cascades(t *testing.T) {
	svc, cascaded := i5GateFixture(t, true)

	require.NoError(t, svc.DeleteMember(context.Background(), uuid.New(), uuid.New()))
	assert.True(t, *cascaded)
}

func TestI5Gate_AlreadyDeleted_SkipsWorkflow_Idempotent(t *testing.T) {
	svc, _ := i5GateFixture(t, false)

	require.NoError(t, svc.DeleteMember(context.Background(), uuid.New(), uuid.New()),
		"Keycloak webhook retries stay no-ops")
}
