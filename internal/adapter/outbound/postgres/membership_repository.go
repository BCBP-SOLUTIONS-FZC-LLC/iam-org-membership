package postgres

import (
	"context"
	"errors"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MembershipRepository owns tenant_memberships (§16 A14 — lifecycle only,
// no role data). Uses uq_tm_active_user (WHERE deleted_at IS NULL) as the
// unique index.
//
// TM-4/TM-5: a normal removal (P-8) keeps the row with status='left' and
// deleted_at NULL; deleted_at is set only by the identity-deletion / GDPR
// path (I-5, §15.2.2). A 'left' row grants nothing (TM-9), so every read here
// that answers "is this user a member" excludes it, and a re-add reactivates
// the same row (TM-1) instead of inserting a second one.
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
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var out []domain.TenantMembership
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		var rows pgx.Rows
		var err error
		if cursor == nil {
			rows, err = tx.Query(ctx, `
				SELECT `+membershipCols+` FROM tenant_memberships
				WHERE tenant_id = $1 AND deleted_at IS NULL AND status <> 'left'
				ORDER BY created_at ASC, id ASC
				LIMIT $2`, tenantID, limit+1)
		} else {
			rows, err = tx.Query(ctx, `
				SELECT `+membershipCols+` FROM tenant_memberships
				WHERE tenant_id = $1 AND deleted_at IS NULL AND status <> 'left'
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
		row := tx.QueryRow(ctx, `SELECT `+membershipCols+` FROM tenant_memberships WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL AND status <> 'left'`, tenantID, userID)
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

// Insert creates a tenant_memberships row. Redelivery or a concurrent retry
// with the same (tenant_id, user_id) resolves via the uq_tm_active_user
// partial-unique index:
//   - no row → a new row is inserted;
//   - a 'left' row → that row is reactivated to tm.Status (TM-1: status
//     transitions are UPDATEs on the existing row; a removed user who is
//     re-added gets the same row back, never a second one);
//   - an active or suspended row → returned unchanged (a suspended row is
//     never silently reset by a KC re-register).

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
		// Insert absorbed by ON CONFLICT. A 'left' row is reactivated in
		// place (TM-1); an active/suspended row is returned unchanged.
		reactivated := tx.QueryRow(ctx, `
			UPDATE tenant_memberships SET status = $3
			WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL AND status = 'left'
			RETURNING `+membershipCols,
			tm.TenantID, tm.UserID, string(tm.Status))
		back, rerr := scanMembership(reactivated)
		if rerr != nil && !errors.Is(rerr, pgx.ErrNoRows) {
			return rerr
		}
		if back != nil {
			out = back
			return nil
		}
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
			WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL AND status <> 'left' AND record_version = $4
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

// SoftDelete is the identity-deletion path (I-5, §15.2.2): status='left' and
// deleted_at=now(). Normal removal (P-8) does not use it — it sets
// status='left' through SetStatus and keeps deleted_at NULL (TM-4/TM-5).
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

var _ port.LeftMembershipEraser = (*MembershipRepository)(nil)

// MarkLeftMembershipDeleted stamps deleted_at on a row P-8 already marked
// 'left', when I-5 reports the identity deleted (§15.2.2). Idempotent.
func (r *MembershipRepository) MarkLeftMembershipDeleted(ctx context.Context, tenantID, userID uuid.UUID) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE tenant_memberships SET deleted_at = now()
			WHERE tenant_id = $1 AND user_id = $2 AND status = 'left' AND deleted_at IS NULL`,
			tenantID, userID)
		return err
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
			`SELECT user_id FROM tenant_memberships WHERE tenant_id = $1 AND deleted_at IS NULL AND status <> 'left'`,
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
	err := tx.QueryRow(ctx, `SELECT record_version FROM tenant_memberships WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL AND status <> 'left'`, tenantID, userID).Scan(&currentVersion)
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
