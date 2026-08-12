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

// TenantDepartmentRepository operates on tenant_departments (RLS-scoped).
type TenantDepartmentRepository struct {
	pool *pgcommon.Pool
}

var _ port.TenantDepartmentRepository = (*TenantDepartmentRepository)(nil)

func NewTenantDepartmentRepository(pool *pgcommon.Pool) *TenantDepartmentRepository {
	return &TenantDepartmentRepository{pool: pool}
}

const tenantDeptSelectColumns = `tenant_id, department_id, is_active, record_version, created_at, updated_at`

func scanTenantDept(row pgx.Row) (*domain.TenantDepartment, error) {
	var td domain.TenantDepartment
	if err := row.Scan(&td.TenantID, &td.DepartmentID, &td.IsActive, &td.RecordVersion, &td.CreatedAt, &td.UpdatedAt); err != nil {
		return nil, err
	}
	return &td, nil
}

func (r *TenantDepartmentRepository) List(ctx context.Context, tenantID uuid.UUID) ([]domain.TenantDepartment, error) {
	return r.listWhere(ctx, tenantID, "")
}

func (r *TenantDepartmentRepository) ListActive(ctx context.Context, tenantID uuid.UUID) ([]domain.TenantDepartment, error) {
	return r.listWhere(ctx, tenantID, " AND is_active = true")
}

func (r *TenantDepartmentRepository) listWhere(ctx context.Context, tenantID uuid.UUID, extra string) ([]domain.TenantDepartment, error) {
	sql := `SELECT ` + tenantDeptSelectColumns + ` FROM tenant_departments WHERE tenant_id = $1` + extra + ` ORDER BY department_id`
	var out []domain.TenantDepartment
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			td, err := scanTenantDept(rows)
			if err != nil {
				return err
			}
			out = append(out, *td)
		}
		return rows.Err()
	})
	return out, err
}

func (r *TenantDepartmentRepository) Find(ctx context.Context, tenantID, departmentID uuid.UUID) (*domain.TenantDepartment, error) {
	var out *domain.TenantDepartment
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+tenantDeptSelectColumns+` FROM tenant_departments WHERE tenant_id = $1 AND department_id = $2`, tenantID, departmentID)
		td, err := scanTenantDept(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.ErrDepartmentNotFound, "department not found for tenant")
			}
			return err
		}
		out = td
		return nil
	})
	return out, err
}

// Activate inserts a tenant_departments row with is_active=true.
// TD-7: if a row for (tenant_id, department_id) already exists — active OR
// inactive — returns ErrDepartmentAlreadyActivated (409). Use P-25 SetActive
// to bring an inactive row back.
func (r *TenantDepartmentRepository) Activate(ctx context.Context, tenantID, departmentID uuid.UUID) (*domain.TenantDepartment, error) {
	var out *domain.TenantDepartment
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO tenant_departments (tenant_id, department_id, is_active)
			VALUES ($1, $2, true)
			ON CONFLICT (tenant_id, department_id) DO NOTHING
			RETURNING `+tenantDeptSelectColumns,
			tenantID, departmentID)
		td, err := scanTenantDept(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.ErrDepartmentAlreadyActivated,
					"department is already activated for this tenant")
			}
			return err
		}
		out = td
		return nil
	})
	return out, err
}

// SetActive flips is_active with optimistic locking (P-25).
func (r *TenantDepartmentRepository) SetActive(ctx context.Context, tenantID, departmentID uuid.UUID, isActive bool, expectedVersion int64) (*domain.TenantDepartment, error) {
	var out *domain.TenantDepartment
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE tenant_departments SET is_active = $3
			WHERE tenant_id = $1 AND department_id = $2 AND record_version = $4
			RETURNING `+tenantDeptSelectColumns,
			tenantID, departmentID, isActive, expectedVersion)
		td, err := scanTenantDept(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				var currentVersion int64
				probe := tx.QueryRow(ctx, `SELECT record_version FROM tenant_departments WHERE tenant_id = $1 AND department_id = $2`, tenantID, departmentID)
				if perr := probe.Scan(&currentVersion); perr != nil {
					if errors.Is(perr, pgx.ErrNoRows) {
						return domain.NewError(domain.ErrDepartmentNotFound, "department not found for tenant")
					}
					return perr
				}
				return domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict").WithDetails(map[string]any{
					"record_version": currentVersion,
				})
			}
			return err
		}
		out = td
		return nil
	})
	return out, err
}
