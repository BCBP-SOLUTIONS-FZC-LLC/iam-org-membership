// Unit tests for internal/core/service/dept_membership_service.go
// ListByDepartment (P-9) and Remove (P-11).
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
func (f *fakeDeptMemRepo) Assign(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, domain.DeptRole, uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
	return nil, nil, errors.New("not used")
}
func (f *fakeDeptMemRepo) Remove(ctx context.Context, tenantID, userID, departmentID uuid.UUID) (*domain.DeptMembership, error) {
	return f.removeFn(ctx, tenantID, userID, departmentID)
}
func (f *fakeDeptMemRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (f *fakeDeptMemRepo) SoftDeleteAllForDept(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}

var _ port.DeptMembershipRepository = (*fakeDeptMemRepo)(nil)

// ── DelegationCheckClient stub (ADR-0008 v2 — replaces the local
//    DelegationRepository.FindActiveDeptDelegateForUser lookup with a
//    Core → Delegation Service DLG-I3 call) ─────────────────────────────

type fakeDelegationCheckClient struct {
	deptDelegateFn func(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*uuid.UUID, error)
}

func (f *fakeDelegationCheckClient) DeptDelegate(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*uuid.UUID, error) {
	return f.deptDelegateFn(ctx, tenantID, userID, deptID)
}

var _ port.DelegationCheckClient = (*fakeDelegationCheckClient)(nil)

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

// activeTenantDeptRepo returns a found (active) tenant-department for any lookup.
// Used to satisfy the P9-VAL-04 pre-check in ListByDepartment without triggering a nil panic.
type activeTenantDeptRepo struct{}

func (r *activeTenantDeptRepo) Find(_ context.Context, tid, did uuid.UUID) (*domain.TenantDepartment, error) {
	return &domain.TenantDepartment{TenantID: tid, DepartmentID: did, IsActive: true}, nil
}
func (r *activeTenantDeptRepo) List(context.Context, uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *activeTenantDeptRepo) ListActive(context.Context, uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *activeTenantDeptRepo) Activate(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
	return nil, nil
}
func (r *activeTenantDeptRepo) SetActive(context.Context, uuid.UUID, uuid.UUID, bool, int64) (*domain.TenantDepartment, error) {
	return nil, nil
}

var _ port.TenantDepartmentRepository = (*activeTenantDeptRepo)(nil)

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
	svc := service.NewDeptMembershipService(repo, nil, &activeTenantDeptRepo{}, nil, nil, nil, nil, nil)

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
	svc := service.NewDeptMembershipService(repo, nil, &activeTenantDeptRepo{}, nil, nil, nil, nil, nil)

	_, err := svc.ListByDepartment(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, repoErr)
}

// ── Remove (P-11) — WFI-11 department-scoped delegate impact ───────────

// ADR-0008 v2 (LLD §11.5): a DelegationCheckClient failure must degrade to
// a nil delegation id (tenant-wide impact, still correct, less precise) —
// never fail the whole Remove. This replaces the old
// TestDeptMembership_Remove_DelegationLookupErrorSurfaces, whose
// expectation (the error propagates) was the pre-split, local-repository
// behavior.
func TestDeptMembership_Remove_DelegationCheckErrorDegradesToNilDelegationID(t *testing.T) {
	delClient := &fakeDelegationCheckClient{
		deptDelegateFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*uuid.UUID, error) {
			return nil, errors.New("delegation service unavailable")
		},
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(_ context.Context, _, _ uuid.UUID, delegationID *uuid.UUID) (*port.DelegateImpact, error) {
			assert.Nil(t, delegationID, "DeptDelegate error → degrade to nil delegation_id (tenant-wide fallback)")
			return &port.DelegateImpact{}, nil
		},
	}
	repo := &fakeDeptMemRepo{
		removeFn: func(_ context.Context, _, _, _ uuid.UUID) (*domain.DeptMembership, error) {
			return &domain.DeptMembership{ID: uuid.New()}, nil
		},
	}
	svc := service.NewDeptMembershipService(repo, nil, nil, nil, delClient, wf, nil, &passthroughTxRunner{})

	_, err := svc.Remove(context.Background(), uuid.New(), uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err, "a Delegation Service outage must not block removal")
}

func TestDeptMembership_Remove_ReturnsWorkflowResolutionRequiredWhenImpactPositive(t *testing.T) {
	// WFI-11: department-scoped GetDelegateImpact returns active_workflows > 0
	// → 409 workflow_resolution_required (must be resolved via P-26 first).
	workflowID := uuid.New()
	delClient := &fakeDelegationCheckClient{
		deptDelegateFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*uuid.UUID, error) {
			return nil, nil // no active delegation → nil delegation_id passed through
		},
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(_ context.Context, _, _ uuid.UUID, delegationID *uuid.UUID) (*port.DelegateImpact, error) {
			assert.Nil(t, delegationID, "no active delegation → nil delegation_id (tenant-wide query fallback)")
			return &port.DelegateImpact{ActiveWorkflows: 3, WorkflowIDs: []uuid.UUID{workflowID}}, nil
		},
	}
	svc := service.NewDeptMembershipService(&fakeDeptMemRepo{}, nil, nil, nil, delClient, wf, nil, nil)

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
	delClient := &fakeDelegationCheckClient{
		deptDelegateFn: func(_ context.Context, tt, uu, dd uuid.UUID) (*uuid.UUID, error) {
			assert.Equal(t, tenantID, tt)
			assert.Equal(t, userID, uu)
			assert.Equal(t, deptID, dd)
			return &delegationID, nil
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
	svc := service.NewDeptMembershipService(repo, nil, nil, nil, delClient, wf, nil, &passthroughTxRunner{})

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
	// delegationCheck==nil path: skip pre-lookup entirely.
	svc := service.NewDeptMembershipService(repo, nil, nil, nil, nil, wf, nil, &passthroughTxRunner{})

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
	svc := service.NewDeptMembershipService(&fakeDeptMemRepo{}, nil, nil, nil, nil, wf, nil, &passthroughTxRunner{runErr: txErr})

	_, err := svc.Remove(context.Background(), uuid.New(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, txErr)
}

// ── P11-BL-03: active dept-scoped delegate with workflows > 0 → 409 ─────────
// WFI-3 / WFI-11: user is the active delegate on a scope=department delegation
// for this specific department; GetDelegateImpact receives the delegation ID
// (scoped query) and returns active_workflows > 0 → 409 workflow_resolution_required.
// The delegation_id must be threaded through so the WF query is dept-scoped.

func TestDeptMembership_Remove_ActiveDeptDelegationWithPositiveImpact_409(t *testing.T) {
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	delegationID := uuid.New()
	workflowID := uuid.New()

	delClient := &fakeDelegationCheckClient{
		deptDelegateFn: func(_ context.Context, tt, uu, dd uuid.UUID) (*uuid.UUID, error) {
			assert.Equal(t, tenantID, tt)
			assert.Equal(t, userID, uu)
			assert.Equal(t, deptID, dd)
			return &delegationID, nil
		},
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(_ context.Context, _, _ uuid.UUID, delID *uuid.UUID) (*port.DelegateImpact, error) {
			require.NotNil(t, delID, "delegation_id must be passed for dept-scoped query (WFI-11)")
			assert.Equal(t, delegationID, *delID)
			return &port.DelegateImpact{ActiveWorkflows: 1, WorkflowIDs: []uuid.UUID{workflowID}}, nil
		},
	}
	svc := service.NewDeptMembershipService(&fakeDeptMemRepo{}, nil, nil, nil, delClient, wf, nil, nil)

	_, err := svc.Remove(context.Background(), tenantID, userID, deptID, uuid.New())

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "workflow_resolution_required", de.Code)
	assert.Equal(t, 1, de.Details["active_workflows"])
	actions, ok := de.Details["allowed_actions"].([]string)
	require.True(t, ok)
	assert.Contains(t, actions, "replace_delegate")
	assert.Contains(t, actions, "stop_workflows")
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
	svc := service.NewDeptMembershipService(repo, nil, nil, nil, nil, wf, nil, &passthroughTxRunner{})

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
	svc := service.NewDeptMembershipService(repo, nil, nil, nil, nil, wf, cache, &passthroughTxRunner{})

	_, err := svc.Remove(context.Background(), tenantID, uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Contains(t, cache.deleteCalls, "om:members:"+tenantID.String()+":50")
	assert.Contains(t, cache.deleteCalls, "om:seat_usage:"+tenantID.String())
}

// ── Assign (P-10) — event field validation (AsyncAPI spec) ────────────────

// fakeTenantDeptRepo for Assign tests (unit_test package local).
type fakeTenantDeptRepoAssign struct {
	isActive bool
}

func (r *fakeTenantDeptRepoAssign) Find(_ context.Context, tid, did uuid.UUID) (*domain.TenantDepartment, error) {
	return &domain.TenantDepartment{TenantID: tid, DepartmentID: did, IsActive: r.isActive}, nil
}
func (r *fakeTenantDeptRepoAssign) List(context.Context, uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *fakeTenantDeptRepoAssign) ListActive(context.Context, uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *fakeTenantDeptRepoAssign) Activate(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
	return nil, nil
}
func (r *fakeTenantDeptRepoAssign) SetActive(context.Context, uuid.UUID, uuid.UUID, bool, int64) (*domain.TenantDepartment, error) {
	return nil, nil
}

var _ port.TenantDepartmentRepository = (*fakeTenantDeptRepoAssign)(nil)

// activeMemberRepo returns an active membership for any user.
type activeMemberRepo struct{}

func (r *activeMemberRepo) FindByUserID(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
	return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid, Status: domain.MembershipActive}, nil
}
func (r *activeMemberRepo) List(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
	return &domain.MembershipListPage{}, nil
}
func (r *activeMemberRepo) Insert(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *activeMemberRepo) SetStatus(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *activeMemberRepo) SoftDelete(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil }
func (r *activeMemberRepo) CountActive(context.Context, uuid.UUID) (int, error)           { return 1, nil }

var _ port.MembershipRepository = (*activeMemberRepo)(nil)

// deptPub captures DomainEvents emitted inside RunInTx.
type deptPub struct{ events []*domain.DomainEvent }

func (p *deptPub) Enqueue(_ context.Context, e *domain.DomainEvent) error {
	p.events = append(p.events, e)
	return nil
}

// deptTxRunner injects a deptPub so service event emission is captured.
type deptTxRunner struct{ pub *deptPub }

func (r *deptTxRunner) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	return fn(port.WithEventPublisher(ctx, r.pub))
}

// fullDeptMemRepo supports Assign + ListByUser (for snapshot).
type fullDeptMemRepo struct {
	existing *domain.DeptMembership // nil = fresh grant; non-nil = prior row
}

func (r *fullDeptMemRepo) ListByUser(_ context.Context, _, _ uuid.UUID) ([]domain.DeptMembership, error) {
	if r.existing != nil {
		return []domain.DeptMembership{*r.existing}, nil
	}
	return nil, nil
}
func (r *fullDeptMemRepo) Assign(_ context.Context, tid, uid, did, memID uuid.UUID, level domain.DeptRole, actorID uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
	return &domain.DeptMembership{TenantID: tid, UserID: uid, DepartmentID: did, RoleLevel: level, RecordVersion: 1}, r.existing, nil
}
func (r *fullDeptMemRepo) ListByDepartment(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *fullDeptMemRepo) Remove(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.DeptMembership, error) {
	return nil, nil
}
func (r *fullDeptMemRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *fullDeptMemRepo) SoftDeleteAllForDept(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}

// P10-SQS-01: DepartmentMembershipGranted has all required AsyncAPI fields.
func TestDeptMembership_Assign_GrantedEventFieldsMatchAsyncAPI(t *testing.T) {
	tenantID, userID, deptID, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	pub := &deptPub{}
	mem := &activeMemberRepo{}
	td := &fakeTenantDeptRepoAssign{isActive: true}
	svc := service.NewDeptMembershipService(&fullDeptMemRepo{}, mem, td, nil, nil, nil, nil, &deptTxRunner{pub: pub})

	_, err := svc.Assign(context.Background(), tenantID, userID, deptID, domain.DeptReviewer, actorID)
	require.NoError(t, err)
	require.Len(t, pub.events, 1, "exactly one DepartmentMembershipGranted event expected")

	evt := pub.events[0]
	assert.Equal(t, domain.EventDepartmentMembershipGranted, evt.Type)
	payload, ok := evt.Data.(domain.DepartmentMembershipGrantedPayload)
	require.True(t, ok, "payload must be DepartmentMembershipGrantedPayload")
	assert.Equal(t, userID, payload.UserID, "user_id")
	assert.Equal(t, actorID, payload.ActorID, "actor_id")
	assert.Equal(t, tenantID, payload.TenantID, "tenant_id")
	assert.Equal(t, deptID, payload.DepartmentID, "department_id")
	assert.Equal(t, domain.DeptReviewer, payload.Level, "level")
}

// P10-SQS-02: DepartmentMembershipLevelChanged has previous_level + new_level.
func TestDeptMembership_Assign_LevelChangedEventHasPreviousAndNewLevel(t *testing.T) {
	tenantID, userID, deptID, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	pub := &deptPub{}
	// Existing row at preparator — simulates prior assignment.
	existing := &domain.DeptMembership{TenantID: tenantID, UserID: userID, DepartmentID: deptID, RoleLevel: domain.DeptPreparator}
	mem := &activeMemberRepo{}
	td := &fakeTenantDeptRepoAssign{isActive: true}
	svc := service.NewDeptMembershipService(&fullDeptMemRepo{existing: existing}, mem, td, nil, nil, nil, nil, &deptTxRunner{pub: pub})

	_, err := svc.Assign(context.Background(), tenantID, userID, deptID, domain.DeptApprover, actorID)
	require.NoError(t, err)
	require.Len(t, pub.events, 1, "exactly one DepartmentMembershipLevelChanged event expected")

	evt := pub.events[0]
	assert.Equal(t, domain.EventDepartmentMembershipLevelChanged, evt.Type)
	payload, ok := evt.Data.(domain.DepartmentMembershipLevelChangedPayload)
	require.True(t, ok, "payload must be DepartmentMembershipLevelChangedPayload")
	assert.Equal(t, domain.DeptPreparator, payload.PreviousLevel, "previous_level must be preparator")
	assert.Equal(t, domain.DeptApprover, payload.NewLevel, "new_level must be approver")
	assert.Equal(t, userID, payload.UserID, "user_id")
	assert.Equal(t, actorID, payload.ActorID, "actor_id")
	assert.Equal(t, deptID, payload.DepartmentID, "department_id")
}

// P10-HAPPY-03 (service): same level → no event emitted.
func TestDeptMembership_Assign_SameLevelEmitsNoEvent(t *testing.T) {
	tenantID, userID, deptID, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	pub := &deptPub{}
	existing := &domain.DeptMembership{TenantID: tenantID, UserID: userID, DepartmentID: deptID, RoleLevel: domain.DeptApprover}
	mem := &activeMemberRepo{}
	td := &fakeTenantDeptRepoAssign{isActive: true}
	svc := service.NewDeptMembershipService(&fullDeptMemRepo{existing: existing}, mem, td, nil, nil, nil, nil, &deptTxRunner{pub: pub})

	_, err := svc.Assign(context.Background(), tenantID, userID, deptID, domain.DeptApprover, actorID)
	require.NoError(t, err)
	assert.Empty(t, pub.events, "no event must be emitted when level is unchanged (TRG-3 spirit)")
}
