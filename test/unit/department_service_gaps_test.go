// Unit tests closing remaining branch gaps in
// internal/core/service/department_service.go: ListForTenant (repo error,
// empty list, per-item catalog hydration error, happy path), Activate
// (already-activated idempotent 200 branch: Find success + Find error,
// generic repo error, success + cache invalidation), SetActive (generic
// repo error, success + cache invalidation), and invalidateCache (nil-cache
// no-op vs live-cache Delete).
package unit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── ListForTenant ────────────────────────────────────────────────────────

func TestDeptService_ListForTenant_RepoErrorPropagates(t *testing.T) {
	listErr := errors.New("db down")
	td := &fakeTenantDeptRepoFull{}
	svc := service.NewDepartmentService(&fakeDeptCatalogRepo{}, &errListTenantDeptRepo{listErr: listErr, inner: td}, nil)
	_, err := svc.ListForTenant(context.Background(), uuid.New())
	assert.ErrorIs(t, err, listErr)
}

// errListTenantDeptRepo wraps fakeTenantDeptRepoFull, overriding only List
// with a configurable error/result.
type errListTenantDeptRepo struct {
	inner   *fakeTenantDeptRepoFull
	listErr error
	listOut []domain.TenantDepartment
}

func (r *errListTenantDeptRepo) List(context.Context, uuid.UUID) ([]domain.TenantDepartment, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.listOut, nil
}
func (r *errListTenantDeptRepo) ListActive(ctx context.Context, tenantID uuid.UUID) ([]domain.TenantDepartment, error) {
	return r.inner.ListActive(ctx, tenantID)
}
func (r *errListTenantDeptRepo) Find(ctx context.Context, tid, did uuid.UUID) (*domain.TenantDepartment, error) {
	return r.inner.Find(ctx, tid, did)
}
func (r *errListTenantDeptRepo) Activate(ctx context.Context, tid, did uuid.UUID) (*domain.TenantDepartment, error) {
	return r.inner.Activate(ctx, tid, did)
}
func (r *errListTenantDeptRepo) SetActive(ctx context.Context, tid, did uuid.UUID, active bool, ver int64) (*domain.TenantDepartment, error) {
	return r.inner.SetActive(ctx, tid, did, active, ver)
}

func TestDeptService_ListForTenant_EmptyList_ReturnsEmptySlice(t *testing.T) {
	td := &errListTenantDeptRepo{inner: &fakeTenantDeptRepoFull{}, listOut: nil}
	svc := service.NewDepartmentService(&fakeDeptCatalogRepo{}, td, nil)
	got, err := svc.ListForTenant(context.Background(), uuid.New())
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

func TestDeptService_ListForTenant_CatalogHydrationErrorPropagates(t *testing.T) {
	deptID := uuid.New()
	td := &errListTenantDeptRepo{
		inner:   &fakeTenantDeptRepoFull{},
		listOut: []domain.TenantDepartment{{DepartmentID: deptID}},
	}
	catalogErr := errors.New("catalog unavailable")
	catalog := &fakeDeptCatalogRepo{findByIDFn: func(context.Context, uuid.UUID) (*domain.Department, error) {
		return nil, catalogErr
	}}
	svc := service.NewDepartmentService(catalog, td, nil)
	_, err := svc.ListForTenant(context.Background(), uuid.New())
	assert.ErrorIs(t, err, catalogErr)
}

func TestDeptService_ListForTenant_HappyPath_HydratesEachRow(t *testing.T) {
	deptID := uuid.New()
	td := &errListTenantDeptRepo{
		inner: &fakeTenantDeptRepoFull{},
		listOut: []domain.TenantDepartment{
			{DepartmentID: deptID, IsActive: false, RecordVersion: 3},
		},
	}
	catalog := &fakeDeptCatalogRepo{findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Department, error) {
		return &domain.Department{ID: id, Code: "ENG", Name: "Engineering", IsSystem: true, IsActive: true}, nil
	}}
	svc := service.NewDepartmentService(catalog, td, nil)
	got, err := svc.ListForTenant(context.Background(), uuid.New())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, deptID, got[0].DepartmentID)
	assert.Equal(t, "ENG", got[0].Code)
	assert.Equal(t, "Engineering", got[0].Name)
	assert.True(t, got[0].IsSystem)
	assert.False(t, got[0].IsActive, "TenantDepartment.IsActive must win over the catalog row's IsActive")
	assert.EqualValues(t, 3, got[0].RecordVersion)
}

// ── Activate ────────────────────────────────────────────────────────────

func TestDeptService_Activate_CatalogLookupErrorPropagates(t *testing.T) {
	catalogErr := errors.New("catalog unavailable")
	catalog := &fakeDeptCatalogRepo{findByIDFn: func(context.Context, uuid.UUID) (*domain.Department, error) {
		return nil, catalogErr
	}}
	svc := buildDeptSvcFull(catalog, &fakeTenantDeptRepoFull{})
	_, _, err := svc.Activate(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, catalogErr)
}

func TestDeptService_Activate_RepoErrorPropagates(t *testing.T) {
	activateErr := errors.New("fk violation")
	td := &fakeTenantDeptRepoFull{activateFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
		return nil, activateErr
	}}
	svc := buildDeptSvcFull(&fakeDeptCatalogRepo{}, td)
	_, _, err := svc.Activate(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, activateErr)
}

func TestDeptService_Activate_AlreadyActivated_ReturnsExistingRowNotCreated(t *testing.T) {
	tenantID, deptID := uuid.New(), uuid.New()
	td := &fakeTenantDeptRepoFull{
		activateFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
			return nil, domain.ErrDepartmentAlreadyActivated
		},
		findFn: func(_ context.Context, tid, did uuid.UUID) (*domain.TenantDepartment, error) {
			return &domain.TenantDepartment{TenantID: tid, DepartmentID: did, IsActive: true, RecordVersion: 7}, nil
		},
	}
	svc := buildDeptSvcFull(&fakeDeptCatalogRepo{}, td)
	got, wasCreated, err := svc.Activate(context.Background(), tenantID, deptID)
	require.NoError(t, err)
	assert.False(t, wasCreated, "P-24 idempotent replay must report wasCreated=false")
	require.NotNil(t, got)
	assert.EqualValues(t, 7, got.RecordVersion)
}

func TestDeptService_Activate_AlreadyActivated_FindErrorPropagates(t *testing.T) {
	findErr := errors.New("db down")
	td := &fakeTenantDeptRepoFull{
		activateFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
			return nil, domain.ErrDepartmentAlreadyActivated
		},
		findFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
			return nil, findErr
		},
	}
	svc := buildDeptSvcFull(&fakeDeptCatalogRepo{}, td)
	_, wasCreated, err := svc.Activate(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, findErr)
	assert.False(t, wasCreated)
}

func TestDeptService_Activate_Success_InvalidatesCache(t *testing.T) {
	tenantID := uuid.New()
	cache := &spyCache{}
	svc := service.NewDepartmentService(&fakeDeptCatalogRepo{}, &fakeTenantDeptRepoFull{}, cache)
	got, wasCreated, err := svc.Activate(context.Background(), tenantID, uuid.New())
	require.NoError(t, err)
	assert.True(t, wasCreated)
	require.NotNil(t, got)
	assert.Contains(t, cache.deleteCalls, "om:tenant:"+tenantID.String())
}

// ── SetActive ───────────────────────────────────────────────────────────

func TestDeptService_SetActive_CatalogLookupErrorPropagates(t *testing.T) {
	catalogErr := errors.New("catalog unavailable")
	catalog := &fakeDeptCatalogRepo{findByIDFn: func(context.Context, uuid.UUID) (*domain.Department, error) {
		return nil, catalogErr
	}}
	svc := buildDeptSvcFull(catalog, &fakeTenantDeptRepoFull{})
	_, err := svc.SetActive(context.Background(), uuid.New(), uuid.New(), true, 1)
	assert.ErrorIs(t, err, catalogErr)
}

func TestDeptService_SetActive_RepoErrorPropagates(t *testing.T) {
	setErr := errors.New("optimistic_lock_conflict")
	td := &fakeTenantDeptRepoFull{setActiveFn: func(context.Context, uuid.UUID, uuid.UUID, bool, int64) (*domain.TenantDepartment, error) {
		return nil, setErr
	}}
	svc := buildDeptSvcFull(&fakeDeptCatalogRepo{}, td)
	_, err := svc.SetActive(context.Background(), uuid.New(), uuid.New(), false, 1)
	assert.ErrorIs(t, err, setErr)
}

func TestDeptService_SetActive_Success_InvalidatesCache(t *testing.T) {
	tenantID := uuid.New()
	cache := &spyCache{}
	svc := service.NewDepartmentService(&fakeDeptCatalogRepo{}, &fakeTenantDeptRepoFull{}, cache)
	got, err := svc.SetActive(context.Background(), tenantID, uuid.New(), true, 1)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Contains(t, cache.deleteCalls, "om:tenant:"+tenantID.String())
}

// ── invalidateCache: nil cache is a safe no-op ─────────────────────────

func TestDeptService_Activate_NilCache_IsSafe(t *testing.T) {
	svc := buildDeptSvcFull(&fakeDeptCatalogRepo{}, &fakeTenantDeptRepoFull{})
	assert.NotPanics(t, func() {
		_, _, err := svc.Activate(context.Background(), uuid.New(), uuid.New())
		require.NoError(t, err)
	})
}
