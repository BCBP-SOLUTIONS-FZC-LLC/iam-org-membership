package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
)

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
