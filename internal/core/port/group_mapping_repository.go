package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
)

// GroupMappingRepository owns the three group-mapping tables (§4.2, §16 A25).
// P-15/P-17/P-29 are full-replacement operations: the service builds the
// desired-state slice and the repo diffs against current state, inserting +
// deleting to converge.
type GroupMappingRepository interface {
	ListDeptRoleMappings(ctx context.Context, tenantID uuid.UUID) ([]domain.GroupDeptRoleMapping, error)
	ReplaceDeptRoleMappings(ctx context.Context, tenantID uuid.UUID, desired []domain.GroupDeptRoleMapping) ([]domain.GroupDeptRoleMapping, error)

	ListTenantRoleMappings(ctx context.Context, tenantID uuid.UUID) ([]domain.GroupTenantRoleMapping, error)
	ReplaceTenantRoleMappings(ctx context.Context, tenantID uuid.UUID, desired []domain.GroupTenantRoleMapping) ([]domain.GroupTenantRoleMapping, error)

	ListDeptMappings(ctx context.Context, tenantID uuid.UUID) ([]domain.GroupDeptMapping, error)
	ReplaceDeptMappings(ctx context.Context, tenantID uuid.UUID, desired []domain.GroupDeptMapping) ([]domain.GroupDeptMapping, error)
}
