// Unit tests for internal/core/service/operator_service.go, covering the
// non-pool-touching methods:
//   - CreateDepartment (O-1)
//   - PatchDepartment (O-2)
//   - ListPlans        (O-5)
//   - PatchPlan        (O-6)
//
// SetFeatureFlags (O-4) uses pgcommon.RunInTx directly on the shared pool
// and is exercised in test/postgres. ReassignOwner (O-7) needs TxRunner
// wiring and is already 69% covered from postgres integration tests.
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

// ── DepartmentRepository stub ──────────────────────────────────────────

type fakeDeptRepo struct {
	insertFn func(ctx context.Context, d *domain.Department) (*domain.Department, error)
	updateFn func(ctx context.Context, id uuid.UUID, name *string, isActive *bool, expectedVersion int64) (*domain.Department, error)
}

func (f *fakeDeptRepo) List(context.Context, bool) ([]domain.Department, error) {
	return nil, errors.New("not used")
}
func (f *fakeDeptRepo) FindByID(context.Context, uuid.UUID) (*domain.Department, error) {
	return nil, errors.New("not used")
}
func (f *fakeDeptRepo) FindByCode(context.Context, string) (*domain.Department, error) {
	return nil, errors.New("not used")
}
func (f *fakeDeptRepo) Insert(ctx context.Context, d *domain.Department) (*domain.Department, error) {
	return f.insertFn(ctx, d)
}
func (f *fakeDeptRepo) Update(ctx context.Context, id uuid.UUID, name *string, isActive *bool, expectedVersion int64) (*domain.Department, error) {
	return f.updateFn(ctx, id, name, isActive, expectedVersion)
}

var _ port.DepartmentRepository = (*fakeDeptRepo)(nil)

// ── PlanRepository stub ────────────────────────────────────────────────

type fakePlanRepo struct {
	listFn   func(ctx context.Context) ([]domain.Plan, error)
	updateFn func(ctx context.Context, code domain.TenantPlan, patch *domain.PlanPatch) (*domain.Plan, error)
}

func (f *fakePlanRepo) List(ctx context.Context) ([]domain.Plan, error) {
	return f.listFn(ctx)
}
func (f *fakePlanRepo) FindByCode(context.Context, domain.TenantPlan) (*domain.Plan, error) {
	return nil, errors.New("not used")
}
func (f *fakePlanRepo) Update(ctx context.Context, code domain.TenantPlan, patch *domain.PlanPatch) (*domain.Plan, error) {
	return f.updateFn(ctx, code, patch)
}

var _ port.PlanRepository = (*fakePlanRepo)(nil)

// buildOperator wires only the collaborators used by the target methods;
// pool/tenants/tenRoles/memBs/txRunner stay nil.
func buildOperator(plans port.PlanRepository, depts port.DepartmentRepository, cache port.Cache) *service.OperatorService {
	return service.NewOperatorService(nil, plans, depts, nil, nil, nil, cache, nil)
}

// ── CreateDepartment (O-1) ─────────────────────────────────────────────

func TestOperator_CreateDepartment_RejectsEmptyCode(t *testing.T) {
	svc := buildOperator(nil, &fakeDeptRepo{}, nil)
	_, err := svc.CreateDepartment(context.Background(), "", "Engineering", false)

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
}

func TestOperator_CreateDepartment_RejectsEmptyName(t *testing.T) {
	svc := buildOperator(nil, &fakeDeptRepo{}, nil)
	_, err := svc.CreateDepartment(context.Background(), "ENG", "", false)

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
}

func TestOperator_CreateDepartment_InsertsWithGivenFieldsAndActive(t *testing.T) {
	captured := &domain.Department{}
	repo := &fakeDeptRepo{
		insertFn: func(_ context.Context, d *domain.Department) (*domain.Department, error) {
			*captured = *d
			d.ID = uuid.New()
			return d, nil
		},
	}
	svc := buildOperator(nil, repo, nil)

	got, err := svc.CreateDepartment(context.Background(), "ENG", "Engineering", true)
	require.NoError(t, err)
	assert.Equal(t, "ENG", captured.Code)
	assert.Equal(t, "Engineering", captured.Name)
	assert.True(t, captured.IsSystem)
	assert.True(t, captured.IsActive, "new departments are always created active")
	assert.NotEqual(t, uuid.Nil, got.ID)
}

func TestOperator_CreateDepartment_NonSystemDefault(t *testing.T) {
	captured := &domain.Department{}
	repo := &fakeDeptRepo{
		insertFn: func(_ context.Context, d *domain.Department) (*domain.Department, error) {
			*captured = *d
			return d, nil
		},
	}
	svc := buildOperator(nil, repo, nil)

	_, err := svc.CreateDepartment(context.Background(), "HR", "Human Resources", false)
	require.NoError(t, err)
	assert.False(t, captured.IsSystem)
}

func TestOperator_CreateDepartment_RepoErrorPropagates(t *testing.T) {
	repoErr := errors.New("duplicate code")
	repo := &fakeDeptRepo{
		insertFn: func(context.Context, *domain.Department) (*domain.Department, error) {
			return nil, repoErr
		},
	}
	svc := buildOperator(nil, repo, nil)

	_, err := svc.CreateDepartment(context.Background(), "ENG", "Engineering", false)
	assert.ErrorIs(t, err, repoErr)
}

// ── PatchDepartment (O-2) ──────────────────────────────────────────────

func TestOperator_PatchDepartment_RejectsEmptyPatch(t *testing.T) {
	svc := buildOperator(nil, &fakeDeptRepo{}, nil)
	_, err := svc.PatchDepartment(context.Background(), uuid.New(), nil, nil, 1)

	assert.ErrorIs(t, err, domain.ErrNoMutableField)
}

func TestOperator_PatchDepartment_PassesNameThrough(t *testing.T) {
	name := "New Name"
	repo := &fakeDeptRepo{
		updateFn: func(_ context.Context, _ uuid.UUID, n *string, ia *bool, ver int64) (*domain.Department, error) {
			require.NotNil(t, n)
			assert.Equal(t, "New Name", *n)
			assert.Nil(t, ia, "is_active untouched when caller only patches name")
			assert.EqualValues(t, 7, ver)
			return &domain.Department{ID: uuid.New(), Name: *n}, nil
		},
	}
	svc := buildOperator(nil, repo, nil)

	got, err := svc.PatchDepartment(context.Background(), uuid.New(), &name, nil, 7)
	require.NoError(t, err)
	assert.Equal(t, "New Name", got.Name)
}

func TestOperator_PatchDepartment_PassesIsActiveThrough(t *testing.T) {
	inactive := false
	repo := &fakeDeptRepo{
		updateFn: func(_ context.Context, _ uuid.UUID, n *string, ia *bool, _ int64) (*domain.Department, error) {
			assert.Nil(t, n)
			require.NotNil(t, ia)
			assert.False(t, *ia)
			return &domain.Department{ID: uuid.New(), IsActive: *ia}, nil
		},
	}
	svc := buildOperator(nil, repo, nil)

	got, err := svc.PatchDepartment(context.Background(), uuid.New(), nil, &inactive, 1)
	require.NoError(t, err)
	assert.False(t, got.IsActive)
}

func TestOperator_PatchDepartment_MapsSystemRetireCheckViolation(t *testing.T) {
	// D-11: retiring a system dept violates chk_system_department_active
	// at the DB layer. The service must convert that raw PG error into
	// a clean 422 ErrSystemDepartmentCannotBeRetired.
	inactive := false
	repo := &fakeDeptRepo{
		updateFn: func(context.Context, uuid.UUID, *string, *bool, int64) (*domain.Department, error) {
			return nil, errors.New("ERROR: violates check constraint \"chk_system_department_active\"")
		},
	}
	svc := buildOperator(nil, repo, nil)

	_, err := svc.PatchDepartment(context.Background(), uuid.New(), nil, &inactive, 1)
	assert.ErrorIs(t, err, domain.ErrSystemDepartmentCannotBeRetired)
}

func TestOperator_PatchDepartment_PassesThroughOtherRepoErrors(t *testing.T) {
	repoErr := errors.New("optimistic lock conflict")
	name := "X"
	repo := &fakeDeptRepo{
		updateFn: func(context.Context, uuid.UUID, *string, *bool, int64) (*domain.Department, error) {
			return nil, repoErr
		},
	}
	svc := buildOperator(nil, repo, nil)

	_, err := svc.PatchDepartment(context.Background(), uuid.New(), &name, nil, 1)
	assert.ErrorIs(t, err, repoErr)
}

// ── ListPlans (O-5) ────────────────────────────────────────────────────

func TestOperator_ListPlans_DelegatesToRepo(t *testing.T) {
	want := []domain.Plan{{Code: domain.TenantPlan("free")}, {Code: domain.TenantPlan("pro")}}
	repo := &fakePlanRepo{
		listFn: func(context.Context) ([]domain.Plan, error) { return want, nil },
	}
	svc := buildOperator(repo, nil, nil)

	got, err := svc.ListPlans(context.Background())
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestOperator_ListPlans_PropagatesRepoError(t *testing.T) {
	repoErr := errors.New("plans unavailable")
	repo := &fakePlanRepo{
		listFn: func(context.Context) ([]domain.Plan, error) { return nil, repoErr },
	}
	svc := buildOperator(repo, nil, nil)

	_, err := svc.ListPlans(context.Background())
	assert.ErrorIs(t, err, repoErr)
}

// ── PatchPlan (O-6) ────────────────────────────────────────────────────

func TestOperator_PatchPlan_DelegatesAndInvalidatesCache(t *testing.T) {
	code := domain.TenantPlan("pro")
	patch := &domain.PlanPatch{}
	repo := &fakePlanRepo{
		updateFn: func(_ context.Context, c domain.TenantPlan, p *domain.PlanPatch) (*domain.Plan, error) {
			assert.Equal(t, code, c)
			assert.Equal(t, patch, p)
			return &domain.Plan{Code: c}, nil
		},
	}
	cache := &spyCache{}
	svc := buildOperator(repo, nil, cache)

	got, err := svc.PatchPlan(context.Background(), code, patch)
	require.NoError(t, err)
	assert.Equal(t, code, got.Code)
	assert.Contains(t, cache.deleteCalls, "om:plans",
		"successful PatchPlan must invalidate the plans catalog cache")
}

func TestOperator_PatchPlan_RepoErrorSkipsCacheInvalidation(t *testing.T) {
	repoErr := errors.New("no such plan")
	repo := &fakePlanRepo{
		updateFn: func(context.Context, domain.TenantPlan, *domain.PlanPatch) (*domain.Plan, error) {
			return nil, repoErr
		},
	}
	cache := &spyCache{}
	svc := buildOperator(repo, nil, cache)

	_, err := svc.PatchPlan(context.Background(), domain.TenantPlan("pro"), &domain.PlanPatch{})
	assert.ErrorIs(t, err, repoErr)
	assert.Empty(t, cache.deleteCalls, "no cache invalidation when repo write failed")
}

func TestOperator_PatchPlan_NilCacheIsSafe(t *testing.T) {
	repo := &fakePlanRepo{
		updateFn: func(_ context.Context, c domain.TenantPlan, _ *domain.PlanPatch) (*domain.Plan, error) {
			return &domain.Plan{Code: c}, nil
		},
	}
	svc := buildOperator(repo, nil, nil)

	got, err := svc.PatchPlan(context.Background(), domain.TenantPlan("free"), &domain.PlanPatch{})
	require.NoError(t, err)
	assert.NotNil(t, got)
}
