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

type DeptRoleLabelRepository struct {
	pool *pgcommon.Pool
}

var _ port.DeptRoleLabelRepository = (*DeptRoleLabelRepository)(nil)

func NewDeptRoleLabelRepository(pool *pgcommon.Pool) *DeptRoleLabelRepository {
	return &DeptRoleLabelRepository{pool: pool}
}

const drlCols = `id, tenant_id, role_code, display_name, record_version, created_at, updated_at`

func scanDeptRoleLabel(row pgx.Row) (*domain.DeptRoleLabel, error) {
	var l domain.DeptRoleLabel
	var code string
	if err := row.Scan(&l.ID, &l.TenantID, &code, &l.DisplayName, &l.RecordVersion, &l.CreatedAt, &l.UpdatedAt); err != nil {
		return nil, err
	}
	l.RoleCode = domain.DeptRole(code)
	return &l, nil
}

func (r *DeptRoleLabelRepository) List(ctx context.Context, tenantID uuid.UUID) ([]domain.DeptRoleLabel, error) {
	var out []domain.DeptRoleLabel
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+drlCols+` FROM dept_role_labels WHERE tenant_id = $1 ORDER BY role_code`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			l, err := scanDeptRoleLabel(rows)
			if err != nil {
				return err
			}
			out = append(out, *l)
		}
		return rows.Err()
	})
	return out, err
}

func (r *DeptRoleLabelRepository) Update(ctx context.Context, tenantID uuid.UUID, roleCode domain.DeptRole, displayName string, expectedVersion int64) (*domain.DeptRoleLabel, error) {
	var out *domain.DeptRoleLabel
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE dept_role_labels SET display_name = $3
			WHERE tenant_id = $1 AND role_code = $2 AND record_version = $4
			RETURNING `+drlCols,
			tenantID, string(roleCode), displayName, expectedVersion)
		l, err := scanDeptRoleLabel(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				var v int64
				probe := tx.QueryRow(ctx, `SELECT record_version FROM dept_role_labels WHERE tenant_id = $1 AND role_code = $2`, tenantID, string(roleCode))
				if perr := probe.Scan(&v); perr != nil {
					if errors.Is(perr, pgx.ErrNoRows) {
						return domain.NewError(domain.ErrMemberNotFound, "role label not found")
					}
					return perr
				}
				return domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict").
					WithDetails(map[string]any{"record_version": v})
			}
			return err
		}
		out = l
		return nil
	})
	return out, err
}

// Seed inserts the three default labels for a new tenant (DRL-1).
// Idempotent via ON CONFLICT (tenant_id, role_code) DO NOTHING.
func (r *DeptRoleLabelRepository) Seed(ctx context.Context, tenantID uuid.UUID) ([]domain.DeptRoleLabel, error) {
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO dept_role_labels (id, tenant_id, role_code, display_name)
			VALUES
				(gen_random_uuid(), $1, 'preparator', 'Preparator'),
				(gen_random_uuid(), $1, 'reviewer',   'Reviewer'),
				(gen_random_uuid(), $1, 'approver',   'Approver')
			ON CONFLICT (tenant_id, role_code) DO NOTHING`, tenantID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return r.List(ctx, tenantID)
}
