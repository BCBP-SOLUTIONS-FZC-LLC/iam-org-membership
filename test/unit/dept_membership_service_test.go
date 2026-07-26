// Unit tests for internal/core/service/dept_membership_service.go
// ListByDepartment (P-9) and Remove (P-11).
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

// ── DeptMembershipRepository stub ──────────────────────────────────────

type fakeDeptMemRepo struct {
	listByDeptFn func(ctx context.Context, tenantID, departmentID uuid.UUID) ([]domain.DeptMembership, error)
	removeFn     func(ctx context.Context, tenantID, userID, departmentID uuid.UUID) (*domain.DeptMembership, error)
}

func (f *fakeDeptMemRepo) ListByUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (f *fakeDeptMemRepo) ListByDepartment(ctx context.Context, tenantID, departmentID uuid.UUID) ([]domain.DeptMembership, error) {
	return f.listByDeptFn(ctx, tenantID, departmentID)
}
func (f *fakeDeptMemRepo) Assign(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, domain.DeptRole, uuid.UUID) (*domain.DeptMembership, error) {
	return nil, errors.New("not used")
}
func (f *fakeDeptMemRepo) Remove(ctx context.Context, tenantID, userID, departmentID uuid.UUID) (*domain.DeptMembership, error) {
	return f.removeFn(ctx, tenantID, userID, departmentID)
}
func (f *fakeDeptMemRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}

var _ port.DeptMembershipRepository = (*fakeDeptMemRepo)(nil)

// ── DelegationRepository stub ──────────────────────────────────────────

type fakeDelegationRepo struct {
	findActiveDeptDelegateFn func(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.Delegation, error)
}

func (f *fakeDelegationRepo) List(context.Context, uuid.UUID) ([]domain.Delegation, error) {
	return nil, nil
}
func (f *fakeDelegationRepo) ListByDelegator(context.Context, uuid.UUID, uuid.UUID) ([]domain.Delegation, error) {
	return nil, nil
}
func (f *fakeDelegationRepo) FindByID(context.Context, uuid.UUID, uuid.UUID) (*domain.Delegation, error) {
	return nil, nil
}
func (f *fakeDelegationRepo) Insert(context.Context, *domain.Delegation) (*domain.Delegation, error) {
	return nil, nil
}
func (f *fakeDelegationRepo) End(context.Context, uuid.UUID, uuid.UUID, domain.DelegationStatus, int64) (*domain.Delegation, error) {
	return nil, nil
}
func (f *fakeDelegationRepo) ListExpiringBefore(context.Context, time.Time, int) ([]domain.Delegation, error) {
	return nil, nil
}
func (f *fakeDelegationRepo) SoftDeleteForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.Delegation, error) {
	return nil, nil
}
func (f *fakeDelegationRepo) FindActiveDeptDelegateForUser(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.Delegation, error) {
	return f.findActiveDeptDelegateFn(ctx, tenantID, userID, deptID)
}

var _ port.DelegationRepository = (*fakeDelegationRepo)(nil)

// ── WorkflowClient stub ────────────────────────────────────────────────

type fakeWorkflowClient struct {
	getDelegateImpactFn func(ctx context.Context, tenantID, userID uuid.UUID, delegationID *uuid.UUID) (*port.DelegateImpact, error)
}

func (f *fakeWorkflowClient) GetDelegateImpact(ctx context.Context, tenantID, userID uuid.UUID, delegationID *uuid.UUID) (*port.DelegateImpact, error) {
	return f.getDelegateImpactFn(ctx, tenantID, userID, delegationID)
}
func (f *fakeWorkflowClient) ReassignDelegate(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, *uuid.UUID) error {
	return nil
}
func (f *fakeWorkflowClient) CancelByDelegate(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) error {
	return nil
}

var _ port.WorkflowClient = (*fakeWorkflowClient)(nil)

// ── TxRunner stub ──────────────────────────────────────────────────────

// passthroughTxRunner just invokes fn with the same ctx — good enough for
// unit tests that don't need real tx semantics.
type passthroughTxRunner struct {
	runErr error
}

func (r *passthroughTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if r.runErr != nil {
		return r.runErr
	}
	return fn(ctx)
}

var _ port.TxRunner = (*passthroughTxRunner)(nil)

// ── ListByDepartment (P-9) ─────────────────────────────────────────────

func TestDeptMembership_ListByDepartment_DelegatesToRepo(t *testing.T) {
	tenantID, deptID := uuid.New(), uuid.New()
	want := []domain.DeptMembership{{ID: uuid.New(), TenantID: tenantID, DepartmentID: deptID, RoleLevel: domain.DeptReviewer}}
	repo := &fakeDeptMemRepo{
		listByDeptFn: func(_ context.Context, tt, dd uuid.UUID) ([]domain.DeptMembership, error) {
			assert.Equal(t, tenantID, tt)
			assert.Equal(t, deptID, dd)
			return want, nil
		},
	}
	svc := service.NewDeptMembershipService(repo, nil, nil, nil, nil, nil, nil)

	got, err := svc.ListByDepartment(context.Background(), tenantID, deptID)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestDeptMembership_ListByDepartment_PropagatesRepoError(t *testing.T) {
	repoErr := errors.New("rls block")
	repo := &fakeDeptMemRepo{
		listByDeptFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			return nil, repoErr
		},
	}
	svc := service.NewDeptMembershipService(repo, nil, nil, nil, nil, nil, nil)

	_, err := svc.ListByDepartment(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, repoErr)
}

// ── Remove (P-11) — WFI-11 department-scoped delegate impact ───────────

func TestDeptMembership_Remove_DelegationLookupErrorSurfaces(t *testing.T) {
	repoErr := errors.New("delegation read failed")
	delRepo := &fakeDelegationRepo{
		findActiveDeptDelegateFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.Delegation, error) {
			return nil, repoErr
		},
	}
	svc := service.NewDeptMembershipService(&fakeDeptMemRepo{}, nil, nil, delRepo, &fakeWorkflowClient{}, nil, nil)

	_, err := svc.Remove(context.Background(), uuid.New(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, repoErr)
}

func TestDeptMembership_Remove_ReturnsWorkflowResolutionRequiredWhenImpactPositive(t *testing.T) {
	// WFI-11: department-scoped GetDelegateImpact returns active_workflows > 0
	// → 409 workflow_resolution_required (must be resolved via P-26 first).
	workflowID := uuid.New()
	delRepo := &fakeDelegationRepo{
		findActiveDeptDelegateFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.Delegation, error) {
			return nil, nil // no active delegation → nil delegation_id passed through
		},
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(_ context.Context, _, _ uuid.UUID, delegationID *uuid.UUID) (*port.DelegateImpact, error) {
			assert.Nil(t, delegationID, "no active delegation → nil delegation_id (tenant-wide query fallback)")
			return &port.DelegateImpact{ActiveWorkflows: 3, WorkflowIDs: []uuid.UUID{workflowID}}, nil
		},
	}
	svc := service.NewDeptMembershipService(&fakeDeptMemRepo{}, nil, nil, delRepo, wf, nil, nil)

	_, err := svc.Remove(context.Background(), uuid.New(), uuid.New(), uuid.New(), uuid.New())

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "workflow_resolution_required", de.Code)
	assert.Equal(t, 3, de.Details["active_workflows"])
	assert.Contains(t, de.Details["allowed_actions"], "replace_delegate")
	assert.Contains(t, de.Details["allowed_actions"], "stop_workflows")
}

func TestDeptMembership_Remove_ScopesDelegationIDWhenActiveDelegationExists(t *testing.T) {
	// If the user is the active delegate on a dept-scoped delegation, we
	// must pass that delegation.id to the workflow query so the impact
	// count is dept-scoped, not tenant-wide (WFI-11).
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	delegationID := uuid.New()
	delRepo := &fakeDelegationRepo{
		findActiveDeptDelegateFn: func(_ context.Context, tt, uu, dd uuid.UUID) (*domain.Delegation, error) {
			assert.Equal(t, tenantID, tt)
			assert.Equal(t, userID, uu)
			assert.Equal(t, deptID, dd)
			return &domain.Delegation{ID: delegationID}, nil
		},
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(_ context.Context, _, _ uuid.UUID, delID *uuid.UUID) (*port.DelegateImpact, error) {
			require.NotNil(t, delID, "delegation_id must be threaded through")
			assert.Equal(t, delegationID, *delID)
			return &port.DelegateImpact{}, nil
		},
	}
	repo := &fakeDeptMemRepo{
		removeFn: func(_ context.Context, _, _, _ uuid.UUID) (*domain.DeptMembership, error) {
			return &domain.DeptMembership{ID: uuid.New()}, nil
		},
	}
	svc := service.NewDeptMembershipService(repo, nil, nil, delRepo, wf, nil, &passthroughTxRunner{})

	_, err := svc.Remove(context.Background(), tenantID, userID, deptID, uuid.New())
	require.NoError(t, err)
}

func TestDeptMembership_Remove_WorkflowFailOpenProceedsWithRemoval(t *testing.T) {
	// If GetDelegateImpact returns an error, the code path `err == nil && ...`
	// falls through → removal proceeds (fail-open, WFI-13-spirit).
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return nil, errors.New("workflow svc unavailable")
		},
	}
	removeCalled := false
	repo := &fakeDeptMemRepo{
		removeFn: func(_ context.Context, _, _, _ uuid.UUID) (*domain.DeptMembership, error) {
			removeCalled = true
			return &domain.DeptMembership{ID: uuid.New()}, nil
		},
	}
	// delegations==nil path: skip pre-lookup entirely.
	svc := service.NewDeptMembershipService(repo, nil, nil, nil, wf, nil, &passthroughTxRunner{})

	got, err := svc.Remove(context.Background(), uuid.New(), uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.True(t, removeCalled, "fail-open: workflow-svc error must not block removal")
}

func TestDeptMembership_Remove_TxErrorPropagates(t *testing.T) {
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		},
	}
	txErr := errors.New("tx aborted")
	svc := service.NewDeptMembershipService(&fakeDeptMemRepo{}, nil, nil, nil, wf, nil, &passthroughTxRunner{runErr: txErr})

	_, err := svc.Remove(context.Background(), uuid.New(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, txErr)
}

func TestDeptMembership_Remove_RepoRemoveErrorPropagates(t *testing.T) {
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		},
	}
	removeErr := errors.New("dept row missing")
	repo := &fakeDeptMemRepo{
		removeFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.DeptMembership, error) {
			return nil, removeErr
		},
	}
	svc := service.NewDeptMembershipService(repo, nil, nil, nil, wf, nil, &passthroughTxRunner{})

	_, err := svc.Remove(context.Background(), uuid.New(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, removeErr)
}

func TestDeptMembership_Remove_InvalidatesCacheOnSuccess(t *testing.T) {
	tenantID := uuid.New()
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		},
	}
	repo := &fakeDeptMemRepo{
		removeFn: func(_ context.Context, _, _, _ uuid.UUID) (*domain.DeptMembership, error) {
			return &domain.DeptMembership{ID: uuid.New()}, nil
		},
	}
	cache := &spyCache{}
	svc := service.NewDeptMembershipService(repo, nil, nil, nil, wf, cache, &passthroughTxRunner{})

	_, err := svc.Remove(context.Background(), tenantID, uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Contains(t, cache.deleteCalls, "om:members:"+tenantID.String()+":50")
	assert.Contains(t, cache.deleteCalls, "om:seat_usage:"+tenantID.String())
}
