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
	// Optimistic-locked (PI-8): the caller passes the record_version it read
	// from the freshly inserted row; a concurrent revoke/accept that bumped
	// record_version yields ErrOptimisticLockConflict.
	SetKeycloakUserID(ctx context.Context, tenantID, id uuid.UUID, keycloakUserID uuid.UUID, expectedVersion int64) error

	// SetStatus transitions the invitation to accepted/expired/revoked
	// (with optimistic locking).
	SetStatus(ctx context.Context, tenantID, id uuid.UUID, status domain.InvitationStatus, expectedVersion int64) (*domain.PendingInvitation, error)

	// SetKCCleanupPending flags for the invitation-kc-cleanup reconciler.
	// Optimistic-locked (PI-8): callers pass the row's expected record_version
	// so a concurrent mutation (e.g. accept racing revoke) is detected instead
	// of silently overwriting.
	SetKCCleanupPending(ctx context.Context, tenantID, id uuid.UUID, pending bool, expectedVersion int64) error

	// CountPending is used by SEAT-1 (active + pending ≤ licensed_seats).
	CountPending(ctx context.Context, tenantID uuid.UUID) (int, error)

	// MostRecentCreatedAt returns the created_at of the most recent invitation
	// for (tenant_id, email) regardless of status — used by PI-11 cooldown.
	// Returns zero time when no prior invitation exists.
	MostRecentCreatedAt(ctx context.Context, tenantID uuid.UUID, email string) (time.Time, error)

	// CountCreatedInWindow returns the number of invitations created for the
	// tenant within the rolling window — used by PI-12 rate limit.
	CountCreatedInWindow(ctx context.Context, tenantID uuid.UUID, since time.Time) (int, error)

	// ListExpiring is the invitation-expiry cron query.
	ListExpiring(ctx context.Context, before time.Time, limit int) ([]domain.PendingInvitation, error)

	// ListPendingKCCleanup is the invitation-kc-cleanup reconciler query.
	ListPendingKCCleanup(ctx context.Context, limit int) ([]domain.PendingInvitation, error)

	// LockByID takes SELECT … FOR UPDATE on the pending_invitations row
	// (PI-4/PI-8 accept-vs-revoke serialization). Returns (nil, nil) when
	// the row is gone.
	LockByID(ctx context.Context, id uuid.UUID) (*domain.PendingInvitation, error)

	// ExpireOverdue flips pending invitations past expires_at to expired
	// and sets kc_cleanup_pending (PI-5 / PI-9). Cross-tenant; callers
	// MUST construct this repository against a BYPASSRLS pool.
	ExpireOverdue(ctx context.Context, limit int) (int, error)

	// ClearKCCleanupPendingByID clears PI-9's durable marker after RP
	// DeleteUser succeeds (or when there is no Keycloak user to delete).
	ClearKCCleanupPendingByID(ctx context.Context, id uuid.UUID) error
}

// InvitationRepositoryNoop supplies LockByID (and can be embedded so new
// methods don't break test fakes that already implement the rest).
type InvitationRepositoryNoop struct{}

func (InvitationRepositoryNoop) LockByID(context.Context, uuid.UUID) (*domain.PendingInvitation, error) {
	return nil, nil
}
func (InvitationRepositoryNoop) ExpireOverdue(context.Context, int) (int, error) { return 0, nil }
func (InvitationRepositoryNoop) ClearKCCleanupPendingByID(context.Context, uuid.UUID) error {
	return nil
}
