// Additional unit tests for internal/core/service/dept_membership_service.go
// closing branches left uncovered by dept_membership_service_test.go /
// dept_scenarios_test.go / dept_coverage_test.go:
//   - ListByDepartment: tenantDepts.Find error branch
//   - Assign: global-catalog error / retired-department branches, tenantDepts.Find
//     error, WFI-9/12 level-decrease delegate-impact gate (block + fail-open),
//     deptMemberships.Assign repo error, pub==nil skip, tx error propagation
//   - Remove: event emission with a live EventPublisher + requestctx IP/UA
package unit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── ListByDepartment — tenantDepts.Find error branch ────────────────────

type errFindTenantDeptRepo struct {
	findErr error
}

func (r *errFindTenantDeptRepo) Find(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
	return nil, r.findErr
}
func (r *errFindTenantDeptRepo) List(context.Context, uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *errFindTenantDeptRepo) ListActive(context.Context, uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *errFindTenantDeptRepo) Activate(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
	return nil, nil
}
func (r *errFindTenantDeptRepo) SetActive(context.Context, uuid.UUID, uuid.UUID, bool, int64) (*domain.TenantDepartment, error) {
	return nil, nil
}

var _ port.TenantDepartmentRepository = (*errFindTenantDeptRepo)(nil)

func TestDeptMembership_ListByDepartment_TenantDeptFindErrorIsDepartmentNotFound(t *testing.T) {
	td := &errFindTenantDeptRepo{findErr: errors.New("no such row")}
	svc := service.NewDeptMembershipService(&fakeDeptMemRepo{}, nil, td, nil, nil, nil, nil, nil)

	_, err := svc.ListByDepartment(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrDepartmentNotFound)
}

// ── Assign — global catalog checks ──────────────────────────────────────

func TestDeptMembershipAssign_CatalogLookupErrorPropagates(t *testing.T) {
	catalogErr := errors.New("catalog-admin-config unreachable")
	catalog := &fakeDeptCatalogRepo{
		findByIDFn: func(context.Context, uuid.UUID) (*domain.Department, error) {
			return nil, catalogErr
		},
	}
	svc := service.NewDeptMembershipService(&fakeDeptMemRepo{}, nil, nil, catalog, nil, nil, nil, nil)
	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(), domain.DeptPreparator, uuid.New())
	assert.ErrorIs(t, err, catalogErr)
}

func TestDeptMembershipAssign_GloballyRetiredDepartment_Rejected(t *testing.T) {
	catalog := &fakeDeptCatalogRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Department, error) {
			return &domain.Department{ID: id, IsActive: false}, nil
		},
	}
	svc := service.NewDeptMembershipService(&fakeDeptMemRepo{}, nil, nil, catalog, nil, nil, nil, nil)
	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(), domain.DeptPreparator, uuid.New())
	assert.ErrorIs(t, err, domain.ErrDepartmentRetired)
}

func TestDeptMembershipAssign_TenantDeptFindErrorPropagates(t *testing.T) {
	findErr := errors.New("db down")
	td := &errFindTenantDeptRepo{findErr: findErr}
	svc := service.NewDeptMembershipService(&fakeDeptMemRepo{}, nil, td, nil, nil, nil, nil, nil)
	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(), domain.DeptPreparator, uuid.New())
	assert.ErrorIs(t, err, findErr)
}

// ── Assign — WFI-9/12 level-decrease delegate-impact gate ──────────────

func TestDeptMembershipAssign_LevelDecrease_ActiveDelegateBlocks(t *testing.T) {
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	delegationID := uuid.New()
	workflowID := uuid.New()
	existing := &domain.DeptMembership{TenantID: tenantID, UserID: userID, DepartmentID: deptID, RoleLevel: domain.DeptApprover}

	mem := &activeMemberRepo{}
	td := &fakeTenantDeptRepoAssign{isActive: true}
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
			require.NotNil(t, delID)
			assert.Equal(t, delegationID, *delID)
			return &port.DelegateImpact{ActiveWorkflows: 1, WorkflowIDs: []uuid.UUID{workflowID}}, nil
		},
	}
	svc := service.NewDeptMembershipService(&fullDeptMemRepo{existing: existing}, mem, td, nil, delClient, wf, nil, &passthroughTxRunner{})

	// Preparator < Approver → a level decrease, so the WFI-9/12 gate fires.
	_, err := svc.Assign(context.Background(), tenantID, userID, deptID, domain.DeptPreparator, uuid.New())

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "workflow_resolution_required", de.Code)
	assert.Equal(t, 1, de.Details["active_workflows"])
}

func TestDeptMembershipAssign_LevelDecrease_WorkflowFailOpenAllowsDecrease(t *testing.T) {
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	delegationID := uuid.New()
	existing := &domain.DeptMembership{TenantID: tenantID, UserID: userID, DepartmentID: deptID, RoleLevel: domain.DeptApprover}

	mem := &activeMemberRepo{}
	td := &fakeTenantDeptRepoAssign{isActive: true}
	delClient := &fakeDelegationCheckClient{
		deptDelegateFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*uuid.UUID, error) {
			return &delegationID, nil
		},
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return nil, errors.New("workflow svc unavailable")
		},
	}
	svc := service.NewDeptMembershipService(&fullDeptMemRepo{existing: existing}, mem, td, nil, delClient, wf, nil, &passthroughTxRunner{})

	got, err := svc.Assign(context.Background(), tenantID, userID, deptID, domain.DeptPreparator, uuid.New())
	require.NoError(t, err, "WFI-9 fail-open: workflow unavailable must allow the level decrease")
	assert.Equal(t, domain.DeptPreparator, got.RoleLevel)
}

func TestDeptMembershipAssign_LevelDecrease_NoActiveDelegationSkipsGate(t *testing.T) {
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	existing := &domain.DeptMembership{TenantID: tenantID, UserID: userID, DepartmentID: deptID, RoleLevel: domain.DeptApprover}

	mem := &activeMemberRepo{}
	td := &fakeTenantDeptRepoAssign{isActive: true}
	delClient := &fakeDelegationCheckClient{
		deptDelegateFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*uuid.UUID, error) {
			return nil, nil // not an active delegate on this dept
		},
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			t.Fatal("GetDelegateImpact must not be called when deptDelegateOrDegrade returns nil")
			return nil, nil
		},
	}
	svc := service.NewDeptMembershipService(&fullDeptMemRepo{existing: existing}, mem, td, nil, delClient, wf, nil, &passthroughTxRunner{})

	got, err := svc.Assign(context.Background(), tenantID, userID, deptID, domain.DeptPreparator, uuid.New())
	require.NoError(t, err)
	assert.Equal(t, domain.DeptPreparator, got.RoleLevel)
}

// ── Assign — repo error, pub==nil skip, and tx error propagation ───────

type assignErrDeptMemRepo struct {
	assignErr error
}

func (r *assignErrDeptMemRepo) ListByUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *assignErrDeptMemRepo) Assign(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, domain.DeptRole, uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
	return nil, nil, r.assignErr
}
func (r *assignErrDeptMemRepo) ListByDepartment(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *assignErrDeptMemRepo) Remove(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.DeptMembership, error) {
	return nil, nil
}
func (r *assignErrDeptMemRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *assignErrDeptMemRepo) SoftDeleteAllForDept(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}

var _ port.DeptMembershipRepository = (*assignErrDeptMemRepo)(nil)

func TestDeptMembershipAssign_RepoAssignErrorPropagates(t *testing.T) {
	assignErr := errors.New("fk_dm_tenant_dept violation")
	repo := &assignErrDeptMemRepo{assignErr: assignErr}
	mem := &activeMemberRepo{}
	td := &fakeTenantDeptRepoAssign{isActive: true}
	svc := service.NewDeptMembershipService(repo, mem, td, nil, nil, nil, nil, &passthroughTxRunner{})

	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(), domain.DeptReviewer, uuid.New())
	assert.ErrorIs(t, err, assignErr)
}

func TestDeptMembershipAssign_NoEventPublisherInContext_NoOp(t *testing.T) {
	// passthroughTxRunner never injects an EventPublisher — the
	// `pub == nil { return nil }` early-return branch inside Assign's tx.
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	mem := &activeMemberRepo{}
	td := &fakeTenantDeptRepoAssign{isActive: true}
	svc := service.NewDeptMembershipService(&fullDeptMemRepo{}, mem, td, nil, nil, nil, nil, &passthroughTxRunner{})

	got, err := svc.Assign(context.Background(), tenantID, userID, deptID, domain.DeptReviewer, uuid.New())
	require.NoError(t, err)
	assert.Equal(t, domain.DeptReviewer, got.RoleLevel)
}

func TestDeptMembershipAssign_TxErrorPropagates(t *testing.T) {
	txErr := errors.New("tx aborted")
	mem := &activeMemberRepo{}
	td := &fakeTenantDeptRepoAssign{isActive: true}
	svc := service.NewDeptMembershipService(&fullDeptMemRepo{}, mem, td, nil, nil, nil, nil, &passthroughTxRunner{runErr: txErr})

	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(), domain.DeptReviewer, uuid.New())
	assert.ErrorIs(t, err, txErr)
}

// ── Assign — event emission carries requestctx IP/UA when present ──────

func TestDeptMembershipAssign_GrantedEventCarriesRequestContextIPAndUA(t *testing.T) {
	tenantID, userID, deptID, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	pub := &deptPub{}
	mem := &activeMemberRepo{}
	td := &fakeTenantDeptRepoAssign{isActive: true}
	svc := service.NewDeptMembershipService(&fullDeptMemRepo{}, mem, td, nil, nil, nil, nil, &deptTxRunner{pub: pub})

	ctx := requestctx.WithContext(context.Background(), &requestctx.RequestContext{
		ClientIP: "198.51.100.9", UserAgent: "assign-test-agent",
	})
	_, err := svc.Assign(ctx, tenantID, userID, deptID, domain.DeptReviewer, actorID)
	require.NoError(t, err)
	require.Len(t, pub.events, 1)
	assert.Equal(t, "198.51.100.9", pub.events[0].IPAddress)
	assert.Equal(t, "assign-test-agent", pub.events[0].UserAgent)
}

// ── Remove — event emission with a live publisher + requestctx IP/UA ───

func TestDeptMembership_Remove_EmitsRevokedEventWithIPAndUserAgent(t *testing.T) {
	tenantID, userID, deptID, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	pub := &deptPub{}
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
	svc := service.NewDeptMembershipService(repo, nil, nil, nil, nil, wf, nil, &deptTxRunner{pub: pub})

	ctx := requestctx.WithContext(context.Background(), &requestctx.RequestContext{
		ClientIP: "203.0.113.7", UserAgent: "integration-test-agent",
	})
	_, err := svc.Remove(ctx, tenantID, userID, deptID, actorID)
	require.NoError(t, err)

	require.Len(t, pub.events, 1)
	evt := pub.events[0]
	assert.Equal(t, domain.EventDepartmentMembershipRevoked, evt.Type)
	assert.Equal(t, "203.0.113.7", evt.IPAddress)
	assert.Equal(t, "integration-test-agent", evt.UserAgent)
	payload, ok := evt.Data.(domain.DepartmentMembershipRevokedPayload)
	require.True(t, ok)
	assert.Equal(t, userID, payload.UserID)
	assert.Equal(t, deptID, payload.DepartmentID)
	assert.Equal(t, actorID, payload.ActorID)
}
