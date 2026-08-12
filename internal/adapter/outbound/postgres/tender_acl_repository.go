package postgres

import (
	"context"
	"errors"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type TenderACLRepository struct {
	pool *pgcommon.Pool
}

var _ port.TenderACLRepository = (*TenderACLRepository)(nil)

func NewTenderACLRepository(pool *pgcommon.Pool) *TenderACLRepository {
	return &TenderACLRepository{pool: pool}
}

const taeCols = `id, tenant_id, tender_id, user_id, tenant_membership_id, access_level, granted_by, reason, expires_at, record_version, created_at, updated_at, deleted_at`

func scanTenderACL(row pgx.Row) (*domain.TenderACLEntry, error) {
	var e domain.TenderACLEntry
	var level string
	var reason *string
	if err := row.Scan(&e.ID, &e.TenantID, &e.TenderID, &e.UserID, &e.TenantMembershipID,
		&level, &e.GrantedBy, &reason, &e.ExpiresAt, &e.RecordVersion,
		&e.CreatedAt, &e.UpdatedAt, &e.DeletedAt); err != nil {
		return nil, err
	}
	e.AccessLevel = domain.TenderACLLevel(level)
	if reason != nil {
		e.Reason = *reason
	}
	return &e, nil
}

func (r *TenderACLRepository) ListByTender(ctx context.Context, tenantID, tenderID uuid.UUID) ([]domain.TenderACLEntry, error) {
	var out []domain.TenderACLEntry
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		// TAE-7: P-21 list is for admin management — returns active AND passively
		// expired rows (deleted_at IS NULL) so admins can see and explicitly
		// revoke expired grants. The expiry filter is TAE-3's authorization
		// concern (I-12 FindActiveForUser), not the management list.
		rows, err := tx.Query(ctx, `
			SELECT `+taeCols+` FROM tender_acl_entries
			WHERE tenant_id = $1 AND tender_id = $2 AND deleted_at IS NULL
			ORDER BY user_id`,
			tenantID, tenderID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanTenderACL(rows)
			if err != nil {
				return err
			}
			out = append(out, *e)
		}
		return rows.Err()
	})
	return out, err
}

// FindActiveForUser returns the active entry for (tenant, tender, user).
// Active = deleted_at IS NULL AND (expires_at IS NULL OR expires_at > now())
// (TAE-3). Returns nil if no active grant.
func (r *TenderACLRepository) FindActiveForUser(ctx context.Context, tenantID, tenderID, userID uuid.UUID) (*domain.TenderACLEntry, error) {
	var out *domain.TenderACLEntry
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+taeCols+` FROM tender_acl_entries
			WHERE tenant_id = $1 AND tender_id = $2 AND user_id = $3
			  AND deleted_at IS NULL
			  AND (expires_at IS NULL OR expires_at > now())`,
			tenantID, tenderID, userID)
		e, err := scanTenderACL(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // active grant absence is not an error
			}
			return err
		}
		out = e
		return nil
	})
	return out, err
}

func (r *TenderACLRepository) Grant(ctx context.Context, e *domain.TenderACLEntry) (*domain.TenderACLEntry, error) {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	var reason *string
	if e.Reason != "" {
		reason = &e.Reason
	}
	var out *domain.TenderACLEntry
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO tender_acl_entries (id, tenant_id, tender_id, user_id, tenant_membership_id,
				access_level, granted_by, reason, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING `+taeCols,
			e.ID, e.TenantID, e.TenderID, e.UserID, e.TenantMembershipID,
			string(e.AccessLevel), e.GrantedBy, reason, e.ExpiresAt)
		created, err := scanTenderACL(row)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_tae_active_entry" {
				return domain.NewError(domain.ErrACLAlreadyExists, "active ACL grant already exists for this user on this tender")
			}
			return err
		}
		out = created
		return nil
	})
	return out, err
}

func (r *TenderACLRepository) Revoke(ctx context.Context, tenantID, tenderID, userID uuid.UUID) (*domain.TenderACLEntry, error) {
	var out *domain.TenderACLEntry
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE tender_acl_entries SET deleted_at = now()
			WHERE tenant_id = $1 AND tender_id = $2 AND user_id = $3 AND deleted_at IS NULL
			RETURNING `+taeCols,
			tenantID, tenderID, userID)
		e, err := scanTenderACL(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.ErrMemberNotFound, "acl entry not found")
			}
			return err
		}
		out = e
		return nil
	})
	return out, err
}

// SoftDeleteForUser is a CASCADE-ONLY method called by the user removal
// path (I-5 / P-7). CONC-1 optimistic locking is intentionally not applied
// (same rationale as DeptMembershipRepository.SoftDeleteAllForUser). For
// direct ACL revoke flows use Revoke(), which optimistic-locks.
func (r *TenderACLRepository) SoftDeleteForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenderACLEntry, error) {
	var out []domain.TenderACLEntry
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE tender_acl_entries SET deleted_at = now()
			WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL
			RETURNING `+taeCols,
			tenantID, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanTenderACL(rows)
			if err != nil {
				return err
			}
			out = append(out, *e)
		}
		return rows.Err()
	})
	return out, err
}
