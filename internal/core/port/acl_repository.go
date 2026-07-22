package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
)

// TenderACLRepository owns tender_acl_entries — additive overlays on
// restricted tenders. No FK on tender_id (cross-service, Tender Service
// owns tenders).
type TenderACLRepository interface {
	// ListByTender returns all active entries for a tender (view/edit/approve).
	ListByTender(ctx context.Context, tenantID, tenderID uuid.UUID) ([]domain.TenderACLEntry, error)

	// FindActiveForUser returns the active entry (deleted_at IS NULL AND
	// (expires_at IS NULL OR expires_at > now())) for (tender, user), or
	// nil if no active grant exists. Used by I-12 service-to-service check.
	FindActiveForUser(ctx context.Context, tenantID, tenderID, userID uuid.UUID) (*domain.TenderACLEntry, error)

	// Grant inserts a new ACL entry (P-22).
	Grant(ctx context.Context, e *domain.TenderACLEntry) (*domain.TenderACLEntry, error)

	// Revoke soft-deletes an active entry (P-23).
	Revoke(ctx context.Context, tenantID, tenderID, userID uuid.UUID) (*domain.TenderACLEntry, error)

	// SoftDeleteForUser cascades on membership removal.
	SoftDeleteForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenderACLEntry, error)
}
