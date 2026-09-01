// Unit tests supplementing department_service.go and dept_membership_service.go
// coverage gaps identified by go test -cover.
//
// PART A — DepartmentService:
//
//	TestDepartmentService_ListForTenant_CatalogError    (91.7% gap: DepartmentByID error in loop)
//	TestDepartmentService_SetActive_RepoError           (91.7% gap: tenantDepts.SetActive error)
//	TestDepartmentService_invalidateCache_NilCache      (invalidateCache nil-cache guard)
//	TestDepartmentService_ListForTenant_EmptyList       (empty tenant_departments list)
//
// PART B — DeptMembershipService:
//
//	TestDeptMembershipService_WithLogger_ReturnsSelf    (WithLogger gap)
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

// ── PART A: DepartmentService supplemental fakes ──────────────────────────

// listableTenantDeptRepo extends the minimal fakeTenantDeptRepoFull pattern with
// a configurable List function so we can drive the ListForTenant path.
// (fakeTenantDeptRepoFull in dept_coverage_test.go hard-codes List to nil,nil.)
type listableTenantDeptRepo struct {
	listFn      func(ctx context.Context, tenantID uuid.UUID) ([]domain.TenantDepartment, error)
	setActiveFn func(ctx context.Context, tenantID, deptID uuid.UUID, active bool, version int64) (*domain.TenantDepartment, error)
}

func (r *listableTenantDeptRepo) List(ctx context.Context, tenantID uuid.UUID) ([]domain.TenantDepartment, error) {
	if r.listFn != nil {
		return r.listFn(ctx, tenantID)
	}
	return nil, nil
}
func (r *listableTenantDeptRepo) ListActive(_ context.Context, _ uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *listableTenantDeptRepo) Find(_ context.Context, tid, did uuid.UUID) (*domain.TenantDepartment, error) {
	return &domain.TenantDepartment{TenantID: tid, DepartmentID: did, IsActive: true}, nil
}
func (r *listableTenantDeptRepo) Activate(_ context.Context, tid, did uuid.UUID) (*domain.TenantDepartment, error) {
	return &domain.TenantDepartment{TenantID: tid, DepartmentID: did, IsActive: true}, nil
}
func (r *listableTenantDeptRepo) SetActive(ctx context.Context, tid, did uuid.UUID, active bool, ver int64) (*domain.TenantDepartment, error) {
	if r.setActiveFn != nil {
		return r.setActiveFn(ctx, tid, did, active, ver)
	}
	return &domain.TenantDepartment{TenantID: tid, DepartmentID: did, IsActive: active}, nil
}

var _ port.TenantDepartmentRepository = (*listableTenantDeptRepo)(nil)

// ── PART A tests ──────────────────────────────────────────────────────────

// TestDepartmentService_ListForTenant_CatalogError covers the error branch
// inside the hydration loop in ListForTenant: tenantDepts.List returns two
// rows, and DepartmentByID returns an error for the first one. The service
// must propagate that error immediately.
func TestDepartmentService_ListForTenant_CatalogError(t *testing.T) {
	tenantID := uuid.New()
	deptID1, deptID2 := uuid.New(), uuid.New()

	tenantDepts := &listableTenantDeptRepo{
		listFn: func(_ context.Context, _ uuid.UUID) ([]domain.TenantDepartment, error) {
			return []domain.TenantDepartment{
				{TenantID: tenantID, DepartmentID: deptID1, IsActive: true},
				{TenantID: tenantID, DepartmentID: deptID2, IsActive: true},
			}, nil
		},
	}

	catalogErr := errors.New("catalog service unavailable")
	catalog := &fakeDeptCatalogRepo{
		findByIDFn: func(_ context.Context, _ uuid.UUID) (*domain.Department, error) {
			return nil, catalogErr
		},
	}

	svc := service.NewDepartmentService(catalog, tenantDepts, nil)
	got, err := svc.ListForTenant(context.Background(), tenantID)

	require.ErrorIs(t, err, catalogErr, "error from DepartmentByID must propagate out of ListForTenant")
	assert.Nil(t, got)
}

// TestDepartmentService_SetActive_RepoError covers the branch where
// tenantDepts.SetActive itself returns an error after the catalog checks pass.
func TestDepartmentService_SetActive_RepoError(t *testing.T) {
	repoErr := errors.New("optimistic_lock_conflict")

	catalog := &fakeDeptCatalogRepo{
		// Returns an active, non-system dept — both guard checks pass.
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Department, error) {
			return &domain.Department{ID: id, IsActive: true, IsSystem: false}, nil
		},
	}
	tenantDepts := &listableTenantDeptRepo{
		setActiveFn: func(_ context.Context, _, _ uuid.UUID, _ bool, _ int64) (*domain.TenantDepartment, error) {
			return nil, repoErr
		},
	}

	svc := service.NewDepartmentService(catalog, tenantDepts, nil)
	got, err := svc.SetActive(context.Background(), uuid.New(), uuid.New(), false, 1)

	require.ErrorIs(t, err, repoErr, "repo error from SetActive must be propagated")
	assert.Nil(t, got)
}

// TestDepartmentService_invalidateCache_NilCache verifies that Activate does
// not panic when the service was constructed with a nil cache (the
// invalidateCache method guards on cache==nil).
func TestDepartmentService_invalidateCache_NilCache(t *testing.T) {
	deptID := uuid.New()
	tenantDepts := &listableTenantDeptRepo{}
	catalog := &fakeDeptCatalogRepo{}

	// nil cache — third argument
	svc := service.NewDepartmentService(catalog, tenantDepts, nil)

	require.NotPanics(t, func() {
		_, _, err := svc.Activate(context.Background(), uuid.New(), deptID)
		require.NoError(t, err)
	})
}

// TestDepartmentService_ListForTenant_EmptyList confirms that when
// tenantDepts.List returns an empty slice, ListForTenant returns an empty
// (non-nil) TenantDepartmentView slice — not nil — satisfying callers that
// range over the result or rely on len()==0.
func TestDepartmentService_ListForTenant_EmptyList(t *testing.T) {
	tenantDepts := &listableTenantDeptRepo{
		listFn: func(_ context.Context, _ uuid.UUID) ([]domain.TenantDepartment, error) {
			return []domain.TenantDepartment{}, nil
		},
	}
	svc := service.NewDepartmentService(&fakeDeptCatalogRepo{}, tenantDepts, nil)

	got, err := svc.ListForTenant(context.Background(), uuid.New())

	require.NoError(t, err)
	assert.NotNil(t, got, "result must be a non-nil empty slice, not nil")
	assert.Empty(t, got)
}

// ── PART B: DeptMembershipService — WithLogger gap ────────────────────────

// fakeLogger is a minimal port.Logger implementation used only to satisfy the
// interface — no assertions on calls are needed for the WithLogger identity test.
type fakeLogger struct{}

func (l *fakeLogger) Debug(_ string, _ map[string]any) {}
func (l *fakeLogger) Info(_ string, _ map[string]any)  {}
func (l *fakeLogger) Warn(_ string, _ map[string]any)  {}
func (l *fakeLogger) Error(_ string, _ map[string]any) {}

var _ port.Logger = (*fakeLogger)(nil)

// TestDeptMembershipService_WithLogger_ReturnsSelf verifies that WithLogger
// returns the same *DeptMembershipService pointer so callers can chain it
// fluently (e.g. svc := service.NewDeptMembershipService(...).WithLogger(log)).
func TestDeptMembershipService_WithLogger_ReturnsSelf(t *testing.T) {
	svc := service.NewDeptMembershipService(
		&fakeDeptMemRepo{}, nil, &activeTenantDeptRepo{}, nil, nil, nil, nil, nil,
	)

	got := svc.WithLogger(&fakeLogger{})

	assert.Same(t, svc, got, "WithLogger must return the same *DeptMembershipService (builder pattern)")
}

// ── DepartmentService.SetActive: catalog DepartmentByID error ───────────────
//
// This covers line 88.16,90.3 in department_service.go:
// when DepartmentByID returns an error, SetActive must propagate it immediately.

// TestDepartmentService_SetActive_CatalogError covers the error branch where
// the catalog's DepartmentByID returns an error and SetActive propagates it
// without calling the repo.
func TestDepartmentService_SetActive_CatalogError(t *testing.T) {
	catalogErr := errors.New("catalog unavailable")

	catalog := &fakeDeptCatalogRepo{
		findByIDFn: func(_ context.Context, _ uuid.UUID) (*domain.Department, error) {
			return nil, catalogErr
		},
	}
	tenantDepts := &listableTenantDeptRepo{}

	svc := service.NewDepartmentService(catalog, tenantDepts, nil)
	got, err := svc.SetActive(context.Background(), uuid.New(), uuid.New(), false, 1)

	require.ErrorIs(t, err, catalogErr, "DepartmentByID error must propagate from SetActive")
	assert.Nil(t, got)
}
