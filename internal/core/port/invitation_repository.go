package port

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
)

// InvitationRepository owns pending_invitations (§4.2, §16 A11).
type InvitationRepository interface {
	List(ctx context.Context, tenantID uuid.UUID) ([]domain.PendingInvitation, error)
	FindByID(ctx context.Context, tenantID, id uuid.UUID) (*domain.PendingInvitation, error)
	FindPendingByEmail(ctx context.Context, tenantID uuid.UUID, email string) (*domain.PendingInvitation, error)
	FindPendingByKeycloakUser(ctx context.Context, tenantID, keycloakUserID uuid.UUID) (*domain.PendingInvitation, error)

	// Insert stages a new pending invitation. The partial unique
	// uq_pi_pending (WHERE status='pending') enforces PI-1 rejoin-friendly
	// dedup.
	Insert(ctx context.Context, inv *domain.PendingInvitation) (*domain.PendingInvitation, error)

	// SetKeycloakUserID is called after RP CreateInvitedUser returns.
	SetKeycloakUserID(ctx context.Context, tenantID, id uuid.UUID, keycloakUserID uuid.UUID) error

	// SetStatus transitions the invitation to accepted/expired/revoked
	// (with optimistic locking).
	SetStatus(ctx context.Context, tenantID, id uuid.UUID, status domain.InvitationStatus, expectedVersion int64) (*domain.PendingInvitation, error)

	// SetKCCleanupPending flags for the invitation-kc-cleanup reconciler.
	SetKCCleanupPending(ctx context.Context, tenantID, id uuid.UUID, pending bool) error

	// CountPending is used by SEAT-1 (active + pending ≤ licensed_seats).
	CountPending(ctx context.Context, tenantID uuid.UUID) (int, error)

	// ListExpiring is the invitation-expiry cron query.
	ListExpiring(ctx context.Context, before time.Time, limit int) ([]domain.PendingInvitation, error)

	// ListPendingKCCleanup is the invitation-kc-cleanup reconciler query.
	ListPendingKCCleanup(ctx context.Context, limit int) ([]domain.PendingInvitation, error)
}
