//go:build integration

// Phase 7 — Full-coverage service-layer sweep (final two services).
//
// Module:   iam-org-membership
// Feature:  Department catalog activation (P-3 / P-24 / P-25)
//   - Role label rename (P-12 / P-13)
//
// Files:    internal/core/service/department_service.go
//
//	internal/core/service/role_label_service.go
//
// Test-case metadata format per Reference_doc/Test_prompt.md.
// Test IDs: P7-DEPT-NNN, P7-LABEL-NNN.
package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═════════════════════════════════════════════════════════════════════════
// DepartmentService — P-3 ListForTenant
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P7-DEPT-001
// Module:            iam-org-membership · Department
// Feature:           P-3 · List active departments for tenant
// API:               GET /api/v1/tenants/{id}/departments
// Scenario:          Happy path — tenant with 3 activated depts
// Preconditions:     3 catalog depts activated for the tenant
// Test Steps:
//  1. Seed tenant + 3 depts + 3 tenant_departments rows (active)
//  2. Call ListForTenant
//
// Expected Result:
//   - Returns 3 TenantDepartmentView items with hydrated Code/Name
//   - All IsActive=true, RecordVersion=1
//
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP7Dept001_ListActive(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "dept-001")
	tctx := withSystemAndTenant(ctx, tenantID)

	dA := seedSystemDept(t, ctx, fx.CatalogDepts, "DEPT_A_001", "Dept A")
	dB := seedSystemDept(t, ctx, fx.CatalogDepts, "DEPT_B_001", "Dept B")
	dC := seedSystemDept(t, ctx, fx.CatalogDepts, "DEPT_C_001", "Dept C")
	activateDept(t, ctx, fx.rawPool, tenantID, dA)
	activateDept(t, ctx, fx.rawPool, tenantID, dB)
	activateDept(t, ctx, fx.rawPool, tenantID, dC)

	views, err := fx.Department.ListForTenant(tctx, tenantID)
	require.NoError(t, err)
	require.Len(t, views, 3)
	for _, v := range views {
		assert.True(t, v.IsActive)
		assert.NotEmpty(t, v.Code)
		assert.NotEmpty(t, v.Name)
	}
}

// Test Case ID:      P7-DEPT-002
// Module:            iam-org-membership · Department
// Feature:           P-3 · Empty tenant returns empty slice, not nil error
// API:               GET /api/v1/tenants/{id}/departments
// Scenario:          Positive — no depts activated
// Preconditions:     Fresh tenant, zero activations
// Test Steps:
//  1. Seed tenant with no activations
//  2. Call ListForTenant
//
// Expected Result:
//   - err == nil, returned slice has length 0 (and is non-nil)
//
// Priority:          P2
// Severity:          Minor
// Automation Status: Automated
func TestP7Dept002_ListEmptyReturnsEmptySlice(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "dept-002")
	tctx := withSystemAndTenant(ctx, tenantID)

	views, err := fx.Department.ListForTenant(tctx, tenantID)
	require.NoError(t, err)
	assert.Empty(t, views)
	assert.NotNil(t, views, "empty must be [] not nil")
}

// ═════════════════════════════════════════════════════════════════════════
// DepartmentService — P-24 Activate
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P7-DEPT-010
// Module:            iam-org-membership · Department
// Feature:           P-24 · Activate a catalog department for the tenant
// API:               POST /api/v1/tenants/{id}/departments
// Scenario:          Happy path — first activation
// Preconditions:     Catalog dept exists, tenant has no activation for it
// Test Steps:
//  1. Seed tenant + catalog dept
//  2. Call Activate(tenant, dept)
//
// Expected Result:
//   - Returns TenantDepartment with IsActive=true, RecordVersion=1
//   - tenant_departments row now exists
//
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP7Dept010_ActivateHappy(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "dept-010")
	deptID := seedSystemDept(t, ctx, fx.CatalogDepts, "DEPT_010", "Dept 010")
	tctx := withSystemAndTenant(ctx, tenantID)

	td, wasCreated, err := fx.Department.Activate(tctx, tenantID, deptID)
	require.NoError(t, err)
	require.NotNil(t, td)
	assert.True(t, wasCreated, "fresh activation must report wasCreated=true (→ 201)")
	assert.Equal(t, deptID, td.DepartmentID)
	assert.True(t, td.IsActive)
	assert.EqualValues(t, 1, td.RecordVersion)

	// tenant_departments row present.
	var n int
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM tenant_departments WHERE tenant_id = $1 AND department_id = $2`,
		tenantID, deptID).Scan(&n))
	assert.Equal(t, 1, n)
}

// Test Case ID:      P7-DEPT-011
// Module:            iam-org-membership · Department
// Feature:           P-24 · Idempotent re-activation
// API:               POST /api/v1/tenants/{id}/departments
// Scenario:          Positive — activating an already-active dept is idempotent
// Preconditions:     Dept already active
// Test Steps:
//  1. Activate twice
//
// Expected Result:
//   - Second call succeeds (no error), returned row still IsActive=true
//
// Priority:          P2
// Severity:          Major
// Automation Status: Automated
func TestP7Dept011_ActivateIdempotent(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "dept-011")
	deptID := seedSystemDept(t, ctx, fx.CatalogDepts, "DEPT_011", "Dept 011")
	tctx := withSystemAndTenant(ctx, tenantID)

	_, _, err := fx.Department.Activate(tctx, tenantID, deptID)
	require.NoError(t, err)
	// TD-7: second activate returns ErrDepartmentAlreadyActivated → HTTP 409.
	_, _, err2 := fx.Department.Activate(tctx, tenantID, deptID)
	require.Error(t, err2, "P-24 second activate must return ErrDepartmentAlreadyActivated (TD-7)")
	require.ErrorIs(t, err2, domain.ErrDepartmentAlreadyActivated)
}

// Test Case ID:      P7-DEPT-012
// Module:            iam-org-membership · Department
// Feature:           P-24 · Unknown department id
// API:               POST /api/v1/tenants/{id}/departments
// Scenario:          Negative — catalog dept doesn't exist
// Preconditions:     No catalog row for the id
// Test Steps:
//  1. Call Activate with a random dept uuid
//
// Expected Result:
//   - Returns error (FindByID → 404 department_not_found)
//   - tenant_departments row NOT created
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Dept012_ActivateUnknownDept(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "dept-012")
	tctx := withSystemAndTenant(ctx, tenantID)

	_, _, err := fx.Department.Activate(tctx, tenantID, uuid.New())
	require.Error(t, err, "unknown dept must error before FK violation")
}

// ═════════════════════════════════════════════════════════════════════════
// DepartmentService — P-25 SetActive
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P7-DEPT-020
// Module:            iam-org-membership · Department
// Feature:           P-25 · Deactivate a NON-system tenant department
// API:               PATCH /api/v1/tenants/{id}/departments/{dept_id}
// Scenario:          Happy path — flip is_active to false on a non-system dept
// Preconditions:     Dept is non-system and active
// Test Steps:
//  1. Seed tenant + non-system dept, activate it
//  2. Call SetActive(is_active=false, record_version=1)
//
// Expected Result:
//   - Returns updated row with IsActive=false, RecordVersion=2
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Dept020_DeactivateNonSystem(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "dept-020")
	deptID := seedNonSystemDept(t, ctx, fx.CatalogDepts, "DEPT_020", "Dept 020")
	activateDept(t, ctx, fx.rawPool, tenantID, deptID)
	tctx := withSystemAndTenant(ctx, tenantID)

	td, err := fx.Department.SetActive(tctx, tenantID, deptID, false, 1)
	require.NoError(t, err)
	assert.False(t, td.IsActive)
	assert.EqualValues(t, 2, td.RecordVersion)
}

// Test Case ID:      P7-DEPT-022
// Module:            iam-org-membership · Department
// Feature:           CONC-4 · SetActive optimistic-lock mismatch
// API:               PATCH /api/v1/tenants/{id}/departments/{dept_id}
// Scenario:          Negative — record_version stale
// Preconditions:     Dept active at v=1
// Test Steps:
//  1. Call SetActive with expectedVersion=999
//
// Expected Result:
//   - Returns ErrOptimisticLockConflict → HTTP 409
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Dept022_SetActiveOptimisticLock(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "dept-022")
	deptID := seedNonSystemDept(t, ctx, fx.CatalogDepts, "DEPT_022", "Dept 022")
	activateDept(t, ctx, fx.rawPool, tenantID, deptID)
	tctx := withSystemAndTenant(ctx, tenantID)

	_, err := fx.Department.SetActive(tctx, tenantID, deptID, false, 999)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "optimistic_lock_conflict", de.Code)
}

// Test Case ID:      P7-DEPT-023
// Module:            iam-org-membership · Department
// Feature:           P-25 · Reactivate a previously-deactivated dept
// API:               PATCH /api/v1/tenants/{id}/departments/{dept_id}
// Scenario:          Happy path — is_active=false → is_active=true round-trip
// Preconditions:     Non-system dept active at v=1
// Test Steps:
//  1. Deactivate (v=1 → v=2)
//  2. Reactivate with v=2 → v=3
//
// Expected Result:
//   - Second call returns IsActive=true, RecordVersion=3
//
// Priority:          P2
// Severity:          Major
// Automation Status: Automated
func TestP7Dept023_ReactivateAfterDeactivate(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "dept-023")
	deptID := seedNonSystemDept(t, ctx, fx.CatalogDepts, "DEPT_023", "Dept 023")
	activateDept(t, ctx, fx.rawPool, tenantID, deptID)
	tctx := withSystemAndTenant(ctx, tenantID)

	td, err := fx.Department.SetActive(tctx, tenantID, deptID, false, 1)
	require.NoError(t, err)
	require.EqualValues(t, 2, td.RecordVersion)

	td2, err := fx.Department.SetActive(tctx, tenantID, deptID, true, 2)
	require.NoError(t, err)
	assert.True(t, td2.IsActive)
	assert.EqualValues(t, 3, td2.RecordVersion)
}

// ═════════════════════════════════════════════════════════════════════════
// RoleLabelService — P-12 List
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P7-LABEL-001
// Module:            iam-org-membership · RoleLabel
// Feature:           P-12 · List dept role labels for tenant
// API:               GET /api/v1/tenants/{id}/roles
// Scenario:          Happy path — tenant has 3 seeded labels
// Preconditions:     3 labels seeded (preparator/reviewer/approver)
// Test Steps:
//  1. Seed 3 labels
//  2. Call List
//
// Expected Result:
//   - Returns 3 labels
//
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP7Label001_ListThreeLabels(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "label-001")
	seedThreeRoleLabels(t, ctx, fx.rawPool, tenantID)
	tctx := withSystemAndTenant(ctx, tenantID)

	labels, err := fx.RoleLabel.List(tctx, tenantID)
	require.NoError(t, err)
	assert.Len(t, labels, 3)
}

// Test Case ID:      P7-LABEL-002
// Module:            iam-org-membership · RoleLabel
// Feature:           P-12 · Empty tenant returns empty slice
// API:               GET /api/v1/tenants/{id}/roles
// Scenario:          Positive — tenant with no seeded labels
// Preconditions:     Fresh tenant, no labels
// Test Steps:
//  1. Call List
//
// Expected Result:
//   - Returns empty slice, no error
//
// Priority:          P2
// Severity:          Minor
// Automation Status: Automated
func TestP7Label002_ListEmpty(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "label-002")
	tctx := withSystemAndTenant(ctx, tenantID)

	labels, err := fx.RoleLabel.List(tctx, tenantID)
	require.NoError(t, err)
	assert.Empty(t, labels)
}

// ═════════════════════════════════════════════════════════════════════════
// RoleLabelService — P-13 Update
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P7-LABEL-010
// Module:            iam-org-membership · RoleLabel
// Feature:           P-13 · Rename dept role label
// API:               PATCH /api/v1/tenants/{id}/roles/{role_code}
// Scenario:          Happy path — rename approver → Buyer
// Preconditions:     Labels seeded at v=1
// Test Steps:
//  1. Seed labels
//  2. Call Update(role_code=approver, display_name="Buyer", version=1)
//
// Expected Result:
//   - Returns updated label with new DisplayName + RecordVersion=2
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Label010_UpdateHappy(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "label-010")
	seedThreeRoleLabels(t, ctx, fx.rawPool, tenantID)
	tctx := withSystemAndTenant(ctx, tenantID)

	l, err := fx.RoleLabel.Update(tctx, tenantID, "approver", "Buyer", 1)
	require.NoError(t, err)
	require.NotNil(t, l)
	assert.Equal(t, "Buyer", l.DisplayName)
	assert.EqualValues(t, 2, l.RecordVersion)
}

// Test Case ID:      P7-LABEL-011
// Module:            iam-org-membership · RoleLabel
// Feature:           TR-7 · 'member' cannot be a persistable role_code
// API:               PATCH /api/v1/tenants/{id}/roles/{role_code}
// Scenario:          Negative — role_code='member'
// Preconditions:     Labels seeded
// Test Steps:
//  1. Call Update with role_code='member'
//
// Expected Result:
//   - Returns ErrValidation with details.code=invalid_role
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Label011_RejectMemberRole(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "label-011")
	tctx := withSystemAndTenant(ctx, tenantID)

	_, err := fx.RoleLabel.Update(tctx, tenantID, "member", "Whatever", 1)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "invalid_role", de.Details["code"])
}

// Test Case ID:      P7-LABEL-012
// Module:            iam-org-membership · RoleLabel
// Feature:           P-13 · Unknown role_code rejected
// API:               PATCH /api/v1/tenants/{id}/roles/{role_code}
// Scenario:          Negative — role_code='intern'
// Preconditions:     Labels seeded
// Test Steps:
//  1. Call Update with role_code='intern'
//
// Expected Result:
//   - Returns ErrValidation with details.code=invalid_role
//
// Priority:          P2
// Severity:          Major
// Automation Status: Automated
func TestP7Label012_RejectUnknownRoleCode(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "label-012")
	tctx := withSystemAndTenant(ctx, tenantID)

	_, err := fx.RoleLabel.Update(tctx, tenantID, "intern", "Intern", 1)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "invalid_role", de.Details["code"])
}

// Test Case ID:      P7-LABEL-013
// Module:            iam-org-membership · RoleLabel
// Feature:           P-13 · display_name must be non-empty
// API:               PATCH /api/v1/tenants/{id}/roles/{role_code}
// Scenario:          Negative — display_name=""
// Preconditions:     Labels seeded
// Test Steps:
//  1. Call Update with display_name=""
//
// Expected Result:
//   - Returns ErrValidation ("display_name must not be empty")
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Label013_RejectEmptyDisplayName(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "label-013")
	seedThreeRoleLabels(t, ctx, fx.rawPool, tenantID)
	tctx := withSystemAndTenant(ctx, tenantID)

	_, err := fx.RoleLabel.Update(tctx, tenantID, "approver", "", 1)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "validation_error", de.Code)
}

// Test Case ID:      P7-LABEL-014
// Module:            iam-org-membership · RoleLabel
// Feature:           CONC-4 · Optimistic-lock mismatch on Update
// API:               PATCH /api/v1/tenants/{id}/roles/{role_code}
// Scenario:          Negative — record_version stale
// Preconditions:     Label at v=1
// Test Steps:
//  1. Call Update with expectedVersion=999
//
// Expected Result:
//   - Returns ErrOptimisticLockConflict
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Label014_OptimisticLockMismatch(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "label-014")
	seedThreeRoleLabels(t, ctx, fx.rawPool, tenantID)
	tctx := withSystemAndTenant(ctx, tenantID)

	_, err := fx.RoleLabel.Update(tctx, tenantID, "approver", "Buyer", 999)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "optimistic_lock_conflict", de.Code)
}

// Test Case ID:      P7-LABEL-015
// Module:            iam-org-membership · RoleLabel
// Feature:           P-13 · Boundary — unicode + very long display name (256 chars)
// API:               PATCH /api/v1/tenants/{id}/roles/{role_code}
// Scenario:          Boundary — unicode payload, long-but-under-limit
// Preconditions:     Labels seeded
// Test Steps:
//  1. Call Update with display_name = 256-char unicode
//
// Expected Result:
//   - Succeeds (schema has no explicit LENGTH cap; only NOT EMPTY)
//   - Stored value round-trips
//
// Priority:          P3
// Severity:          Minor
// Automation Status: Automated
func TestP7Label015_UnicodeLongDisplayName(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "label-015")
	seedThreeRoleLabels(t, ctx, fx.rawPool, tenantID)
	tctx := withSystemAndTenant(ctx, tenantID)

	unicode := strings.Repeat("Ĥėļŀő世界🌍", 20) // ~200 utf-8 code points
	l, err := fx.RoleLabel.Update(tctx, tenantID, "reviewer", unicode, 1)
	require.NoError(t, err)
	assert.Equal(t, unicode, l.DisplayName)
}

// ═════════════════════════════════════════════════════════════════════════
// helpers
// ═════════════════════════════════════════════════════════════════════════

// seedNonSystemDept registers a non-system (is_system=false) department (so
// P-25 deactivation is legal, D-9/D-11 doesn't block). The departments table
// was dropped (migration-runbook Phase 4 — LLD §12 step 4); catalog is nil
// for tests that construct repos directly without a fixture.
func seedNonSystemDept(t *testing.T, ctx context.Context, catalog *fakeCatalogDepartments, code, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if catalog != nil {
		catalog.add(domain.Department{ID: id, Code: code, Name: name, IsSystem: false, IsActive: true})
	}
	return id
}

// seedThreeRoleLabels inserts the standard 3 dept-role labels for a tenant.
func seedThreeRoleLabels(t *testing.T, ctx context.Context, rawPool *pgxpoolPool, tenantID uuid.UUID) {
	t.Helper()
	rows := [][]string{
		{"preparator", "Preparator"},
		{"reviewer", "Reviewer"},
		{"approver", "Approver"},
	}
	for _, r := range rows {
		_, err := rawPool.Exec(ctx,
			`INSERT INTO dept_role_labels (id, tenant_id, role_code, display_name)
			 VALUES (gen_random_uuid(), $1, $2, $3)`,
			tenantID, r[0], r[1])
		require.NoError(t, err)
	}
}
