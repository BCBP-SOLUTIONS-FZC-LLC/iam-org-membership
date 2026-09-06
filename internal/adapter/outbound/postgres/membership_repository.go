package postgres

import (
	"context"
	"errors"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MembershipRepository owns tenant_memberships (§16 A14 — lifecycle only,
// no role data). Uses uq_tm_active_user (WHERE deleted_at IS NULL) as the
// unique index so a user can rejoin after a soft-leave.
type MembershipRepository struct {
	pool *pgcommon.Pool
}

var _ port.MembershipRepository = (*MembershipRepository)(nil)

func NewMembershipRepository(pool *pgcommon.Pool) *MembershipRepository {
	return &MembershipRepository{pool: pool}
}

const membershipCols = `id, tenant_id, user_id, status, record_version, created_at, updated_at, deleted_at`

func scanMembership(row pgx.Row) (*domain.TenantMembership, error) {
	var m domain.TenantMembership
	var status string
	if err := row.Scan(&m.ID, &m.TenantID, &m.UserID, &status, &m.RecordVersion, &m.CreatedAt, &m.UpdatedAt, &m.DeletedAt); err != nil {
		return nil, err
	}
	m.Status = domain.MembershipStatus(status)
	return &m, nil
}

// List returns a keyset-paginated page of active memberships (§21.2,
// idx_tm_tenant_created). Order: (created_at, id). Cursor represents the
// last row of the previous page.
func (r *MembershipRepository) List(ctx context.Context, tenantID uuid.UUID, cursor *domain.MembershipListCursor, limit int) (*domain.MembershipListPage, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var out []domain.TenantMembership
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		var rows pgx.Rows
		var err error
		if cursor == nil {
			rows, err = tx.Query(ctx, `
				SELECT `+membershipCols+` FROM tenant_memberships
				WHERE tenant_id = $1 AND deleted_at IS NULL
				ORDER BY created_at ASC, id ASC
				LIMIT $2`, tenantID, limit+1)
		} else {
			rows, err = tx.Query(ctx, `
				SELECT `+membershipCols+` FROM tenant_memberships
				WHERE tenant_id = $1 AND deleted_at IS NULL
				  AND (created_at, id) > ($2, $3)
				ORDER BY created_at ASC, id ASC
				LIMIT $4`, tenantID, cursor.CreatedAt, cursor.ID, limit+1)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMembership(rows)
			if err != nil {
				return err
			}
			out = append(out, *m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	page := &domain.MembershipListPage{}
	if len(out) > limit {
		last := out[limit-1]
		page.NextCursor = &domain.MembershipListCursor{CreatedAt: last.CreatedAt, ID: last.ID}
		out = out[:limit]
	}
	page.Items = make([]domain.MembershipListItem, len(out))
	for i, m := range out {
		page.Items[i] = domain.MembershipListItem{Membership: m}
	}
	return page, nil
}

func (r *MembershipRepository) FindByUserID(ctx context.Context, tenantID, userID uuid.UUID) (*domain.TenantMembership, error) {
	var out *domain.TenantMembership
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+membershipCols+` FROM tenant_memberships WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL`, tenantID, userID)
		m, err := scanMembership(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.ErrMemberNotFound, "member not found")
			}
			return err
		}
		out = m
		return nil
	})
	return out, err
}

// Insert creates a tenant_memberships row (PI-10 idempotent). Redelivery
// or concurrent retry with the same (tenant_id, user_id) resolves via the
// uq_tm_active_user partial-unique index. We deliberately DO NOTHING on
// conflict rather than laundering the row's status (a `suspended` or
// `left` row must not be silently reset to `active` by a KC re-register
// — that would bypass SEAT-1 re-check + TM-8 last-owner gate + audit
// trail). After ON CONFLICT DO NOTHING returns 0 rows, a follow-up SELECT
// fetches the existing active row so the caller sees the true state.
func (r *MembershipRepository) Insert(ctx context.Context, tm *domain.TenantMembership) (*domain.TenantMembership, error) {
	if tm.ID == uuid.Nil {
		tm.ID = uuid.New()
	}
	var out *domain.TenantMembership
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (tenant_id, user_id) WHERE deleted_at IS NULL
			DO NOTHING
			RETURNING `+membershipCols,
			tm.ID, tm.TenantID, tm.UserID, string(tm.Status))
		created, err := scanMembership(row)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if created != nil {
			out = created
			return nil
		}
		// Insert absorbed by ON CONFLICT — return the existing row unchanged.
		// Status stays as-is (suspended/left/active); callers wanting to
		// reactivate a soft-leaver must go through SetStatus with a valid
		// optimistic-lock version and any relevant policy gates.
		winner := tx.QueryRow(ctx, `
			SELECT `+membershipCols+` FROM tenant_memberships
			WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL`,
			tm.TenantID, tm.UserID)
		final, ferr := scanMembership(winner)
		if ferr != nil {
			return ferr
		}
		out = final
		return nil
	})
	return out, err
}

func (r *MembershipRepository) SetStatus(ctx context.Context, tenantID, userID uuid.UUID, status domain.MembershipStatus, expectedVersion int64) (*domain.TenantMembership, error) {
	var out *domain.TenantMembership
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE tenant_memberships SET status = $3
			WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL AND record_version = $4
			RETURNING `+membershipCols,
			tenantID, userID, string(status), expectedVersion)
		m, err := scanMembership(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return r.probeMembership(ctx, tx, tenantID, userID)
			}
			return err
		}
		out = m
		return nil
	})
	return out, err
}

func (r *MembershipRepository) SoftDelete(ctx context.Context, tenantID, userID uuid.UUID, expectedVersion int64) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx, `
			UPDATE tenant_memberships SET deleted_at = now(), status = 'left'
			WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL AND record_version = $3`,
			tenantID, userID, expectedVersion)
		if err != nil {
			return err
		}
		if cmd.RowsAffected() == 0 {
			return r.probeMembership(ctx, tx, tenantID, userID)
		}
		return nil
	})
}

func (r *MembershipRepository) CountActive(ctx context.Context, tenantID uuid.UUID) (int, error) {
	var n int
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM tenant_memberships
			WHERE tenant_id = $1 AND deleted_at IS NULL AND status = 'active'`, tenantID).Scan(&n)
	})
	return n, err
}

func (r *MembershipRepository) ListActiveUserIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT user_id FROM tenant_memberships WHERE tenant_id = $1 AND deleted_at IS NULL`,
			tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}

func (r *MembershipRepository) probeMembership(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) error {
	var currentVersion int64
	err := tx.QueryRow(ctx, `SELECT record_version FROM tenant_memberships WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL`, tenantID, userID).Scan(&currentVersion)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.ErrMemberNotFound, "member not found")
		}
		return err
	}
	return domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict").WithDetails(map[string]any{
		"record_version": currentVersion,
	})
}
