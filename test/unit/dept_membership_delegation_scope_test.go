package unit_test

// P-10 demotion and P-11 removal: the §8.8.4 dept-scope pre-filter decides
// the Workflow check. Three Delegation outcomes, each with its own rule:
//
//	not found      → zero Workflow calls          (WFI-9, LLD line 3090)
//	lookup failed  → tenant-wide Workflow check   (§20.5, LLD line 4120; WFI-11)
//	found          → dept-scoped check with the delegation_id (§8.8.4)
//
// and a Workflow failure on any check is fail-closed: 503, no DB write
// (WFI-8, LLD line 3050; §20.5, LLD line 4119).

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

// ── fixtures ──────────────────────────────────────────────────────────────

// recordingWorkflow counts GetDelegateImpact calls and records the
// delegation_id of each.
type recordingWorkflow struct {
	fakeWorkflowClient
	calls []*uuid.UUID
	ret   *port.DelegateImpact
	err   error
}

func (w *recordingWorkflow) GetDelegateImpact(_ context.Context, _, _ uuid.UUID, delegationID *uuid.UUID) (*port.DelegateImpact, error) {
	w.calls = append(w.calls, delegationID)
	if w.err != nil {
		return nil, w.err
	}
	if w.ret == nil {
		return &port.DelegateImpact{}, nil
	}
	return w.ret, nil
}

func delegationNotFound() *fakeDelegationCheckClient {
	return &fakeDelegationCheckClient{deptDelegateFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*uuid.UUID, error) {
		return nil, nil
	}}
}

func delegationFailed() *fakeDelegationCheckClient {
	return &fakeDelegationCheckClient{deptDelegateFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*uuid.UUID, error) {
		return nil, errors.New("delegation 503")
	}}
}

func delegationFound(id uuid.UUID) *fakeDelegationCheckClient {
	return &fakeDelegationCheckClient{deptDelegateFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*uuid.UUID, error) {
		return &id, nil
	}}
}

// demotionSvc builds a service where the user is currently an approver in
// deptID, so an Assign at preparator is a demotion. *wrote reports whether
// the dept-membership write ran.
func demotionSvc(t *testing.T, deptID uuid.UUID, del port.DelegationCheckClient, wf port.WorkflowClient) (*service.DeptMembershipService, *bool) {
	t.Helper()
	wrote := false
	current := domain.DeptMembership{DepartmentID: deptID, RoleLevel: domain.DeptApprover}
	repo := &gapDeptMemRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			return []domain.DeptMembership{current}, nil
		},
		assignFn: func(_ context.Context, _, _, _, _ uuid.UUID, level domain.DeptRole, _ uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
			wrote = true
			return &domain.DeptMembership{ID: uuid.New(), DepartmentID: deptID, RoleLevel: level}, &current, nil
		},
	}
	svc := service.NewDeptMembershipService(repo, &activeMemberRepo{}, &activeTenantDeptRepo{}, nil, del, wf, nil, &passthroughTxRunner{})
	return svc, &wrote
}

func demote(svc *service.DeptMembershipService, deptID uuid.UUID) (*domain.DeptMembership, error) {
	return svc.Assign(context.Background(), uuid.New(), uuid.New(), deptID, domain.DeptPreparator, uuid.New())
}

// removalSvc builds a P-11 service; *wrote reports whether the removal ran.
func removalSvc(del port.DelegationCheckClient, wf port.WorkflowClient) (*service.DeptMembershipService, *bool) {
	wrote := false
	repo := &fakeDeptMemRepo{removeFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.DeptMembership, error) {
		wrote = true
		return &domain.DeptMembership{ID: uuid.New()}, nil
	}}
	return service.NewDeptMembershipService(repo, nil, nil, nil, del, wf, nil, &passthroughTxRunner{}), &wrote
}

func remove(svc *service.DeptMembershipService) (*domain.DeptMembership, error) {
	return svc.Remove(context.Background(), uuid.New(), uuid.New(), uuid.New(), uuid.New())
}

// ── P-10 demotion ─────────────────────────────────────────────────────────

// LLD WFI-9 (line 3090): "No active `scope='department'` delegation naming the
// user as delegate for this dept (nil result) → zero added Workflow Service
// calls, zero behavior change from today."
func TestP10Demotion_DelegationNotFound_ZeroWorkflowCalls_Proceeds(t *testing.T) {
	deptID := uuid.New()
	wf := &recordingWorkflow{err: errors.New("must not be called")}
	svc, wrote := demotionSvc(t, deptID, delegationNotFound(), wf)

	got, err := demote(svc, deptID)
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, wf.calls, "WFI-9: no dept delegation → zero Workflow calls")
	assert.True(t, *wrote)
}

// LLD §20.5 (line 4120): "— dept-scope precision leg (P-10/P-11) | Delegation
// (`dept-delegate`) | **degrade** | falls back to tenant-wide impact (correct,
// less precise)".
func TestP10Demotion_DelegationLookupFails_TenantWideCheck_409(t *testing.T) {
	deptID := uuid.New()
	wf := &recordingWorkflow{ret: &port.DelegateImpact{ActiveWorkflows: 2, WorkflowIDs: []uuid.UUID{uuid.New(), uuid.New()}}}
	svc, wrote := demotionSvc(t, deptID, delegationFailed(), wf)

	_, err := demote(svc, deptID)
	require.ErrorIs(t, err, domain.ErrWorkflowResolutionRequired)
	require.Len(t, wf.calls, 1, "one Workflow call")
	assert.Nil(t, wf.calls[0], "failed lookup → tenant-wide check (nil delegation_id)")
	assert.False(t, *wrote, "WFI-3: no state change on 409")
}

// LLD §20.5 (line 4120) for the fallback, and LLD WFI-8 (line 3050): "A
// Workflow Service call failure (`5xx`/timeout) at **any** step … returns
// `503 workflow_service_unavailable` with no DB write".
func TestP10Demotion_DelegationLookupFails_WorkflowError_503NoWrite(t *testing.T) {
	deptID := uuid.New()
	wf := &recordingWorkflow{err: errors.New("workflow 502")}
	svc, wrote := demotionSvc(t, deptID, delegationFailed(), wf)

	_, err := demote(svc, deptID)
	require.ErrorIs(t, err, domain.ErrWorkflowServiceUnavailable)
	require.Len(t, wf.calls, 1)
	assert.Nil(t, wf.calls[0])
	assert.False(t, *wrote, "WFI-8: no DB write")
}

// LLD §8.8.4 dept-scope (test-plan line 3635): "mock it returning a match +
// `GetDelegateImpact` `>0` → `ErrWorkflowResolutionRequired`" — the
// Workflow query is scoped to the found delegation_id.
func TestP10Demotion_DelegationFound_DeptScopedCheck_Proceeds(t *testing.T) {
	deptID, delegationID := uuid.New(), uuid.New()
	wf := &recordingWorkflow{}
	svc, wrote := demotionSvc(t, deptID, delegationFound(delegationID), wf)

	_, err := demote(svc, deptID)
	require.NoError(t, err)
	require.Len(t, wf.calls, 1)
	require.NotNil(t, wf.calls[0])
	assert.Equal(t, delegationID, *wf.calls[0], "dept-scoped check carries the delegation_id")
	assert.True(t, *wrote)
}

// LLD §20.5 (line 4119): "User removal / dept demotion·removal
// (P-8/I-5/P-10/P-11) | Workflow (`GetDelegateImpact`) | fail-closed |
// `503 workflow_service_unavailable`, no change (WFI-8)".
func TestP10Demotion_DelegationFound_WorkflowError_503NoWrite(t *testing.T) {
	deptID := uuid.New()
	wf := &recordingWorkflow{err: errors.New("workflow timeout")}
	svc, wrote := demotionSvc(t, deptID, delegationFound(uuid.New()), wf)

	_, err := demote(svc, deptID)
	require.ErrorIs(t, err, domain.ErrWorkflowServiceUnavailable)
	assert.False(t, *wrote)
}

// ── P-11 removal ──────────────────────────────────────────────────────────

// LLD WFI-9 (line 3090): "The department-scope pre-filter runs **before** any
// Workflow Service call for P-10/P-11 … (nil result) → zero added Workflow
// Service calls". The Workflow fake would fail if called, so a successful
// removal proves the call was skipped.
func TestP11Remove_DelegationNotFound_ZeroWorkflowCalls_Proceeds(t *testing.T) {
	wf := &recordingWorkflow{err: errors.New("must not be called")}
	svc, wrote := removalSvc(delegationNotFound(), wf)

	got, err := remove(svc)
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, wf.calls, "WFI-9: no dept delegation → zero Workflow calls")
	assert.True(t, *wrote)
}

// LLD §20.5 (line 4120): "falls back to tenant-wide impact (correct, less
// precise)"; WFI-8 (line 3050): "A **Delegation Service** outage on the
// §8.8.4 dept-scope lookup does **not** `503` — it degrades to tenant-wide
// impact (WFI-11)".
func TestP11Remove_DelegationLookupFails_TenantWideCheck_Proceeds(t *testing.T) {
	wf := &recordingWorkflow{}
	svc, wrote := removalSvc(delegationFailed(), wf)

	_, err := remove(svc)
	require.NoError(t, err)
	require.Len(t, wf.calls, 1)
	assert.Nil(t, wf.calls[0], "failed lookup → tenant-wide check (nil delegation_id)")
	assert.True(t, *wrote)
}

// LLD §8.8.4 / WFI-11: a found dept delegation scopes the Workflow query to
// its delegation_id; LLD §17 (line 3909) 409 `workflow_resolution_required`
// for "dept removal (P-11): the user is a delegate on active workflows. …
// No state change, no event (WFI-3)".
func TestP11Remove_DelegationFound_DeptScopedCheck_409NoWrite(t *testing.T) {
	delegationID := uuid.New()
	wf := &recordingWorkflow{ret: &port.DelegateImpact{ActiveWorkflows: 2, WorkflowIDs: []uuid.UUID{uuid.New(), uuid.New()}}}
	svc, wrote := removalSvc(delegationFound(delegationID), wf)

	_, err := remove(svc)
	require.ErrorIs(t, err, domain.ErrWorkflowResolutionRequired)
	require.Len(t, wf.calls, 1)
	require.NotNil(t, wf.calls[0])
	assert.Equal(t, delegationID, *wf.calls[0])
	assert.False(t, *wrote)
}

// LLD §20.5 (line 4119): P-11 against Workflow is "fail-closed | `503
// workflow_service_unavailable`, no change (WFI-8)" — also on the
// tenant-wide fallback path.
func TestP11Remove_DelegationLookupFails_WorkflowError_503NoWrite(t *testing.T) {
	wf := &recordingWorkflow{err: errors.New("workflow 502")}
	svc, wrote := removalSvc(delegationFailed(), wf)

	_, err := remove(svc)
	require.ErrorIs(t, err, domain.ErrWorkflowServiceUnavailable)
	assert.False(t, *wrote)
}
