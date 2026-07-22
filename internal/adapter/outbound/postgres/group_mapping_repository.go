package postgres

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type GroupMappingRepository struct {
	pool *pgcommon.Pool
}

var _ port.GroupMappingRepository = (*GroupMappingRepository)(nil)

func NewGroupMappingRepository(pool *pgcommon.Pool) *GroupMappingRepository {
	return &GroupMappingRepository{pool: pool}
}

const gdrmCols = `id, tenant_id, keycloak_group_name, role_code, record_version, created_at, updated_at`
const gtrmCols = `id, tenant_id, keycloak_group_name, role_code, record_version, created_at, updated_at`
const gdmCols = `id, tenant_id, keycloak_group_name, department_id, record_version, created_at, updated_at`

// ── GroupDeptRoleMapping (P-14/P-15) ────────────────────────────────────

func (r *GroupMappingRepository) ListDeptRoleMappings(ctx context.Context, tenantID uuid.UUID) ([]domain.GroupDeptRoleMapping, error) {
	var out []domain.GroupDeptRoleMapping
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+gdrmCols+` FROM group_dept_role_mappings WHERE tenant_id = $1 ORDER BY keycloak_group_name`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m domain.GroupDeptRoleMapping
			var code string
			if err := rows.Scan(&m.ID, &m.TenantID, &m.KeycloakGroupName, &code, &m.RecordVersion, &m.CreatedAt, &m.UpdatedAt); err != nil {
				return err
			}
			m.RoleCode = domain.DeptRole(code)
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// ReplaceDeptRoleMappings is P-15 full-replacement.
func (r *GroupMappingRepository) ReplaceDeptRoleMappings(ctx context.Context, tenantID uuid.UUID, desired []domain.GroupDeptRoleMapping) ([]domain.GroupDeptRoleMapping, error) {
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM group_dept_role_mappings WHERE tenant_id = $1`, tenantID); err != nil {
			return err
		}
		for _, m := range desired {
			if _, err := tx.Exec(ctx, `
				INSERT INTO group_dept_role_mappings (id, tenant_id, keycloak_group_name, role_code)
				VALUES (gen_random_uuid(), $1, $2, $3)`,
				tenantID, m.KeycloakGroupName, string(m.RoleCode)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return r.ListDeptRoleMappings(ctx, tenantID)
}

// ── GroupTenantRoleMapping (P-29) ───────────────────────────────────────

func (r *GroupMappingRepository) ListTenantRoleMappings(ctx context.Context, tenantID uuid.UUID) ([]domain.GroupTenantRoleMapping, error) {
	var out []domain.GroupTenantRoleMapping
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+gtrmCols+` FROM group_tenant_role_mappings WHERE tenant_id = $1 ORDER BY keycloak_group_name`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m domain.GroupTenantRoleMapping
			var code string
			if err := rows.Scan(&m.ID, &m.TenantID, &m.KeycloakGroupName, &code, &m.RecordVersion, &m.CreatedAt, &m.UpdatedAt); err != nil {
				return err
			}
			m.RoleCode = domain.TenantRoleCode(code)
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

func (r *GroupMappingRepository) ReplaceTenantRoleMappings(ctx context.Context, tenantID uuid.UUID, desired []domain.GroupTenantRoleMapping) ([]domain.GroupTenantRoleMapping, error) {
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM group_tenant_role_mappings WHERE tenant_id = $1`, tenantID); err != nil {
			return err
		}
		for _, m := range desired {
			if _, err := tx.Exec(ctx, `
				INSERT INTO group_tenant_role_mappings (id, tenant_id, keycloak_group_name, role_code)
				VALUES (gen_random_uuid(), $1, $2, $3)`,
				tenantID, m.KeycloakGroupName, string(m.RoleCode)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return r.ListTenantRoleMappings(ctx, tenantID)
}

// ── GroupDeptMapping (P-16/P-17) ────────────────────────────────────────

func (r *GroupMappingRepository) ListDeptMappings(ctx context.Context, tenantID uuid.UUID) ([]domain.GroupDeptMapping, error) {
	var out []domain.GroupDeptMapping
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+gdmCols+` FROM group_dept_mappings WHERE tenant_id = $1 ORDER BY keycloak_group_name, department_id`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m domain.GroupDeptMapping
			if err := rows.Scan(&m.ID, &m.TenantID, &m.KeycloakGroupName, &m.DepartmentID, &m.RecordVersion, &m.CreatedAt, &m.UpdatedAt); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

func (r *GroupMappingRepository) ReplaceDeptMappings(ctx context.Context, tenantID uuid.UUID, desired []domain.GroupDeptMapping) ([]domain.GroupDeptMapping, error) {
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM group_dept_mappings WHERE tenant_id = $1`, tenantID); err != nil {
			return err
		}
		for _, m := range desired {
			if _, err := tx.Exec(ctx, `
				INSERT INTO group_dept_mappings (id, tenant_id, keycloak_group_name, department_id)
				VALUES (gen_random_uuid(), $1, $2, $3)`,
				tenantID, m.KeycloakGroupName, m.DepartmentID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return r.ListDeptMappings(ctx, tenantID)
}
