// Unit tests for RemoveUser (P-8 pre-check §8.8), RemovalResolution
// (P-26 §8.8.3), and ValidateAndEmitAssigneeOverride (I-13 §5.4).
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

// spyWorkflowClient records which method was called + returns configurable
// results. Extends the existing fakeWorkflowClient with Reassign/Cancel.
type spyWorkflowClient struct {
	reassignFn func(ctx context.Context, tenantID, oldUserID, newUserID uuid.UUID, delegationID *uuid.UUID) error
	cancelFn   func(ctx context.Context, tenantID, userID uuid.UUID, delegationID *uuid.UUID) error

	reassignCalled bool
	cancelCalled   bool
}

func (s *spyWorkflowClient) GetDelegateImpact(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
	return &port.DelegateImpact{}, nil
}
func (s *spyWorkflowClient) ReassignDelegate(ctx context.Context, tenantID, oldUserID, newUserID uuid.UUID, delegationID *uuid.UUID) error {
	s.reassignCalled = true
	if s.reassignFn != nil {
		return s.reassignFn(ctx, tenantID, oldUserID, newUserID, delegationID)
	}
	return nil
}
func (s *spyWorkflowClient) CancelByDelegate(ctx context.Context, tenantID, userID uuid.UUID, delegationID *uuid.UUID) error {
	s.cancelCalled = true
	if s.cancelFn != nil {
		return s.cancelFn(ctx, tenantID, userID, delegationID)
	}
	return nil
}

var _ port.WorkflowClient = (*spyWorkflowClient)(nil)

func buildMembershipSvcForRemoval(m port.MembershipRepository, r port.TenantRoleRepository, dm port.DeptMembershipRepository, wf port.WorkflowClient, tr port.TxRunner) *service.MembershipService {
	return service.NewMembershipService(m, r, dm, &port.TenantRepositoryNoop{}, nil, nil, nil, wf, tr, nil, 30)
}

// ── RemoveUser — WFI-3 pre-check branches ──────────────────────────────

func TestMembership_RemoveUser_WorkflowClientUnavailableIs503(t *testing.T) {
	// LLD §8.8 WFI-3/WFI-8: RemoveUser blocks on workflow — a 5xx from the
	// workflow service must surface as ErrWorkflowServiceUnavailable, NOT
	// fall through to the tx.
	svc := buildMembershipSvcForRemoval(nil, nil, nil,
		&fakeWorkflowClient{
			getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
				return nil, errors.New("connection reset")
			},
		}, nil)

	err := svc.RemoveUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrWorkflowServiceUnavailable)
}

func TestMembership_RemoveUser_ActiveWorkflowsBlockWithResolutionRequired(t *testing.T) {
	workflowID := uuid.New()
	svc := buildMembershipSvcForRemoval(nil, nil, nil,
		&fakeWorkflowClient{
			getDelegateImpactFn: func(_ context.Context, _, _ uuid.UUID, delID *uuid.UUID) (*port.DelegateImpact, error) {
				assert.Nil(t, delID, "P-8 uses tenant-wide impact (no dept scope)")
				return &port.DelegateImpact{ActiveWorkflows: 2, WorkflowIDs: []uuid.UUID{workflowID}}, nil
			},
		}, nil)

	err := svc.RemoveUser(context.Background(), uuid.New(), uuid.New(), uuid.New())

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "workflow_resolution_required", de.Code)
	assert.Equal(t, 2, de.Details["active_workflows"])
	// The response body advertises the P-26 resolution actions so the
	// client knows what to POST.
	actions, ok := de.Details["allowed_actions"].([]string)
	require.True(t, ok)
	assert.Contains(t, actions, string(service.RemovalReplaceDelegate))
	assert.Contains(t, actions, string(service.RemovalStopWorkflows))
}

// ── RemovalResolution — workflow=nil error path ─────────────────────────

func TestMembership_RemovalResolution_NilWorkflowIsUnavailable(t *testing.T) {
	svc := buildMembershipSvcForRemoval(nil, nil, nil, nil, nil)
	err := svc.RemovalResolution(context.Background(), uuid.New(), uuid.New(),
		service.RemovalReplaceDelegate, nil, uuid.New())
	assert.ErrorIs(t, err, domain.ErrWorkflowServiceUnavailable)
}

// ── RemovalResolution — replace_delegate branch ─────────────────────────

func TestMembership_RemovalResolution_ReplaceDelegate_RequiresReplacementID(t *testing.T) {
	wf := &spyWorkflowClient{}
	svc := buildMembershipSvcForRemoval(nil, nil, nil, wf, nil)
	err := svc.RemovalResolution(context.Background(), uuid.New(), uuid.New(),
		service.RemovalReplaceDelegate, nil, uuid.New())

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
	assert.False(t, wf.reassignCalled, "must reject before calling workflow")
}

func TestMembership_RemovalResolution_ReplaceDelegate_ReplacementNotFound(t *testing.T) {
	m := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return nil, errors.New("no such member")
		},
	}
	wf := &spyWorkflowClient{}
	svc := buildMembershipSvcForRemoval(m, nil, nil, wf, nil)
	replacement := uuid.New()
	err := svc.RemovalResolution(context.Background(), uuid.New(), uuid.New(),
		service.RemovalReplaceDelegate, &replacement, uuid.New())

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "invalid_replacement", de.Code)
	assert.Equal(t, "invalid_replacement", de.Details["code"])
	assert.False(t, wf.reassignCalled)
}

func TestMembership_RemovalResolution_ReplaceDelegate_ReplacementNotActive(t *testing.T) {
	m := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: domain.MembershipSuspended}, nil
		},
	}
	wf := &spyWorkflowClient{}
	svc := buildMembershipSvcForRemoval(m, nil, nil, wf, nil)
	replacement := uuid.New()
	err := svc.RemovalResolution(context.Background(), uuid.New(), uuid.New(),
		service.RemovalReplaceDelegate, &replacement, uuid.New())
	assert.ErrorIs(t, err, domain.ErrInvalidReplacement)
	assert.False(t, wf.reassignCalled)
}

func TestMembership_RemovalResolution_ReplaceDelegate_HappyPath(t *testing.T) {
	m := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: domain.MembershipActive}, nil
		},
	}
	wf := &spyWorkflowClient{}
	svc := buildMembershipSvcForRemoval(m, nil, nil, wf, nil)
	replacement := uuid.New()
	err := svc.RemovalResolution(context.Background(), uuid.New(), uuid.New(),
		service.RemovalReplaceDelegate, &replacement, uuid.New())
	require.NoError(t, err)
	assert.True(t, wf.reassignCalled, "workflow.ReassignDelegate must fire on success")
}

func TestMembership_RemovalResolution_ReplaceDelegate_WorkflowErrorPropagates(t *testing.T) {
	m := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: domain.MembershipActive}, nil
		},
	}
	wf := &spyWorkflowClient{
		reassignFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, *uuid.UUID) error {
			return errors.New("workflow 502")
		},
	}
	svc := buildMembershipSvcForRemoval(m, nil, nil, wf, nil)
	replacement := uuid.New()
	err := svc.RemovalResolution(context.Background(), uuid.New(), uuid.New(),
		service.RemovalReplaceDelegate, &replacement, uuid.New())
	assert.ErrorIs(t, err, domain.ErrWorkflowServiceUnavailable)
}

// ── RemovalResolution — stop_workflows branch ───────────────────────────

func TestMembership_RemovalResolution_StopWorkflows_HappyPath(t *testing.T) {
	wf := &spyWorkflowClient{}
	svc := buildMembershipSvcForRemoval(nil, nil, nil, wf, nil)
	err := svc.RemovalResolution(context.Background(), uuid.New(), uuid.New(),
		service.RemovalStopWorkflows, nil, uuid.New())
	require.NoError(t, err)
	assert.True(t, wf.cancelCalled)
}

func TestMembership_RemovalResolution_StopWorkflows_WorkflowErrorPropagates(t *testing.T) {
	wf := &spyWorkflowClient{
		cancelFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) error {
			return errors.New("workflow 503")
		},
	}
	svc := buildMembershipSvcForRemoval(nil, nil, nil, wf, nil)
	err := svc.RemovalResolution(context.Background(), uuid.New(), uuid.New(),
		service.RemovalStopWorkflows, nil, uuid.New())
	assert.ErrorIs(t, err, domain.ErrWorkflowServiceUnavailable)
}

// ── RemovalResolution — invalid action ──────────────────────────────────

func TestMembership_RemovalResolution_UnknownActionIsValidationError(t *testing.T) {
	wf := &spyWorkflowClient{}
	svc := buildMembershipSvcForRemoval(nil, nil, nil, wf, nil)
	err := svc.RemovalResolution(context.Background(), uuid.New(), uuid.New(),
		service.RemovalAction("bogus"), nil, uuid.New())

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	// Bug B-9 fixed: invalid_action now uses ErrInvalidAction → 422 (LLD §17)
	assert.Equal(t, "invalid_action", de.Code)
	assert.Equal(t, domain.ErrInvalidAction, de.Cause)
}

// ── ValidateAndEmitAssigneeOverride (I-13) ──────────────────────────────

func TestMembership_ValidateAndEmitAssigneeOverride_ActorRoleReadError(t *testing.T) {
	r := &fakeRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, errors.New("roles unavailable")
		},
	}
	svc := buildMembershipSvcForRemoval(nil, r, nil, nil, nil)
	err := svc.ValidateAndEmitAssigneeOverride(context.Background(),
		uuid.New(), uuid.New(), uuid.New(), uuid.New(), domain.DeptApprover, uuid.New())
	assert.ErrorIs(t, err, domain.ErrInsufficientRole)
}

func TestMembership_ValidateAndEmitAssigneeOverride_ActorWithoutElevatedRoleRejected(t *testing.T) {
	// LLD I-13 step 1: actor must hold tender_admin, tenant_admin, or owner.
	r := &fakeRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			// Actor has only 'member' (derived) — no elevated grant.
			return nil, nil
		},
	}
	svc := buildMembershipSvcForRemoval(nil, r, nil, nil, nil)
	err := svc.ValidateAndEmitAssigneeOverride(context.Background(),
		uuid.New(), uuid.New(), uuid.New(), uuid.New(), domain.DeptApprover, uuid.New())
	assert.ErrorIs(t, err, domain.ErrInsufficientRole)
}

func TestMembership_ValidateAndEmitAssigneeOverride_AssigneeNotAMember(t *testing.T) {
	// Actor is tender_admin so passes AUTH-3. Assignee lookup fails.
	r := &fakeRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenderAdmin}}, nil
		},
	}
	m := &fakeMemRepoFull{
		findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return nil, errors.New("no such member")
		},
	}
	svc := buildMembershipSvcForRemoval(m, r, nil, nil, nil)
	err := svc.ValidateAndEmitAssigneeOverride(context.Background(),
		uuid.New(), uuid.New(), uuid.New(), uuid.New(), domain.DeptReviewer, uuid.New())
	assert.ErrorIs(t, err, domain.ErrAssigneeIneligible)
}

func TestMembership_ValidateAndEmitAssigneeOverride_AssigneeSuspendedIsIneligible(t *testing.T) {
	r := &fakeRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
	}
	m := &fakeMemRepoFull{
		findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: domain.MembershipSuspended}, nil
		},
	}
	dm := &fakeDeptMemListByUser{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			return nil, nil
		},
	}
	svc := buildMembershipSvcForRemoval(m, r, dm, nil, nil)
	err := svc.ValidateAndEmitAssigneeOverride(context.Background(),
		uuid.New(), uuid.New(), uuid.New(), uuid.New(), domain.DeptReviewer, uuid.New())
	assert.ErrorIs(t, err, domain.ErrAssigneeIneligible)
}

func TestMembership_ValidateAndEmitAssigneeOverride_AssigneeWrongDepartmentIneligible(t *testing.T) {
	deptID := uuid.New()
	r := &fakeRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantAdmin}}, nil
		},
	}
	m := &fakeMemRepoFull{
		findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: domain.MembershipActive}, nil
		},
	}
	dm := &fakeDeptMemListByUser{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			// Assignee is preparator in a DIFFERENT department.
			return []domain.DeptMembership{{DepartmentID: uuid.New(), RoleLevel: domain.DeptPreparator}}, nil
		},
	}
	svc := buildMembershipSvcForRemoval(m, r, dm, nil, nil)
	err := svc.ValidateAndEmitAssigneeOverride(context.Background(),
		uuid.New(), uuid.New(), uuid.New(), deptID, domain.DeptReviewer, uuid.New())
	assert.ErrorIs(t, err, domain.ErrAssigneeIneligible)
}

func TestMembership_ValidateAndEmitAssigneeOverride_AssigneeBelowRequiredLevelIneligible(t *testing.T) {
	deptID := uuid.New()
	r := &fakeRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenderAdmin}}, nil
		},
	}
	m := &fakeMemRepoFull{
		findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: domain.MembershipActive}, nil
		},
	}
	dm := &fakeDeptMemListByUser{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			// Preparator < Approver → fails Satisfies.
			return []domain.DeptMembership{{DepartmentID: deptID, RoleLevel: domain.DeptPreparator}}, nil
		},
	}
	svc := buildMembershipSvcForRemoval(m, r, dm, nil, nil)
	err := svc.ValidateAndEmitAssigneeOverride(context.Background(),
		uuid.New(), uuid.New(), uuid.New(), deptID, domain.DeptApprover, uuid.New())
	assert.ErrorIs(t, err, domain.ErrAssigneeIneligible)
}

func TestMembership_ValidateAndEmitAssigneeOverride_HappyPath(t *testing.T) {
	deptID := uuid.New()
	r := &fakeRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenderAdmin}}, nil
		},
	}
	m := &fakeMemRepoFull{
		findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: domain.MembershipActive}, nil
		},
	}
	dm := &fakeDeptMemListByUser{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			return []domain.DeptMembership{{DepartmentID: deptID, RoleLevel: domain.DeptApprover}}, nil
		},
	}
	svc := buildMembershipSvcForRemoval(m, r, dm, nil, &passthroughTxRunner{})

	err := svc.ValidateAndEmitAssigneeOverride(context.Background(),
		uuid.New(), uuid.New(), uuid.New(), deptID, domain.DeptReviewer, uuid.New())
	require.NoError(t, err, "actor=tender_admin, assignee=Approver, required=Reviewer → eligible")
}
