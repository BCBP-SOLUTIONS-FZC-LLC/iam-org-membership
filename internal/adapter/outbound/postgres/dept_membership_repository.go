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

type DeptMembershipRepository struct {
	pool *pgcommon.Pool
}

var _ port.DeptMembershipRepository = (*DeptMembershipRepository)(nil)

func NewDeptMembershipRepository(pool *pgcommon.Pool) *DeptMembershipRepository {
	return &DeptMembershipRepository{pool: pool}
}

const dmCols = `id, tenant_id, user_id, tenant_membership_id, department_id, role_level, granted_by, record_version, created_at, updated_at, deleted_at`

func scanDeptMembership(row pgx.Row) (*domain.DeptMembership, error) {
	var d domain.DeptMembership
	var lvl string
	if err := row.Scan(&d.ID, &d.TenantID, &d.UserID, &d.TenantMembershipID, &d.DepartmentID, &lvl, &d.GrantedBy, &d.RecordVersion, &d.CreatedAt, &d.UpdatedAt, &d.DeletedAt); err != nil {
		return nil, err
	}
	d.RoleLevel = domain.DeptRole(lvl)
	return &d, nil
}

func (r *DeptMembershipRepository) ListByUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.DeptMembership, error) {
	return r.listWhere(ctx, `WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL ORDER BY department_id`, tenantID, userID)
}

func (r *DeptMembershipRepository) ListByDepartment(ctx context.Context, tenantID, departmentID uuid.UUID) ([]domain.DeptMembership, error) {
	return r.listWhere(ctx, `WHERE tenant_id = $1 AND department_id = $2 AND deleted_at IS NULL ORDER BY user_id`, tenantID, departmentID)
}

func (r *DeptMembershipRepository) listWhere(ctx context.Context, whereClause string, args ...any) ([]domain.DeptMembership, error) {
	var out []domain.DeptMembership
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+dmCols+` FROM dept_memberships `+whereClause, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			dm, err := scanDeptMembership(rows)
			if err != nil {
				return err
			}
			out = append(out, *dm)
		}
		return rows.Err()
	})
	return out, err
}

// Assign upserts a (user, dept) row to the given level. If the level
// changes, this soft-deletes the existing row and inserts a new one so
// callers can emit DepartmentMembershipLevelChanged with previous_level.
func (r *DeptMembershipRepository) Assign(ctx context.Context, tenantID, userID, departmentID, membershipID uuid.UUID, level domain.DeptRole, grantedBy uuid.UUID) (*domain.DeptMembership, error) {
	var out *domain.DeptMembership
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		// Look up existing active row.
		row := tx.QueryRow(ctx, `
			SELECT `+dmCols+` FROM dept_memberships
			WHERE tenant_id = $1 AND user_id = $2 AND department_id = $3 AND deleted_at IS NULL`,
			tenantID, userID, departmentID)
		existing, err := scanDeptMembership(row)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if existing != nil {
			if existing.RoleLevel == level {
				out = existing
				return nil
			}
			// Level changing — soft-delete old row so the unique index frees.
			if _, err := tx.Exec(ctx, `UPDATE dept_memberships SET deleted_at = now() WHERE id = $1`, existing.ID); err != nil {
				return err
			}
		}
		// Insert new row.
		newRow := tx.QueryRow(ctx, `
			INSERT INTO dept_memberships (id, tenant_id, user_id, tenant_membership_id, department_id, role_level, granted_by)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6)
			RETURNING `+dmCols,
			tenantID, userID, membershipID, departmentID, string(level), grantedBy)
		created, err := scanDeptMembership(newRow)
		if err != nil {
			return err
		}
		out = created
		return nil
	})
	return out, err
}

func (r *DeptMembershipRepository) Remove(ctx context.Context, tenantID, userID, departmentID uuid.UUID) (*domain.DeptMembership, error) {
	var out *domain.DeptMembership
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE dept_memberships SET deleted_at = now()
			WHERE tenant_id = $1 AND user_id = $2 AND department_id = $3 AND deleted_at IS NULL
			RETURNING `+dmCols,
			tenantID, userID, departmentID)
		dm, err := scanDeptMembership(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.ErrMemberNotFound, "dept membership not found")
			}
			return err
		}
		out = dm
		return nil
	})
	return out, err
}

func (r *DeptMembershipRepository) SoftDeleteAllForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.DeptMembership, error) {
	var out []domain.DeptMembership
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE dept_memberships SET deleted_at = now()
			WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL
			RETURNING `+dmCols,
			tenantID, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			dm, err := scanDeptMembership(rows)
			if err != nil {
				return err
			}
			out = append(out, *dm)
		}
		return rows.Err()
	})
	return out, err
}
