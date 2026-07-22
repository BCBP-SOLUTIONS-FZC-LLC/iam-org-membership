package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
)

// DepartmentRepository owns the global departments catalog. Operator-only
// writes via O-1/O-2 (AUTH-6). No RLS on this table (global reference).
type DepartmentRepository interface {
	List(ctx context.Context, activeOnly bool) ([]domain.Department, error)
	FindByID(ctx context.Context, id uuid.UUID) (*domain.Department, error)
	FindByCode(ctx context.Context, code string) (*domain.Department, error)
	Insert(ctx context.Context, d *domain.Department) (*domain.Department, error)
	Update(ctx context.Context, id uuid.UUID, name *string, isActive *bool, expectedVersion int64) (*domain.Department, error)
}

// TenantDepartmentRepository owns per-tenant activation of catalog depts.
// RLS-scoped by tenant. P-3 lists active depts for the caller's tenant.
// P-24 activates + P-25 toggles is_active.
type TenantDepartmentRepository interface {
	List(ctx context.Context, tenantID uuid.UUID) ([]domain.TenantDepartment, error)
	ListActive(ctx context.Context, tenantID uuid.UUID) ([]domain.TenantDepartment, error)
	Find(ctx context.Context, tenantID, departmentID uuid.UUID) (*domain.TenantDepartment, error)
	Activate(ctx context.Context, tenantID, departmentID uuid.UUID) (*domain.TenantDepartment, error)
	SetActive(ctx context.Context, tenantID, departmentID uuid.UUID, isActive bool, expectedVersion int64) (*domain.TenantDepartment, error)
}
