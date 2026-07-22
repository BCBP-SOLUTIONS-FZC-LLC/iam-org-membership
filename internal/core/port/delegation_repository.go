package port

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
)

// DelegationRepository owns the delegations aggregate.
type DelegationRepository interface {
	List(ctx context.Context, tenantID uuid.UUID) ([]domain.Delegation, error)
	ListByDelegator(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, error)
	FindByID(ctx context.Context, tenantID, id uuid.UUID) (*domain.Delegation, error)

	Insert(ctx context.Context, d *domain.Delegation) (*domain.Delegation, error)

	// End flips status → 'ended'/'cancelled' with optimistic locking and
	// returns the resulting row (event payload uses previous_* fields).
	End(ctx context.Context, tenantID, id uuid.UUID, status domain.DelegationStatus, expectedVersion int64) (*domain.Delegation, error)

	// ListExpiringBefore is the delegation-expiry cron query
	// (idx_delegations_ends_at).
	ListExpiringBefore(ctx context.Context, before time.Time, limit int) ([]domain.Delegation, error)

	// SoftDeleteForUser cascades on membership removal — closes any active
	// delegations where the removed user is delegator or delegate.
	SoftDeleteForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.Delegation, error)
}
