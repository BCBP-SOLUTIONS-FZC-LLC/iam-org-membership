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

// TenantRoleRepository owns elevated grants. 'member' barred by chk_tr_no_member.
type TenantRoleRepository struct {
	pool *pgcommon.Pool
}

var _ port.TenantRoleRepository = (*TenantRoleRepository)(nil)

func NewTenantRoleRepository(pool *pgcommon.Pool) *TenantRoleRepository {
	return &TenantRoleRepository{pool: pool}
}

const tenantRoleCols = `id, tenant_id, user_id, tenant_membership_id, role_code, granted_by, record_version, created_at, updated_at, deleted_at`

func scanTenantRole(row pgx.Row) (*domain.TenantRole, error) {
	var r domain.TenantRole
	var role string
	if err := row.Scan(&r.ID, &r.TenantID, &r.UserID, &r.TenantMembershipID, &role, &r.GrantedBy, &r.RecordVersion, &r.CreatedAt, &r.UpdatedAt, &r.DeletedAt); err != nil {
		return nil, err
	}
	r.RoleCode = domain.TenantRoleCode(role)
	return &r, nil
}

func (r *TenantRoleRepository) ListByUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenantRole, error) {
	return r.listWhere(ctx, `WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL`, tenantID, userID)
}

func (r *TenantRoleRepository) ListByRole(ctx context.Context, tenantID uuid.UUID, role domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return r.listWhere(ctx, `WHERE tenant_id = $1 AND role_code = $2 AND deleted_at IS NULL`, tenantID, string(role))
}

func (r *TenantRoleRepository) listWhere(ctx context.Context, whereClause string, args ...any) ([]domain.TenantRole, error) {
	var out []domain.TenantRole
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+tenantRoleCols+` FROM tenant_roles `+whereClause+` ORDER BY role_code`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			tr, err := scanTenantRole(rows)
			if err != nil {
				return err
			}
			out = append(out, *tr)
		}
		return rows.Err()
	})
	return out, err
}

// CountActiveOwners is the hot-path TM-8 guard query.
func (r *TenantRoleRepository) CountActiveOwners(ctx context.Context, tenantID uuid.UUID) (int, error) {
	var n int
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM tenant_roles
			WHERE tenant_id = $1 AND role_code = 'tenant_owner' AND deleted_at IS NULL`, tenantID).Scan(&n)
	})
	return n, err
}

func (r *TenantRoleRepository) Grant(ctx context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
	if tr.ID == uuid.Nil {
		tr.ID = uuid.New()
	}
	var out *domain.TenantRole
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO tenant_roles (id, tenant_id, user_id, tenant_membership_id, role_code, granted_by)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING `+tenantRoleCols,
			tr.ID, tr.TenantID, tr.UserID, tr.TenantMembershipID, string(tr.RoleCode), tr.GrantedBy)
		created, err := scanTenantRole(row)
		if err != nil {
			return err
		}
		out = created
		return nil
	})
	return out, err
}

func (r *TenantRoleRepository) Revoke(ctx context.Context, tenantID, userID uuid.UUID, role domain.TenantRoleCode) (*domain.TenantRole, error) {
	var out *domain.TenantRole
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE tenant_roles SET deleted_at = now()
			WHERE tenant_id = $1 AND user_id = $2 AND role_code = $3 AND deleted_at IS NULL
			RETURNING `+tenantRoleCols,
			tenantID, userID, string(role))
		tr, err := scanTenantRole(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.ErrMemberNotFound, "role grant not found")
			}
			return err
		}
		out = tr
		return nil
	})
	return out, err
}

func (r *TenantRoleRepository) SoftDeleteAllForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenantRole, error) {
	var out []domain.TenantRole
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE tenant_roles SET deleted_at = now()
			WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL
			RETURNING `+tenantRoleCols,
			tenantID, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			tr, err := scanTenantRole(rows)
			if err != nil {
				return err
			}
			out = append(out, *tr)
		}
		return rows.Err()
	})
	return out, err
}
