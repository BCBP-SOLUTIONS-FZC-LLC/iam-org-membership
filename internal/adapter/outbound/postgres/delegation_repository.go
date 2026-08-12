package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type DelegationRepository struct {
	pool *pgcommon.Pool
}

var _ port.DelegationRepository = (*DelegationRepository)(nil)

func NewDelegationRepository(pool *pgcommon.Pool) *DelegationRepository {
	return &DelegationRepository{pool: pool}
}

const delegationCols = `id, tenant_id, delegator_id, delegate_id,
	delegator_membership_id, delegate_membership_id,
	scope, scope_id, reason, starts_at, ends_at, status,
	record_version, created_at, updated_at, deleted_at,
	review_due_at, review_notice_sent_at, review_window_days`

func scanDelegation(row pgx.Row) (*domain.Delegation, error) {
	var d domain.Delegation
	var scope, status string
	var reason *string
	if err := row.Scan(&d.ID, &d.TenantID, &d.DelegatorID, &d.DelegateID,
		&d.DelegatorMembershipID, &d.DelegateMembershipID,
		&scope, &d.ScopeID, &reason, &d.StartsAt, &d.EndsAt, &status,
		&d.RecordVersion, &d.CreatedAt, &d.UpdatedAt, &d.DeletedAt,
		&d.ReviewDueAt, &d.ReviewNoticeSentAt, &d.ReviewWindowDays); err != nil {
		return nil, err
	}
	d.Scope = domain.DelegationScope(scope)
	d.Status = domain.DelegationStatus(status)
	if reason != nil {
		d.Reason = *reason
	}
	return &d, nil
}

func (r *DelegationRepository) List(ctx context.Context, tenantID uuid.UUID) ([]domain.Delegation, error) {
	return r.listWhere(ctx, `WHERE tenant_id = $1 AND deleted_at IS NULL ORDER BY starts_at DESC`, tenantID)
}

func (r *DelegationRepository) ListByDelegator(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, error) {
	return r.listWhere(ctx, `WHERE tenant_id = $1 AND delegator_id = $2 AND deleted_at IS NULL ORDER BY starts_at DESC`, tenantID, delegatorID)
}

func (r *DelegationRepository) listWhere(ctx context.Context, whereClause string, args ...any) ([]domain.Delegation, error) {
	var out []domain.Delegation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+delegationCols+` FROM delegations `+whereClause, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDelegation(rows)
			if err != nil {
				return err
			}
			out = append(out, *d)
		}
		return rows.Err()
	})
	return out, err
}

func (r *DelegationRepository) FindByID(ctx context.Context, tenantID, id uuid.UUID) (*domain.Delegation, error) {
	var out *domain.Delegation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+delegationCols+` FROM delegations WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenantID, id)
		d, err := scanDelegation(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.ErrDelegationNotFound, "delegation not found")
			}
			return err
		}
		out = d
		return nil
	})
	return out, err
}

func (r *DelegationRepository) Insert(ctx context.Context, d *domain.Delegation) (*domain.Delegation, error) {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	if d.StartsAt.IsZero() {
		d.StartsAt = time.Now().UTC()
	}
	if d.Status == "" {
		d.Status = domain.DelegationActive
	}
	var reason *string
	if d.Reason != "" {
		reason = &d.Reason
	}
	var out *domain.Delegation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO delegations (id, tenant_id, delegator_id, delegate_id,
				delegator_membership_id, delegate_membership_id,
				scope, scope_id, reason, starts_at, ends_at, status, review_due_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			RETURNING `+delegationCols,
			d.ID, d.TenantID, d.DelegatorID, d.DelegateID,
			d.DelegatorMembershipID, d.DelegateMembershipID,
			string(d.Scope), d.ScopeID, reason, d.StartsAt, d.EndsAt, string(d.Status), d.ReviewDueAt)
		created, err := scanDelegation(row)
		if err != nil {
			return err
		}
		out = created
		return nil
	})
	return out, err
}

func (r *DelegationRepository) End(ctx context.Context, tenantID, id uuid.UUID, status domain.DelegationStatus, expectedVersion int64) (*domain.Delegation, error) {
	var out *domain.Delegation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE delegations SET status = $3
			WHERE tenant_id = $1 AND id = $2 AND record_version = $4 AND deleted_at IS NULL AND status = 'active'
			RETURNING `+delegationCols,
			tenantID, id, string(status), expectedVersion)
		d, err := scanDelegation(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				var v int64
				var s string
				probe := tx.QueryRow(ctx, `SELECT record_version, status FROM delegations WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenantID, id)
				if perr := probe.Scan(&v, &s); perr != nil {
					if errors.Is(perr, pgx.ErrNoRows) {
						return domain.NewError(domain.ErrDelegationNotFound, "delegation not found")
					}
					return perr
				}
				// Terminal state (cancelled/expired) — treat as not found per DEL-3.
				if s != "active" {
					return domain.NewError(domain.ErrDelegationNotFound, "delegation not found")
				}
				return domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict").
					WithDetails(map[string]any{"record_version": v})
			}
			return err
		}
		out = d
		return nil
	})
	return out, err
}

func (r *DelegationRepository) ListExpiringBefore(ctx context.Context, before time.Time, limit int) ([]domain.Delegation, error) {
	return r.listWhere(ctx, `WHERE ends_at < $1 AND status = 'active' AND deleted_at IS NULL ORDER BY ends_at LIMIT `+itoa(limit), before)
}

func (r *DelegationRepository) FindActiveDeptDelegateForUser(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.Delegation, error) {
	var out *domain.Delegation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+delegationCols+`
			FROM delegations
			WHERE tenant_id = $1 AND delegate_id = $2 AND scope = 'department' AND scope_id = $3
			  AND status = 'active' AND deleted_at IS NULL
			LIMIT 1`,
			tenantID, userID, deptID)
		d, err := scanDelegation(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		out = d
		return nil
	})
	return out, err
}

// SoftDeleteForUser is a CASCADE-ONLY method called by the user removal
// path (I-5 / P-7). CONC-1 optimistic locking is intentionally not applied
// (same rationale as DeptMembershipRepository.SoftDeleteAllForUser). For
// direct end-delegation flows use End(), which optimistic-locks.
func (r *DelegationRepository) SoftDeleteForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.Delegation, error) {
	var out []domain.Delegation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE delegations SET status = 'ended', deleted_at = now()
			WHERE tenant_id = $1 AND (delegator_id = $2 OR delegate_id = $2) AND deleted_at IS NULL
			RETURNING `+delegationCols,
			tenantID, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDelegation(rows)
			if err != nil {
				return err
			}
			out = append(out, *d)
		}
		return rows.Err()
	})
	return out, err
}

// ExtendReview pushes review_due_at forward by windowDays and resets review_notice_sent_at to NULL (DEL-13, P-32).
func (r *DelegationRepository) ExtendReview(ctx context.Context, tenantID, id uuid.UUID, windowDays int, expectedVersion int64) (*domain.Delegation, error) {
	var out *domain.Delegation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE delegations
			SET review_due_at = review_due_at + ($3::text || ' days')::interval,
			    review_notice_sent_at = NULL,
			    review_window_days = $3
			WHERE tenant_id = $1 AND id = $2 AND record_version = $4
			  AND deleted_at IS NULL AND status = 'active' AND ends_at IS NULL
			RETURNING `+delegationCols,
			tenantID, id, windowDays, expectedVersion)
		d, err := scanDelegation(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// probe for the right error
				var v int64
				var s string
				var endAt *time.Time
				probe := tx.QueryRow(ctx, `SELECT record_version, status, ends_at FROM delegations WHERE tenant_id=$1 AND id=$2 AND deleted_at IS NULL`, tenantID, id)
				if perr := probe.Scan(&v, &s, &endAt); perr != nil {
					if errors.Is(perr, pgx.ErrNoRows) {
						return domain.NewError(domain.ErrDelegationNotFound, "delegation not found")
					}
					return perr
				}
				if endAt != nil {
					return domain.NewError(domain.ErrDelegationNotOpenEnded, "extend only applies to open-ended delegations (ends_at IS NULL)")
				}
				if s != "active" {
					return domain.NewError(domain.ErrDelegationNotFound, "delegation not found")
				}
				return domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict").
					WithDetails(map[string]any{"record_version": v})
			}
			return err
		}
		out = d
		return nil
	})
	return out, err
}

// FindOpenEndedForReview returns delegations where ends_at IS NULL AND review_due_at <= threshold.
func (r *DelegationRepository) FindOpenEndedForReview(ctx context.Context, threshold time.Time, limit int) ([]domain.Delegation, error) {
	return r.listWhere(ctx,
		`WHERE ends_at IS NULL AND status = 'active' AND deleted_at IS NULL
		   AND review_due_at IS NOT NULL AND review_due_at <= $1
		 ORDER BY review_due_at LIMIT `+itoa(limit), threshold)
}

// FindOpenEndedForWarning returns delegations within the warning window that have not yet been warned.
func (r *DelegationRepository) FindOpenEndedForWarning(ctx context.Context, warnBefore time.Time, limit int) ([]domain.Delegation, error) {
	return r.listWhere(ctx,
		`WHERE ends_at IS NULL AND status = 'active' AND deleted_at IS NULL
		   AND review_due_at IS NOT NULL AND review_due_at > now()
		   AND review_due_at <= $1 AND review_notice_sent_at IS NULL
		 ORDER BY review_due_at LIMIT `+itoa(limit), warnBefore)
}

// MarkReviewNoticeSent sets review_notice_sent_at = now() for a delegation.
func (r *DelegationRepository) MarkReviewNoticeSent(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx, `
			UPDATE delegations SET review_notice_sent_at = now()
			WHERE tenant_id = $1 AND id = $2 AND record_version = $3
			  AND deleted_at IS NULL AND status = 'active'`,
			tenantID, id, expectedVersion)
		if err != nil {
			return err
		}
		if cmd.RowsAffected() == 0 {
			return domain.NewError(domain.ErrDelegationNotFound, "delegation not found or version conflict")
		}
		return nil
	})
}

// itoa is a tiny helper so we can inline the LIMIT clause without importing strconv into the DDL string.
func itoa(n int) string {
	if n <= 0 {
		return "100"
	}
	// Simple positive-int formatter.
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
