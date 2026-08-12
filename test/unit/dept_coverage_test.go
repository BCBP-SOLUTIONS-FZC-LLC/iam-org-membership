// Unit tests for department and dept-membership service validation gaps:
//
//	P10-INVALID-LEVEL-01    (invalid role_level → 422)
//	P10-TARGET-NOT-FOUND-01 (user not in tenant → 404)
//	P10-SUSPENDED-ASSIGN-01 regression guard (BUG-P10-1)
//	P24-RETIRED-DEPT-01     (activate globally retired dept → 422)
//	P25-DEACTIVATE-SYSTEM   (deactivate system dept → 422)
//	P25-ACTIVATE-RETIRED    (re-activate globally retired via patch → 422)
package unit_test

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── fakeDeptCatalogRepo ────────────────────────────────────────────────

type fakeDeptCatalogRepo struct {
	findByIDFn func(ctx context.Context, id uuid.UUID) (*domain.Department, error)
}

func (f *fakeDeptCatalogRepo) List(_ context.Context, _ bool) ([]domain.Department, error) {
	return nil, nil
}
func (f *fakeDeptCatalogRepo) FindByID(ctx context.Context, id uuid.UUID) (*domain.Department, error) {
	if f.findByIDFn != nil {
		return f.findByIDFn(ctx, id)
	}
	return &domain.Department{ID: id, IsActive: true, IsSystem: false}, nil
}
func (f *fakeDeptCatalogRepo) FindByCode(_ context.Context, _ string) (*domain.Department, error) {
	return nil, nil
}
func (f *fakeDeptCatalogRepo) Insert(_ context.Context, d *domain.Department) (*domain.Department, error) {
	return d, nil
}
func (f *fakeDeptCatalogRepo) Update(_ context.Context, _ uuid.UUID, _ *string, _ *bool, _ int64) (*domain.Department, error) {
	return nil, nil
}

var _ port.DepartmentRepository = (*fakeDeptCatalogRepo)(nil)

// ── fakeTenantDeptRepoFull ─────────────────────────────────────────────

type fakeTenantDeptRepoFull struct {
	findFn      func(ctx context.Context, tenantID, deptID uuid.UUID) (*domain.TenantDepartment, error)
	activateFn  func(ctx context.Context, tenantID, deptID uuid.UUID) (*domain.TenantDepartment, error)
	setActiveFn func(ctx context.Context, tenantID, deptID uuid.UUID, active bool, version int64) (*domain.TenantDepartment, error)
}

func (f *fakeTenantDeptRepoFull) List(_ context.Context, _ uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (f *fakeTenantDeptRepoFull) ListActive(_ context.Context, _ uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (f *fakeTenantDeptRepoFull) Find(ctx context.Context, tid, did uuid.UUID) (*domain.TenantDepartment, error) {
	if f.findFn != nil {
		return f.findFn(ctx, tid, did)
	}
	return &domain.TenantDepartment{DepartmentID: did, TenantID: tid, IsActive: true}, nil
}
func (f *fakeTenantDeptRepoFull) Activate(ctx context.Context, tid, did uuid.UUID) (*domain.TenantDepartment, error) {
	if f.activateFn != nil {
		return f.activateFn(ctx, tid, did)
	}
	return &domain.TenantDepartment{DepartmentID: did, TenantID: tid, IsActive: true}, nil
}
func (f *fakeTenantDeptRepoFull) SetActive(ctx context.Context, tid, did uuid.UUID, active bool, ver int64) (*domain.TenantDepartment, error) {
	if f.setActiveFn != nil {
		return f.setActiveFn(ctx, tid, did, active, ver)
	}
	return &domain.TenantDepartment{DepartmentID: did, TenantID: tid, IsActive: active}, nil
}

var _ port.TenantDepartmentRepository = (*fakeTenantDeptRepoFull)(nil)

func buildDeptSvcFull(catalog *fakeDeptCatalogRepo, td *fakeTenantDeptRepoFull) *service.DepartmentService {
	return service.NewDepartmentService(catalog, td, nil)
}

// ── P24-RETIRED-DEPT-01 ────────────────────────────────────────────────

// Test Case ID:      P24-RETIRED-DEPT-01
// Feature:           P-24 · activate globally retired dept → 422 department_retired
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestDeptService_Activate_RetiredCatalogDept_Returns422(t *testing.T) {
	catalog := &fakeDeptCatalogRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Department, error) {
			return &domain.Department{ID: id, IsActive: false}, nil
		},
	}
	svc := buildDeptSvcFull(catalog, &fakeTenantDeptRepoFull{})
	_, _, err := svc.Activate(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrDepartmentRetired)
}

// ── P25-DEACTIVATE-SYSTEM-01 ───────────────────────────────────────────

// Test Case ID:      P25-DEACTIVATE-SYSTEM-01
// Feature:           P-25 · deactivate system department → 422 (D-9/D-11)
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestDeptService_SetActive_SystemDept_DeactivateBlocked(t *testing.T) {
	catalog := &fakeDeptCatalogRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Department, error) {
			return &domain.Department{ID: id, IsActive: true, IsSystem: true}, nil
		},
	}
	svc := buildDeptSvcFull(catalog, &fakeTenantDeptRepoFull{})
	_, err := svc.SetActive(context.Background(), uuid.New(), uuid.New(), false, 1)
	assert.ErrorIs(t, err, domain.ErrSystemDepartmentCannotBeRetired)
}

// ── P25-ACTIVATE-RETIRED-01 ────────────────────────────────────────────

// Test Case ID:      P25-ACTIVATE-RETIRED-01
// Feature:           P-25 · is_active=true on globally retired catalog dept → 422
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestDeptService_SetActive_RetiredCatalogDept_ActivateBlocked(t *testing.T) {
	catalog := &fakeDeptCatalogRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Department, error) {
			return &domain.Department{ID: id, IsActive: false, IsSystem: false}, nil
		},
	}
	svc := buildDeptSvcFull(catalog, &fakeTenantDeptRepoFull{})
	_, err := svc.SetActive(context.Background(), uuid.New(), uuid.New(), true, 1)
	assert.ErrorIs(t, err, domain.ErrDepartmentRetired)
}

// ── P10-INVALID-LEVEL-01 ───────────────────────────────────────────────

// Test Case ID:      P10-INVALID-LEVEL-01
// Feature:           P-10 · role_level='wizard' → 422 invalid_role_level
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestDeptMembershipAssign_InvalidLevel_Rejected(t *testing.T) {
	svc := service.NewDeptMembershipService(
		&fakeDeptMemRepo{}, nil, nil, nil, nil, nil, nil, nil)
	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(),
		domain.DeptRole("wizard"), uuid.New())

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
	assert.Equal(t, "invalid_role_level", de.Details["code"])
}

// ── P10-TARGET-NOT-FOUND-01 ────────────────────────────────────────────

// Test Case ID:      P10-TARGET-NOT-FOUND-01
// Feature:           P-10 · target user not in tenant → 404 member_not_found
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestDeptMembershipAssign_TargetNotFound(t *testing.T) {
	mem := &fakeMembershipRepo{
		findByUserIDFn: func(_ context.Context, _, _ uuid.UUID) (*domain.TenantMembership, error) {
			return nil, domain.NewError(domain.ErrMemberNotFound, "member not found")
		},
	}
	catalog := &fakeDeptCatalogRepo{}
	td := &fakeTenantDeptRepoFull{}
	svc := service.NewDeptMembershipService(
		&fakeDeptMemRepo{}, mem, td, catalog, nil, nil, nil, nil)
	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(),
		domain.DeptPreparator, uuid.New())
	assert.ErrorIs(t, err, domain.ErrMemberNotFound)
}

// ── P10-SUSPENDED-ASSIGN-01 (BUG-P10-1 regression guard) ──────────────

// Test Case ID:      P10-SUSPENDED-ASSIGN-01
// Feature:           P-10 · assign suspended member → 422 member_not_active (DM-2 / BUG-P10-1)
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestDeptMembershipAssign_SuspendedMember_Rejected(t *testing.T) {
	mem := &fakeMembershipRepo{
		findByUserIDFn: func(_ context.Context, _, _ uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{
				ID:     uuid.New(),
				Status: domain.MembershipSuspended,
			}, nil
		},
	}
	catalog := &fakeDeptCatalogRepo{}
	td := &fakeTenantDeptRepoFull{}
	svc := service.NewDeptMembershipService(
		&fakeDeptMemRepo{}, mem, td, catalog, nil, nil, nil, nil)
	_, err := svc.Assign(context.Background(), uuid.New(), uuid.New(), uuid.New(),
		domain.DeptPreparator, uuid.New())
	assert.ErrorIs(t, err, domain.ErrMemberNotActive)
}
