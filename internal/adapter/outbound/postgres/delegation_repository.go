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
	record_version, created_at, updated_at, deleted_at`

func scanDelegation(row pgx.Row) (*domain.Delegation, error) {
	var d domain.Delegation
	var scope, status string
	var reason *string
	if err := row.Scan(&d.ID, &d.TenantID, &d.DelegatorID, &d.DelegateID,
		&d.DelegatorMembershipID, &d.DelegateMembershipID,
		&scope, &d.ScopeID, &reason, &d.StartsAt, &d.EndsAt, &status,
		&d.RecordVersion, &d.CreatedAt, &d.UpdatedAt, &d.DeletedAt); err != nil {
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
				scope, scope_id, reason, starts_at, ends_at, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			RETURNING `+delegationCols,
			d.ID, d.TenantID, d.DelegatorID, d.DelegateID,
			d.DelegatorMembershipID, d.DelegateMembershipID,
			string(d.Scope), d.ScopeID, reason, d.StartsAt, d.EndsAt, string(d.Status))
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
			WHERE tenant_id = $1 AND id = $2 AND record_version = $4 AND deleted_at IS NULL
			RETURNING `+delegationCols,
			tenantID, id, string(status), expectedVersion)
		d, err := scanDelegation(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				var v int64
				probe := tx.QueryRow(ctx, `SELECT record_version FROM delegations WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenantID, id)
				if perr := probe.Scan(&v); perr != nil {
					if errors.Is(perr, pgx.ErrNoRows) {
						return domain.NewError(domain.ErrDelegationNotFound, "delegation not found")
					}
					return perr
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
