package service

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

// RoleLabelService owns P-12/P-13.
type RoleLabelService struct {
	labels port.DeptRoleLabelRepository
	cache  port.Cache
}

func NewRoleLabelService(labels port.DeptRoleLabelRepository, cache port.Cache) *RoleLabelService {
	return &RoleLabelService{labels: labels, cache: cache}
}

func (s *RoleLabelService) List(ctx context.Context, tenantID uuid.UUID) ([]domain.DeptRoleLabel, error) {
	return s.labels.List(ctx, tenantID)
}

// Update is P-13. Validates role_code and non-empty display_name.
func (s *RoleLabelService) Update(ctx context.Context, tenantID uuid.UUID, roleCode string, displayName string, expectedVersion int64) (*domain.DeptRoleLabel, error) {
	code := domain.DeptRole(roleCode)
	if code != domain.DeptPreparator && code != domain.DeptReviewer && code != domain.DeptApprover {
		return nil, domain.NewError(domain.ErrValidation, "invalid role_code").
			WithDetails(map[string]any{"code": "invalid_role"})
	}
	if displayName == "" {
		return nil, domain.NewError(domain.ErrValidation, "display_name must not be empty")
	}
	l, err := s.labels.Update(ctx, tenantID, code, displayName, expectedVersion)
	if err != nil {
		return nil, err
	}
	if s.cache != nil {
		_ = s.cache.Delete(ctx, cacheKeyRoles(tenantID))
	}
	return l, nil
}
