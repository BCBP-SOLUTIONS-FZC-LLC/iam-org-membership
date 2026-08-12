package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DepartmentRepository operates on the global departments catalog (no RLS).
// Operator-only writes via O-1/O-2; reads are unrestricted.
type DepartmentRepository struct {
	pool *pgcommon.Pool
}

var _ port.DepartmentRepository = (*DepartmentRepository)(nil)

func NewDepartmentRepository(pool *pgcommon.Pool) *DepartmentRepository {
	return &DepartmentRepository{pool: pool}
}

const departmentSelectColumns = `id, code, name, is_system, is_active, record_version, created_at, updated_at`

func scanDepartment(row pgx.Row) (*domain.Department, error) {
	var d domain.Department
	if err := row.Scan(&d.ID, &d.Code, &d.Name, &d.IsSystem, &d.IsActive, &d.RecordVersion, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	return &d, nil
}

func (r *DepartmentRepository) List(ctx context.Context, activeOnly bool) ([]domain.Department, error) {
	sql := `SELECT ` + departmentSelectColumns + ` FROM departments`
	if activeOnly {
		sql += ` WHERE is_active = true`
	}
	sql += ` ORDER BY code`

	var out []domain.Department
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDepartment(rows)
			if err != nil {
				return err
			}
			out = append(out, *d)
		}
		return rows.Err()
	})
	return out, err
}

func (r *DepartmentRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.Department, error) {
	var out *domain.Department
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+departmentSelectColumns+` FROM departments WHERE id = $1`, id)
		d, err := scanDepartment(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.ErrDepartmentNotFound, "department not found")
			}
			return err
		}
		out = d
		return nil
	})
	return out, err
}

func (r *DepartmentRepository) FindByCode(ctx context.Context, code string) (*domain.Department, error) {
	var out *domain.Department
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+departmentSelectColumns+` FROM departments WHERE code = $1`, code)
		d, err := scanDepartment(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.ErrDepartmentNotFound, "department not found")
			}
			return err
		}
		out = d
		return nil
	})
	return out, err
}

func (r *DepartmentRepository) Insert(ctx context.Context, d *domain.Department) (*domain.Department, error) {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	var out *domain.Department
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO departments (id, code, name, is_system, is_active)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING `+departmentSelectColumns,
			d.ID, d.Code, d.Name, d.IsSystem, d.IsActive)
		created, scanErr := scanDepartment(row)
		if scanErr != nil {
			var pgErr *pgconn.PgError
			if errors.As(scanErr, &pgErr) && pgErr.Code == "23505" {
				return domain.NewError(domain.ErrConflict, "department code already exists").
					WithDetails(map[string]any{"code": "duplicate_code"})
			}
			return scanErr
		}
		out = created
		return nil
	})
	return out, err
}

// Update applies name and/or is_active with optimistic locking. code and
// is_system are immutable at the DB layer (D-2/D-10 triggers).
func (r *DepartmentRepository) Update(ctx context.Context, id uuid.UUID, name *string, isActive *bool, expectedVersion int64) (*domain.Department, error) {
	if name == nil && isActive == nil {
		return r.FindByID(ctx, id)
	}
	// Build SET clause.
	args := []any{id, expectedVersion}
	set := ""
	if name != nil {
		args = append(args, *name)
		set = fmt.Sprintf(`name = $%d`, len(args))
	}
	if isActive != nil {
		args = append(args, *isActive)
		if set != "" {
			set += ", "
		}
		set += fmt.Sprintf(`is_active = $%d`, len(args))
	}
	sql := `UPDATE departments SET ` + set +
		` WHERE id = $1 AND record_version = $2 RETURNING ` + departmentSelectColumns

	var out *domain.Department
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, sql, args...)
		updated, err := scanDepartment(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Probe to distinguish 404 vs version conflict.
				var currentVersion int64
				probe := tx.QueryRow(ctx, `SELECT record_version FROM departments WHERE id = $1`, id)
				if perr := probe.Scan(&currentVersion); perr != nil {
					if errors.Is(perr, pgx.ErrNoRows) {
						return domain.NewError(domain.ErrDepartmentNotFound, "department not found")
					}
					return perr
				}
				return domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict").WithDetails(map[string]any{
					"record_version": currentVersion,
				})
			}
			return err
		}
		out = updated
		return nil
	})
	return out, err
}
