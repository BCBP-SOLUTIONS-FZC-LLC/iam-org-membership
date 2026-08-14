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
//
// Returns (current, previous, err) — previous is nil when no active row
// existed before this call (a fresh grant). previous MUST come from here,
// not from a separate pre-fetch by the caller: the FOR UPDATE probe below
// only locks an EXISTING row, so on a brand-new (tenant, user, dept) key
// there is nothing to lock, and two truly concurrent callers would both
// see "no existing row" and both misclassify their write as a fresh grant
// (B15). The pg_advisory_xact_lock below closes that gap by serializing
// concurrent callers for the same key even before any row exists;
// transaction-scoped, released automatically on commit/rollback.
func (r *DeptMembershipRepository) Assign(ctx context.Context, tenantID, userID, departmentID, membershipID uuid.UUID, level domain.DeptRole, grantedBy uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
	var out, previous *domain.DeptMembership
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended($1::text || $2::text || $3::text, 0))`,
			tenantID, userID, departmentID); err != nil {
			return err
		}
		// Look up existing active row with FOR UPDATE to serialize concurrent
		// callers holding the same (tenant, user, dept) triple (idempotency,
		// PI-10 spirit for dept memberships).
		row := tx.QueryRow(ctx, `
			SELECT `+dmCols+` FROM dept_memberships
			WHERE tenant_id = $1 AND user_id = $2 AND department_id = $3 AND deleted_at IS NULL
			FOR UPDATE`,
			tenantID, userID, departmentID)
		existing, err := scanDeptMembership(row)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		previous = existing
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
		// Insert new row. ON CONFLICT DO NOTHING absorbs the race where a
		// concurrent worker inserted first (they hold uq_dm_active_membership
		// and our FOR UPDATE only locked existing rows, not phantom inserts).
		newRow := tx.QueryRow(ctx, `
			INSERT INTO dept_memberships (id, tenant_id, user_id, tenant_membership_id, department_id, role_level, granted_by)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6)
			ON CONFLICT (tenant_id, user_id, department_id) WHERE deleted_at IS NULL
			DO NOTHING
			RETURNING `+dmCols,
			tenantID, userID, membershipID, departmentID, string(level), grantedBy)
		created, err := scanDeptMembership(newRow)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if created != nil {
			out = created
			return nil
		}
		// Insert absorbed by ON CONFLICT — fetch the winning row.
		winner := tx.QueryRow(ctx, `
			SELECT `+dmCols+` FROM dept_memberships
			WHERE tenant_id = $1 AND user_id = $2 AND department_id = $3 AND deleted_at IS NULL`,
			tenantID, userID, departmentID)
		final, ferr := scanDeptMembership(winner)
		if ferr != nil {
			return ferr
		}
		out = final
		return nil
	})
	return out, previous, err
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

// SoftDeleteAllForDept is called on department deactivation (GAP-P25-1).
// Soft-deletes every active dept_membership row for the given (tenant, dept).
func (r *DeptMembershipRepository) SoftDeleteAllForDept(ctx context.Context, tenantID, departmentID uuid.UUID) ([]domain.DeptMembership, error) {
	var out []domain.DeptMembership
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE dept_memberships SET deleted_at = now()
			WHERE tenant_id = $1 AND department_id = $2 AND deleted_at IS NULL
			RETURNING `+dmCols,
			tenantID, departmentID)
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

// SoftDeleteAllForUser is a CASCADE-ONLY method called by the user removal
// path (I-5 / P-7). CONC-1 optimistic locking is deliberately not applied
// here — the caller is deleting the parent tenant_membership, and enforcing
// per-row record_version would cause a partial-cascade (some rows locked
// out, others deleted) that would leave the tenant in an inconsistent state.
// This is safe because dept memberships have no dependents once the user
// is gone. Do not call this from a direct-mutation endpoint — use Remove()
// for those, which DOES optimistic-lock the target row.
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
