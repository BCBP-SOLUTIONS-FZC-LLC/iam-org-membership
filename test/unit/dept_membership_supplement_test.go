// dept_membership_supplement_test.go fills coverage gaps in
// dept_membership_service.go identified from coverage.out:
//
//   - ListByDepartment (66.7%): tenantDepts.Find error → ErrDepartmentNotFound
//   - Assign (79.3%): catalog.DepartmentByID error; catalog retired dept;
//     tenantDepts.Find → ErrDepartmentNotFound; tenantDepts.Find → general error;
//     deactivated dept (IsActive=false); memberships.FindByUserID → general error;
//     deptMemberships.Assign error inside tx; event enqueue paths
//   - Remove (79.2%): workflow.GetDelegateImpact error → fail-open;
//     active workflows → 409; deptMemberships.Remove error inside tx
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

// ── ListByDepartment gap: tenantDepts.Find returns a non-DepartmentNotFound error ──

// dmsErrTenantDeptRepo is a TenantDepartmentRepository whose Find returns a
// configurable error (used to test the pre-check path in ListByDepartment).
type dmsErrTenantDeptRepo struct {
	findErr error
}

func (r *dmsErrTenantDeptRepo) Find(_ context.Context, _, _ uuid.UUID) (*domain.TenantDepartment, error) {
	return nil, r.findErr
}
func (r *dmsErrTenantDeptRepo) List(context.Context, uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *dmsErrTenantDeptRepo) ListActive(context.Context, uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *dmsErrTenantDeptRepo) Activate(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
	return nil, nil
}
func (r *dmsErrTenantDeptRepo) SetActive(context.Context, uuid.UUID, uuid.UUID, bool, int64) (*domain.TenantDepartment, error) {
	return nil, nil
}

var _ port.TenantDepartmentRepository = (*dmsErrTenantDeptRepo)(nil)

// TestDeptMembershipService_ListByDepartment_FindError_PropagatesWrapped verifies
// that when tenantDepts.Find returns an error (any error — the ListByDepartment
// impl wraps it as ErrDepartmentNotFound), the caller sees that domain error.
func TestDeptMembershipService_ListByDepartment_FindError_PropagatesWrapped(t *testing.T) {
	dbErr := domain.NewError(domain.ErrDepartmentNotFound, "not activated")
	svc := service.NewDeptMembershipService(
		nil, nil,
		&dmsErrTenantDeptRepo{findErr: dbErr},
		nil, nil, nil, nil, nil,
	)

	_, err := svc.ListByDepartment(context.Background(), uuid.New(), uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrDepartmentNotFound)
}

// ── Assign gaps ───────────────────────────────────────────────────────────

// dmsFullDeptMemRepo is a DeptMembershipRepository whose Assign is configurable.
type dmsFullDeptMemRepo struct {
	listByUserFn func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error)
	assignFn     func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, domain.DeptRole, uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error)
}

func (f *dmsFullDeptMemRepo) ListByUser(ctx context.Context, t, u uuid.UUID) ([]domain.DeptMembership, error) {
	if f.listByUserFn != nil {
		return f.listByUserFn(ctx, t, u)
	}
	return nil, nil
}
func (f *dmsFullDeptMemRepo) ListByDepartment(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (f *dmsFullDeptMemRepo) Assign(ctx context.Context, tenantID, userID, deptID, memID uuid.UUID, level domain.DeptRole, actorID uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
	if f.assignFn != nil {
		return f.assignFn(ctx, tenantID, userID, deptID, memID, level, actorID)
	}
	return &domain.DeptMembership{ID: uuid.New(), DepartmentID: deptID, RoleLevel: level}, nil, nil
}
func (f *dmsFullDeptMemRepo) Remove(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.DeptMembership, error) {
	return nil, nil
}
func (f *dmsFullDeptMemRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (f *dmsFullDeptMemRepo) SoftDeleteAllForDept(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}

var _ port.DeptMembershipRepository = (*dmsFullDeptMemRepo)(nil)

// dmsMembershipRepo is a MembershipRepository stub for Assign tests.
type dmsMembershipRepo struct {
	findByUserIDFn func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error)
}

func (r *dmsMembershipRepo) List(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
	return nil, nil
}
func (r *dmsMembershipRepo) FindByUserID(ctx context.Context, t, u uuid.UUID) (*domain.TenantMembership, error) {
	if r.findByUserIDFn != nil {
		return r.findByUserIDFn(ctx, t, u)
	}
	return &domain.TenantMembership{ID: uuid.New(), Status: domain.MembershipActive}, nil
}
func (r *dmsMembershipRepo) Insert(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *dmsMembershipRepo) SetStatus(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *dmsMembershipRepo) SoftDelete(context.Context, uuid.UUID, uuid.UUID, int64) error {
	return nil
}
func (r *dmsMembershipRepo) CountActive(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (r *dmsMembershipRepo) ListActiveUserIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

var _ port.MembershipRepository = (*dmsMembershipRepo)(nil)

// dmsDeptCatalogReader is a DepartmentCatalogReader stub for Assign tests.
type dmsDeptCatalogReader struct {
	deptByIDFn func(context.Context, uuid.UUID) (*domain.Department, error)
}

func (r *dmsDeptCatalogReader) Departments(context.Context) ([]domain.Department, error) {
	return nil, nil
}
func (r *dmsDeptCatalogReader) DepartmentByID(ctx context.Context, id uuid.UUID) (*domain.Department, error) {
	if r.deptByIDFn != nil {
		return r.deptByIDFn(ctx, id)
	}
	return &domain.Department{ID: id, IsActive: true}, nil
}

var _ port.DepartmentCatalogReader = (*dmsDeptCatalogReader)(nil)

// dmsActiveTenantDeptRepo is a TenantDepartmentRepository whose Find
// behaviour is configurable for Assign tests.
type dmsActiveTenantDeptRepo struct {
	findFn func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error)
}

func (r *dmsActiveTenantDeptRepo) Find(ctx context.Context, tid, did uuid.UUID) (*domain.TenantDepartment, error) {
	if r.findFn != nil {
		return r.findFn(ctx, tid, did)
	}
	return &domain.TenantDepartment{TenantID: tid, DepartmentID: did, IsActive: true}, nil
}
func (r *dmsActiveTenantDeptRepo) List(context.Context, uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *dmsActiveTenantDeptRepo) ListActive(context.Context, uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *dmsActiveTenantDeptRepo) Activate(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
	return nil, nil
}
func (r *dmsActiveTenantDeptRepo) SetActive(context.Context, uuid.UUID, uuid.UUID, bool, int64) (*domain.TenantDepartment, error) {
	return nil, nil
}

var _ port.TenantDepartmentRepository = (*dmsActiveTenantDeptRepo)(nil)

// buildAssignSvc is a helper that wires a DeptMembershipService for Assign tests.
func buildAssignSvc(
	dm port.DeptMembershipRepository,
	m port.MembershipRepository,
	td port.TenantDepartmentRepository,
	cat port.DepartmentCatalogReader,
) *service.DeptMembershipService {
	return service.NewDeptMembershipService(dm, m, td, cat, nil, nil, nil, &passthroughTxRunner{})
}

// TestDeptMembership_Assign_InvalidRole_Returns422 verifies that an unknown
// role level is rejected before any repository call.
func TestDeptMembership_Assign_InvalidRole_Returns422(t *testing.T) {
	svc := buildAssignSvc(nil, nil, nil, nil)

	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(), "invalid_level", uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrInvalidRole)
}

// TestDeptMembership_Assign_CatalogError_Propagates verifies that a
// catalog.DepartmentByID error propagates from Assign before the tx.
func TestDeptMembership_Assign_CatalogError_Propagates(t *testing.T) {
	catErr := errors.New("catalog_unavailable")
	catalog := &dmsDeptCatalogReader{
		deptByIDFn: func(context.Context, uuid.UUID) (*domain.Department, error) {
			return nil, catErr
		},
	}
	svc := buildAssignSvc(nil, nil, &dmsActiveTenantDeptRepo{}, catalog)

	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(), domain.DeptPreparator, uuid.New())

	require.ErrorIs(t, err, catErr)
}

// TestDeptMembership_Assign_RetiredDept_ReturnsErrDepartmentRetired verifies
// that a globally retired department (IsActive=false in the catalog) is rejected
// with ErrDepartmentRetired.
func TestDeptMembership_Assign_RetiredDept_ReturnsErrDepartmentRetired(t *testing.T) {
	catalog := &dmsDeptCatalogReader{
		deptByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Department, error) {
			return &domain.Department{ID: id, IsActive: false}, nil
		},
	}
	svc := buildAssignSvc(nil, nil, &dmsActiveTenantDeptRepo{}, catalog)

	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(), domain.DeptPreparator, uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrDepartmentRetired)
}

// TestDeptMembership_Assign_TenantDeptNotFound_ReturnsErrDepartmentDeactivated
// verifies that when tenantDepts.Find returns ErrDepartmentNotFound (never
// activated), Assign returns ErrDepartmentDeactivated.
func TestDeptMembership_Assign_TenantDeptNotFound_ReturnsErrDepartmentDeactivated(t *testing.T) {
	tenantDepts := &dmsActiveTenantDeptRepo{
		findFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
			return nil, domain.NewError(domain.ErrDepartmentNotFound, "not in tenant_departments")
		},
	}
	svc := buildAssignSvc(nil, nil, tenantDepts, nil)

	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(), domain.DeptPreparator, uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrDepartmentDeactivated)
}

// TestDeptMembership_Assign_TenantDeptFindGeneralError_Propagates verifies
// that a non-ErrDepartmentNotFound error from tenantDepts.Find propagates.
func TestDeptMembership_Assign_TenantDeptFindGeneralError_Propagates(t *testing.T) {
	dbErr := errors.New("db_timeout")
	tenantDepts := &dmsActiveTenantDeptRepo{
		findFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
			return nil, dbErr
		},
	}
	svc := buildAssignSvc(nil, nil, tenantDepts, nil)

	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(), domain.DeptPreparator, uuid.New())

	assert.ErrorIs(t, err, dbErr)
}

// TestDeptMembership_Assign_TenantDeptDeactivated_ReturnsErrDepartmentDeactivated
// verifies that when tenantDepts.Find returns a row with IsActive=false,
// Assign returns ErrDepartmentDeactivated.
func TestDeptMembership_Assign_TenantDeptDeactivated_ReturnsErrDepartmentDeactivated(t *testing.T) {
	tenantDepts := &dmsActiveTenantDeptRepo{
		findFn: func(_ context.Context, tid, did uuid.UUID) (*domain.TenantDepartment, error) {
			return &domain.TenantDepartment{TenantID: tid, DepartmentID: did, IsActive: false}, nil
		},
	}
	svc := buildAssignSvc(nil, nil, tenantDepts, nil)

	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(), domain.DeptPreparator, uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrDepartmentDeactivated)
}

// TestDeptMembership_Assign_MemberNotFound_ReturnsErrMemberNotActive verifies
// that when memberships.FindByUserID returns ErrMemberNotFound, Assign
// surfaces ErrMemberNotActive (DM-2: must be a tenant member to get a dept role).
func TestDeptMembership_Assign_MemberNotFound_ReturnsErrMemberNotActive(t *testing.T) {
	mem := &dmsMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return nil, domain.NewError(domain.ErrMemberNotFound, "not a member")
		},
	}
	svc := buildAssignSvc(nil, mem, &dmsActiveTenantDeptRepo{}, nil)

	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(), domain.DeptPreparator, uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrMemberNotActive)
}

// TestDeptMembership_Assign_MemberFindGeneralError_Propagates verifies
// that a non-ErrMemberNotFound error from FindByUserID propagates.
func TestDeptMembership_Assign_MemberFindGeneralError_Propagates(t *testing.T) {
	dbErr := errors.New("db_connection_lost")
	mem := &dmsMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return nil, dbErr
		},
	}
	svc := buildAssignSvc(nil, mem, &dmsActiveTenantDeptRepo{}, nil)

	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(), domain.DeptPreparator, uuid.New())

	assert.ErrorIs(t, err, dbErr)
}

// TestDeptMembership_Assign_SuspendedMember_ReturnsErrMemberNotActive verifies
// that a suspended tenant member cannot receive a dept assignment (DM-2).
func TestDeptMembership_Assign_SuspendedMember_ReturnsErrMemberNotActive(t *testing.T) {
	mem := &dmsMembershipRepo{
		findByUserIDFn: func(_ context.Context, _, uid uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), UserID: uid, Status: domain.MembershipSuspended}, nil
		},
	}
	svc := buildAssignSvc(nil, mem, &dmsActiveTenantDeptRepo{}, nil)

	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(), domain.DeptPreparator, uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrMemberNotActive)
}

// TestDeptMembership_Assign_DeptMemRepoError_Propagates verifies that an
// error from deptMemberships.Assign inside the tx propagates to the caller.
func TestDeptMembership_Assign_DeptMemRepoError_Propagates(t *testing.T) {
	assignErr := errors.New("unique_constraint_violation")
	dm := &dmsFullDeptMemRepo{
		assignFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, domain.DeptRole, uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
			return nil, nil, assignErr
		},
	}
	svc := buildAssignSvc(dm, &dmsMembershipRepo{}, &dmsActiveTenantDeptRepo{}, nil)

	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(), domain.DeptPreparator, uuid.New())

	assert.ErrorIs(t, err, assignErr)
}

// TestDeptMembership_Assign_HappyPath_ReturnsDeptMembership verifies the
// happy-path: active tenant dept + active member + successful repo.Assign
// → non-nil DeptMembership returned.
func TestDeptMembership_Assign_HappyPath_ReturnsDeptMembership(t *testing.T) {
	deptID := uuid.New()
	want := &domain.DeptMembership{ID: uuid.New(), DepartmentID: deptID, RoleLevel: domain.DeptReviewer}
	dm := &dmsFullDeptMemRepo{
		assignFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, domain.DeptRole, uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
			return want, nil, nil
		},
	}
	svc := buildAssignSvc(dm, &dmsMembershipRepo{}, &dmsActiveTenantDeptRepo{}, nil)

	got, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), deptID, domain.DeptReviewer, uuid.New())

	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, want.ID, got.ID)
}

// ── Remove gaps ───────────────────────────────────────────────────────────

// TestDeptMembership_Remove_WorkflowError_FailsOpen verifies WFI-9: when
// workflow.GetDelegateImpact returns an error, Remove proceeds (fail-open)
// rather than blocking.
func TestDeptMembership_Remove_WorkflowError_FailsOpen(t *testing.T) {
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return nil, errors.New("workflow_service_unavailable")
		},
	}
	dm := &dmsFullDeptMemRepo{}
	dm.assignFn = nil // Remove uses removeFn — but dmsFullDeptMemRepo has Remove returning nil, nil

	// We need a Remove method — use fakeDeptMemRepo instead.
	fakeRepo := &fakeDeptMemRepo{
		removeFn: func(_ context.Context, _, _, _ uuid.UUID) (*domain.DeptMembership, error) {
			return &domain.DeptMembership{ID: uuid.New()}, nil
		},
	}

	svc := service.NewDeptMembershipService(fakeRepo, nil, nil, nil, nil, wf, nil, &passthroughTxRunner{})

	got, err := svc.Remove(context.Background(), uuid.New(), uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err, "WFI-9: workflow error must not block removal (fail-open)")
	require.NotNil(t, got)
}

// TestDeptMembership_Remove_ActiveWorkflows_Returns409 verifies that when
// GetDelegateImpact reports active_workflows > 0, Remove returns
// ErrWorkflowResolutionRequired.
func TestDeptMembership_Remove_ActiveWorkflows_Returns409(t *testing.T) {
	wfID := uuid.New()
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{ActiveWorkflows: 1, WorkflowIDs: []uuid.UUID{wfID}}, nil
		},
	}
	svc := service.NewDeptMembershipService(nil, nil, nil, nil, nil, wf, nil, &passthroughTxRunner{})

	_, err := svc.Remove(context.Background(), uuid.New(), uuid.New(), uuid.New(), uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrWorkflowResolutionRequired)
}

// TestDeptMembership_Remove_RepoError_Propagates verifies that an error from
// deptMemberships.Remove inside the tx propagates to the caller.
func TestDeptMembership_Remove_RepoError_Propagates(t *testing.T) {
	removeErr := errors.New("dept_not_found_in_repo")
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{ActiveWorkflows: 0}, nil
		},
	}
	fakeRepo := &fakeDeptMemRepo{
		removeFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.DeptMembership, error) {
			return nil, removeErr
		},
	}
	svc := service.NewDeptMembershipService(fakeRepo, nil, nil, nil, nil, wf, nil, &passthroughTxRunner{})

	_, err := svc.Remove(context.Background(), uuid.New(), uuid.New(), uuid.New(), uuid.New())

	assert.ErrorIs(t, err, removeErr)
}

// TestDeptMembership_Remove_WithPublisher_EmitsEvent covers lines 239-250 in
// dept_membership_service.go: the event construction block inside Remove's
// RunInTx closure when pub != nil.
func TestDeptMembership_Remove_WithPublisher_EmitsEvent(t *testing.T) {
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{ActiveWorkflows: 0}, nil
		},
	}
	removedDM := &domain.DeptMembership{ID: uuid.New()}
	fakeRepo := &fakeDeptMemRepo{
		removeFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.DeptMembership, error) {
			return removedDM, nil
		},
	}
	pub := &deptPub{}
	txRunner := &deptTxRunner{pub: pub}

	svc := service.NewDeptMembershipService(fakeRepo, nil, nil, nil, nil, wf, nil, txRunner)

	tenantID, userID, deptID, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	got, err := svc.Remove(context.Background(), tenantID, userID, deptID, actorID)

	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, pub.events, 1, "DepartmentMembershipRevoked event must be emitted")
	assert.Equal(t, domain.EventDepartmentMembershipRevoked, pub.events[0].Type)
}

// TestDeptMembership_Assign_WithPublisher_IPUserAgentFromContext covers lines
// 173-176 in dept_membership_service.go: extracting IP/UserAgent from
// requestctx inside the Assign tx closure.
// This covers the `if rc, ok := requestctx.FromContext(txCtx); ok` branch.
func TestDeptMembership_Assign_WithPublisher_WithRequestCtx_ExtractsIPUA(t *testing.T) {
	dm := &dmsFullDeptMemRepo{
		assignFn: func(ctx context.Context, tid, uid, did, memID uuid.UUID, level domain.DeptRole, actorID uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
			return &domain.DeptMembership{TenantID: tid, UserID: uid, DepartmentID: did, RoleLevel: level}, nil, nil
		},
	}
	pub := &deptPub{}

	// rcAndPubTxRunner injects both a publisher AND a requestctx into txCtx.
	txRunner := &rcAndPubTxRunner{pub: pub, ip: "10.0.0.1", ua: "test-ua/1.0"}

	svc := service.NewDeptMembershipService(
		dm, &dmsMembershipRepo{}, &dmsActiveTenantDeptRepo{}, nil, nil, nil, nil, txRunner,
	)

	tenantID, userID, deptID, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	_, err := svc.Assign(context.Background(), tenantID, userID, deptID, domain.DeptReviewer, actorID)

	require.NoError(t, err)
	require.Len(t, pub.events, 1, "event must be emitted")
	evt := pub.events[0]
	assert.Equal(t, "10.0.0.1", evt.IPAddress, "IP must be extracted from requestctx")
	assert.Equal(t, "test-ua/1.0", evt.UserAgent, "UserAgent must be extracted from requestctx")
}

// rcAndPubTxRunner injects both an event publisher and a requestctx into the
// tx context so IP/UserAgent extraction branches in Assign/Remove are reached.
type rcAndPubTxRunner struct {
	pub *deptPub
	ip  string
	ua  string
}

func (r *rcAndPubTxRunner) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	ctx = port.WithEventPublisher(ctx, r.pub)
	ctx = requestctx.WithContext(ctx, &requestctx.RequestContext{
		ClientIP:  r.ip,
		UserAgent: r.ua,
	})
	return fn(ctx)
}

var _ port.TxRunner = (*rcAndPubTxRunner)(nil)

// ── AUTH-9 (IB-3) — service-account defense-in-depth reject ────────────────

// fakeTokenServiceClient is a settable stand-in for port.TokenServiceClient,
// shared across this package's AUTH-9 tests (Assign here, ReconcileRoles in
// membership_scenarios_test.go).
type fakeTokenServiceClient struct {
	isServiceAccountFn func(context.Context, uuid.UUID, uuid.UUID) (bool, error)
}

func (f *fakeTokenServiceClient) IsServiceAccount(ctx context.Context, tenantID, userID uuid.UUID) (bool, error) {
	if f.isServiceAccountFn != nil {
		return f.isServiceAccountFn(ctx, tenantID, userID)
	}
	return false, nil
}

var _ port.TokenServiceClient = (*fakeTokenServiceClient)(nil)

// TestDeptMembership_Assign_ServiceAccountTarget_Returns403 verifies AUTH-9:
// a target user that Token Service reports as a service account is rejected
// before any other check (invalid-level check already passed).
func TestDeptMembership_Assign_ServiceAccountTarget_Returns403(t *testing.T) {
	ts := &fakeTokenServiceClient{
		isServiceAccountFn: func(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
			return true, nil
		},
	}
	svc := service.NewDeptMembershipService(nil, nil, nil, nil, nil, nil, nil, &passthroughTxRunner{}).
		WithTokenServiceClient(ts)

	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(), domain.DeptPreparator, uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrServiceAccountNotGrantable)
}

// TestDeptMembership_Assign_TokenServiceError_FailsOpen verifies AUTH-9's
// fail-open degrade: a Token Service error must not block Assign — the
// primary guarantee is the structural composite-FK bar (TR-8/DM-4).
func TestDeptMembership_Assign_TokenServiceError_FailsOpen(t *testing.T) {
	ts := &fakeTokenServiceClient{
		isServiceAccountFn: func(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
			return false, errors.New("token_service_unavailable")
		},
	}
	svc := buildAssignSvc(
		&dmsFullDeptMemRepo{}, &dmsMembershipRepo{}, &dmsActiveTenantDeptRepo{}, nil,
	).WithTokenServiceClient(ts)

	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(), domain.DeptPreparator, uuid.New())

	require.NoError(t, err, "AUTH-9 check must fail open on Token Service outage")
}
