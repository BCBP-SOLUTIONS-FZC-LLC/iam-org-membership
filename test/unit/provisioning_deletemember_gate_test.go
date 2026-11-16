package unit_test

// I-5 delegate-impact gate (LLD I-5 row, §8.8 WFI-1/WFI-3/WFI-8): the
// Keycloak-deletion path refuses like P-8 does — 409 when workflows depend
// on the user, 503 when Workflow is unavailable — and never writes.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
)

// i5GateFixture returns a provisioning service whose cascade records whether
// it ran, plus a pointer to that flag.
func i5GateFixture(t *testing.T, memberExists bool, wf port.WorkflowClient) (*service.ProvisioningService, *bool) {
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
		&passthroughTxRunner{}, nil, nil).WithWorkflow(wf)
	return svc, &cascaded
}

func TestI5Gate_ActiveWorkflows_409NoWrite(t *testing.T) {
	wf := &fakeWorkflowClient{getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
		return &port.DelegateImpact{ActiveWorkflows: 2, WorkflowIDs: []uuid.UUID{uuid.New(), uuid.New()}}, nil
	}}
	svc, cascaded := i5GateFixture(t, true, wf)

	err := svc.DeleteMember(context.Background(), uuid.New(), uuid.New())
	require.ErrorIs(t, err, domain.ErrWorkflowResolutionRequired)
	assert.False(t, *cascaded, "WFI-3: no state change when workflows depend on the user")
}

func TestI5Gate_WorkflowUnavailable_503NoWrite(t *testing.T) {
	wf := &fakeWorkflowClient{getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
		return nil, errors.New("workflow 502")
	}}
	svc, cascaded := i5GateFixture(t, true, wf)

	err := svc.DeleteMember(context.Background(), uuid.New(), uuid.New())
	require.ErrorIs(t, err, domain.ErrWorkflowServiceUnavailable)
	assert.False(t, *cascaded, "WFI-8: no DB write when Workflow is unavailable")
}

func TestI5Gate_NoImpact_Cascades(t *testing.T) {
	wf := &fakeWorkflowClient{getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
		return &port.DelegateImpact{}, nil
	}}
	svc, cascaded := i5GateFixture(t, true, wf)

	require.NoError(t, svc.DeleteMember(context.Background(), uuid.New(), uuid.New()))
	assert.True(t, *cascaded)
}

func TestI5Gate_AlreadyDeleted_SkipsWorkflow_Idempotent(t *testing.T) {
	wf := &fakeWorkflowClient{getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
		t.Fatal("TM-12: a retried delete for a gone member must not call Workflow")
		return nil, nil
	}}
	svc, _ := i5GateFixture(t, false, wf)

	require.NoError(t, svc.DeleteMember(context.Background(), uuid.New(), uuid.New()),
		"Keycloak webhook retries stay no-ops even when Workflow would refuse")
}
