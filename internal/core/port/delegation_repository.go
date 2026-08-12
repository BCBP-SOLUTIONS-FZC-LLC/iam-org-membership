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

	// FindActiveDeptDelegateForUser returns the active delegation (if any)
	// where the given user is the delegate for a `scope='department'` grant
	// on the given department. Used by WFI-11 §8.8.4 to pass the resolved
	// delegation.id to WorkflowClient.GetDelegateImpact so the workflow
	// dept-scope query is precise rather than tenant-wide.
	// Returns (nil, nil) when no such delegation exists.
	FindActiveDeptDelegateForUser(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.Delegation, error)

	// ExtendReview pushes review_due_at forward by windowDays and resets
	// review_notice_sent_at to NULL (DEL-13, P-32).
	ExtendReview(ctx context.Context, tenantID, id uuid.UUID, windowDays int, expectedVersion int64) (*domain.Delegation, error)

	// FindOpenEndedForReview returns delegations where ends_at IS NULL AND
	// review_due_at IS NOT NULL AND review_due_at <= threshold, with limit.
	FindOpenEndedForReview(ctx context.Context, threshold time.Time, limit int) ([]domain.Delegation, error)

	// FindOpenEndedForWarning returns delegations where ends_at IS NULL AND
	// review_due_at IS NOT NULL AND review_due_at > now() AND
	// review_due_at <= warnBefore AND review_notice_sent_at IS NULL, with limit.
	FindOpenEndedForWarning(ctx context.Context, warnBefore time.Time, limit int) ([]domain.Delegation, error)

	// MarkReviewNoticeSent sets review_notice_sent_at = now() for a delegation.
	MarkReviewNoticeSent(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64) error
}
