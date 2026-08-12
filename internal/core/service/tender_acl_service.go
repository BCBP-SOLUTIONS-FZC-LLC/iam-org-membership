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
	// B-TAE-02: reason capped at 500 chars (§16 A27, same cap as DEL-10).
	if len(reason) > 500 {
		return nil, domain.NewError(domain.ErrValidation, "reason must not exceed 500 characters").
			WithDetails(map[string]any{"code": "reason_too_long"})
	}
	m, err := s.memberships.FindByUserID(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	// TAE-5: grantee must hold an active tenant membership (not suspended/left).
	if m.Status != domain.MembershipActive {
		return nil, domain.NewError(domain.ErrMemberNotActive, "grantee must be an active tenant member")
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
