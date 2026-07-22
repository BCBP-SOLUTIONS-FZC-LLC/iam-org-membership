package service

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

// TenderACLService owns P-21/P-22/P-23 and I-12 (service-to-service check).
type TenderACLService struct {
	acls        port.TenderACLRepository
	memberships port.MembershipRepository
}

func NewTenderACLService(acls port.TenderACLRepository, m port.MembershipRepository) *TenderACLService {
	return &TenderACLService{acls: acls, memberships: m}
}

func (s *TenderACLService) List(ctx context.Context, tenantID, tenderID uuid.UUID) ([]domain.TenderACLEntry, error) {
	return s.acls.ListByTender(ctx, tenantID, tenderID)
}

// Grant is P-22.
func (s *TenderACLService) Grant(ctx context.Context, tenantID, tenderID, userID uuid.UUID, level domain.TenderACLLevel, actorID uuid.UUID, reason string, expiresAt *time.Time) (*domain.TenderACLEntry, error) {
	switch level {
	case domain.ACLView, domain.ACLEdit, domain.ACLApprove:
	default:
		return nil, domain.NewError(domain.ErrValidation, "invalid access_level").
			WithDetails(map[string]any{"code": "invalid_access_level"})
	}
	if expiresAt != nil && !expiresAt.After(time.Now().UTC()) {
		return nil, domain.NewError(domain.ErrInvalidExpiresAt, "expires_at must be in the future")
	}
	m, err := s.memberships.FindByUserID(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	return s.acls.Grant(ctx, &domain.TenderACLEntry{
		TenantID:           tenantID,
		TenderID:           tenderID,
		UserID:             userID,
		TenantMembershipID: m.ID,
		AccessLevel:        level,
		GrantedBy:          actorID,
		Reason:             reason,
		ExpiresAt:          expiresAt,
	})
}

// Revoke is P-23.
func (s *TenderACLService) Revoke(ctx context.Context, tenantID, tenderID, userID uuid.UUID) (*domain.TenderACLEntry, error) {
	return s.acls.Revoke(ctx, tenantID, tenderID, userID)
}

// CheckAccess is I-12 (service-to-service check).
func (s *TenderACLService) CheckAccess(ctx context.Context, tenantID, tenderID, userID uuid.UUID) (*domain.TenderACLEntry, error) {
	return s.acls.FindActiveForUser(ctx, tenantID, tenderID, userID)
}
