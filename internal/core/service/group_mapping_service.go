package service

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

// GroupMappingService owns P-14/P-15/P-16/P-17/P-29.
type GroupMappingService struct {
	repo  port.GroupMappingRepository
	cache port.Cache
}

func NewGroupMappingService(repo port.GroupMappingRepository, cache port.Cache) *GroupMappingService {
	return &GroupMappingService{repo: repo, cache: cache}
}

// ── Dept-role mappings (P-14/P-15) ─────────────────────────────────────

func (s *GroupMappingService) ListDeptRole(ctx context.Context, tenantID uuid.UUID) ([]domain.GroupDeptRoleMapping, error) {
	return s.repo.ListDeptRoleMappings(ctx, tenantID)
}

func (s *GroupMappingService) ReplaceDeptRole(ctx context.Context, tenantID uuid.UUID, mappings []domain.GroupDeptRoleMapping) ([]domain.GroupDeptRoleMapping, error) {
	for _, m := range mappings {
		if m.KeycloakGroupName == "" {
			return nil, domain.NewError(domain.ErrValidation, "keycloak_group_name is required")
		}
		if m.RoleCode != domain.DeptPreparator && m.RoleCode != domain.DeptReviewer && m.RoleCode != domain.DeptApprover {
			return nil, domain.NewError(domain.ErrValidation, "invalid role_code for dept-role mapping").
				WithDetails(map[string]any{"code": "invalid_role_level"})
		}
	}
	out, err := s.repo.ReplaceDeptRoleMappings(ctx, tenantID, mappings)
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx, tenantID)
	return out, nil
}

// ── Tenant-role mappings (P-29) ────────────────────────────────────────

func (s *GroupMappingService) ListTenantRole(ctx context.Context, tenantID uuid.UUID) ([]domain.GroupTenantRoleMapping, error) {
	return s.repo.ListTenantRoleMappings(ctx, tenantID)
}

func (s *GroupMappingService) ReplaceTenantRole(ctx context.Context, tenantID uuid.UUID, mappings []domain.GroupTenantRoleMapping) ([]domain.GroupTenantRoleMapping, error) {
	for _, m := range mappings {
		if m.KeycloakGroupName == "" {
			return nil, domain.NewError(domain.ErrValidation, "keycloak_group_name is required")
		}
		if !m.RoleCode.IsElevated() {
			// GTRM-6: chk_gtrm_no_member barred 'member' at DB level too.
			return nil, domain.NewError(domain.ErrValidation, "role_code must be tenant_owner/tenant_admin/tender_admin").
				WithDetails(map[string]any{"code": "invalid_role"})
		}
	}
	out, err := s.repo.ReplaceTenantRoleMappings(ctx, tenantID, mappings)
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx, tenantID)
	return out, nil
}

// ── Dept mappings (P-16/P-17) ──────────────────────────────────────────

func (s *GroupMappingService) ListDept(ctx context.Context, tenantID uuid.UUID) ([]domain.GroupDeptMapping, error) {
	return s.repo.ListDeptMappings(ctx, tenantID)
}

func (s *GroupMappingService) ReplaceDept(ctx context.Context, tenantID uuid.UUID, mappings []domain.GroupDeptMapping) ([]domain.GroupDeptMapping, error) {
	for _, m := range mappings {
		if m.KeycloakGroupName == "" {
			return nil, domain.NewError(domain.ErrValidation, "keycloak_group_name is required")
		}
		if m.DepartmentID == uuid.Nil {
			return nil, domain.NewError(domain.ErrValidation, "department_id is required").
				WithDetails(map[string]any{"code": "invalid_uuid"})
		}
	}
	out, err := s.repo.ReplaceDeptMappings(ctx, tenantID, mappings)
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx, tenantID)
	return out, nil
}

func (s *GroupMappingService) invalidate(ctx context.Context, tenantID uuid.UUID) {
	if s.cache == nil {
		return
	}
	_ = s.cache.Delete(ctx, cacheKeyGRM(tenantID), cacheKeyGDM(tenantID))
}
