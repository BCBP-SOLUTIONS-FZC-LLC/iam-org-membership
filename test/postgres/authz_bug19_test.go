//go:build integration

// authz_bug19_test.go — BUG-19: I-8 (AuthZRepository.FindMembershipProjection)
// must return each department once, however many elevated tenant_roles the
// user holds. The old query joined tenant_roles and dept_memberships side by
// side, so N roles × M departments gave N×M rows; roles survived through
// array_agg(DISTINCT …) but json_agg repeated every department N times.
package postgres_test

import (
	"context"
	"sort"
	"testing"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBUG19_I8_DepartmentsNotMultipliedByRoles(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)

	// Elevated codes a tenant_roles row may hold (chk_tr_no_member bars 'member').
	elevated := []string{"tenant_admin", "tenant_owner", "tender_admin"}
	levels := []string{"preparator", "reviewer", "approver"}

	cases := []struct {
		name         string
		roles, depts int
	}{
		{"1role_3depts", 1, 3},
		{"2roles_3depts", 2, 3}, // regression: 6 before the fix
		{"3roles_2depts", 3, 2}, // regression: 6 before the fix
		{"0roles_3depts", 0, 3},
		{"2roles_0depts", 2, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			tenantID := seedTenant(t, ctx, rawPool, "bug19-"+tc.name)
			userID, memID := uuid.New(), uuid.New()

			_, err := rawPool.Exec(ctx,
				`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES ($1, $2, $3, 'active')`,
				memID, tenantID, userID)
			require.NoError(t, err)
			for _, rc := range elevated[:tc.roles] {
				_, err = rawPool.Exec(ctx,
					`INSERT INTO tenant_roles (tenant_id, user_id, tenant_membership_id, role_code, granted_by) VALUES ($1, $2, $3, $4, $2)`,
					tenantID, userID, memID, rc)
				require.NoError(t, err)
			}
			wantLevel := map[uuid.UUID]domain.DeptRole{}
			for i := 0; i < tc.depts; i++ {
				deptID := uuid.New()
				_, err = rawPool.Exec(ctx,
					`INSERT INTO tenant_departments (tenant_id, department_id, is_active) VALUES ($1, $2, true)`,
					tenantID, deptID)
				require.NoError(t, err)
				_, err = rawPool.Exec(ctx,
					`INSERT INTO dept_memberships (tenant_id, user_id, tenant_membership_id, department_id, role_level, granted_by) VALUES ($1, $2, $3, $4, $5, $2)`,
					tenantID, userID, memID, deptID, levels[i])
				require.NoError(t, err)
				wantLevel[deptID] = domain.DeptRole(levels[i])
			}

			proj, err := pgadapter.NewAuthZRepository(appPool).
				FindMembershipProjection(withTenant(ctx, tenantID), tenantID, userID)
			require.NoError(t, err)
			require.NotNil(t, proj)

			// Departments: exactly M, each once, ordered by department_id,
			// role_level unchanged, and an empty (non-nil) array for M = 0.
			require.NotNil(t, proj.Departments, "departments must be an empty array, not null")
			require.Len(t, proj.Departments, tc.depts, "one entry per department, not one per role × department")
			seen := map[uuid.UUID]int{}
			ids := make([]string, 0, len(proj.Departments))
			for _, d := range proj.Departments {
				seen[d.DepartmentID]++
				ids = append(ids, d.DepartmentID.String())
				assert.Equal(t, wantLevel[d.DepartmentID], d.RoleLevel, "role_level for %s", d.DepartmentID)
			}
			for id, n := range seen {
				assert.Equal(t, 1, n, "department %s must appear exactly once", id)
			}
			assert.True(t, sort.StringsAreSorted(ids), "departments ordered by department_id: %v", ids)

			// Roles: still deduplicated (array_agg DISTINCT). Order is the
			// tenant_role enum order, so compare as a set.
			require.Len(t, proj.Roles, tc.roles)
			got := make([]string, len(proj.Roles))
			for i, r := range proj.Roles {
				got[i] = string(r)
			}
			assert.ElementsMatch(t, elevated[:tc.roles], got)
		})
	}
}
