// Extended tests for P-24/P-25 dept service and P-10 dept assign scenarios.
package unit_test

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── P24 Activate ──────────────────────────────────────────────────────

// Test Case ID:      P24-HAPPY-01
// Feature:           P-24 · activate department → 200 TenantDepartmentView
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestDeptService_Activate_HappyPath(t *testing.T) {
	deptID := uuid.New()
	td := &fakeTenantDeptRepoFull{
		activateFn: func(_ context.Context, _, _ uuid.UUID) (*domain.TenantDepartment, error) {
			return &domain.TenantDepartment{DepartmentID: deptID, IsActive: true}, nil
		},
	}
	svc := buildDeptSvcFull(&fakeDeptCatalogRepo{}, td)
	got, _, err := svc.Activate(context.Background(), uuid.New(), deptID)
	require.NoError(t, err)
	assert.True(t, got.IsActive)
}

// Test Case ID:      P24-IDEMPOTENT-01
// Feature:           P-24 · activate already-active dept → idempotent 200
// Priority: P2 · Severity: Minor · Automation Status: Automated
func TestDeptService_Activate_AlreadyActive_Idempotent(t *testing.T) {
	deptID := uuid.New()
	existing := &domain.TenantDepartment{DepartmentID: deptID, IsActive: true}
	td := &fakeTenantDeptRepoFull{
		activateFn: func(_ context.Context, _, _ uuid.UUID) (*domain.TenantDepartment, error) {
			return existing, nil // idempotent — returns same row
		},
	}
	svc := buildDeptSvcFull(&fakeDeptCatalogRepo{}, td)
	got, _, err := svc.Activate(context.Background(), uuid.New(), deptID)
	require.NoError(t, err)
	assert.True(t, got.IsActive)
}

// Test Case ID:      P24-NOT-FOUND-01
// Feature:           P-24 · non-existent department → 404 department_not_found
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestDeptService_Activate_DeptNotFound(t *testing.T) {
	catalog := &fakeDeptCatalogRepo{
		findByIDFn: func(_ context.Context, _ uuid.UUID) (*domain.Department, error) {
			return nil, domain.NewError(domain.ErrDepartmentNotFound, "dept not found")
		},
	}
	svc := buildDeptSvcFull(catalog, &fakeTenantDeptRepoFull{})
	_, _, err := svc.Activate(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrDepartmentNotFound)
}

// ── P25 SetActive ─────────────────────────────────────────────────────

// Test Case ID:      P25-HAPPY-01
// Feature:           P-25 · deactivate non-system dept → 200
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestDeptService_SetActive_Deactivate_HappyPath(t *testing.T) {
	deptID := uuid.New()
	catalog := &fakeDeptCatalogRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Department, error) {
			return &domain.Department{ID: id, IsActive: true, IsSystem: false}, nil
		},
	}
	td := &fakeTenantDeptRepoFull{
		setActiveFn: func(_ context.Context, _, _ uuid.UUID, active bool, _ int64) (*domain.TenantDepartment, error) {
			return &domain.TenantDepartment{DepartmentID: deptID, IsActive: active}, nil
		},
	}
	svc := buildDeptSvcFull(catalog, td)
	got, err := svc.SetActive(context.Background(), uuid.New(), deptID, false, 1)
	require.NoError(t, err)
	assert.False(t, got.IsActive)
}

// Test Case ID:      P25-CONC-01
// Feature:           P-25 · wrong record_version → 409 optimistic_lock_conflict
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestDeptService_SetActive_WrongVersion_Returns409(t *testing.T) {
	catalog := &fakeDeptCatalogRepo{}
	td := &fakeTenantDeptRepoFull{
		setActiveFn: func(_ context.Context, _, _ uuid.UUID, _ bool, _ int64) (*domain.TenantDepartment, error) {
			return nil, domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict")
		},
	}
	svc := buildDeptSvcFull(catalog, td)
	_, err := svc.SetActive(context.Background(), uuid.New(), uuid.New(), false, 99)
	assert.ErrorIs(t, err, domain.ErrOptimisticLockConflict)
}

// ── P10 Assign ────────────────────────────────────────────────────────

// Test Case ID:      P10-DEACTIVATED-DEPT-01
// Feature:           P-10 · assign to deactivated dept → 422 department_deactivated
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestDeptMembershipAssign_DeactivatedDept_Returns422(t *testing.T) {
	mem := &fakeMembershipRepo{
		findByUserIDFn: func(_ context.Context, _, _ uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), Status: domain.MembershipActive}, nil
		},
	}
	catalog := &fakeDeptCatalogRepo{}
	td := &fakeTenantDeptRepoFull{
		findFn: func(_ context.Context, _, _ uuid.UUID) (*domain.TenantDepartment, error) {
			return &domain.TenantDepartment{IsActive: false}, nil // deactivated
		},
	}
	svc := service.NewDeptMembershipService(
		&fakeDeptMemRepo{}, mem, td, catalog, nil, nil, nil, nil)
	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(),
		domain.DeptPreparator, uuid.New())
	assert.ErrorIs(t, err, domain.ErrDepartmentDeactivated)
}

// Test Case ID:      P10-HAPPY-01 / P10-LEVEL-CHANGE-01
// Feature:           P-10 · happy path Assign + level change need postgres integration test.
//                    Inside RunInTx the repo's ListByUser + Assign are called.
//                    fakeDeptMemRepo.ListByUser returns "not used" error — test deferred.
// Coverage:          postgres integration suite (test/postgres).
