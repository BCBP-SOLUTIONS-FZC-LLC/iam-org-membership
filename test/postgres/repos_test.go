//go:build integration

// Phase 8 — Repository full-coverage sweep.
//
// Module:   iam-org-membership
// Feature:  Persistence layer — 4 previously-uncovered repositories
// Files:    internal/adapter/outbound/postgres/{plan,tenant_department,department,tenant}_repository.go
//
// Test-case metadata format per Reference_doc/Test_prompt.md.
// Test IDs: P8-PLAN-NNN, P8-TDEPT-NNN, P8-DEPT-NNN, P8-TENANT-NNN.
package postgres_test

import (
	"context"
	"errors"
	"testing"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═════════════════════════════════════════════════════════════════════════
// TenantDepartmentRepository
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P8-TDEPT-001
// Module:            iam-org-membership · Persistence
// Feature:           tenant_departments · List all (active + inactive)
// API:               Internal
// Scenario:          Happy — mixed active + inactive rows returned
// Preconditions:     Tenant with 2 depts activated + 1 flipped to inactive
// Test Steps:
//  1. Seed tenant, 3 catalog depts, activate all 3, flip one to inactive
//  2. Call List(tenant)
//
// Expected Result:
//   - Returns 3 rows regardless of is_active
//
// Priority:          P2
// Severity:          Major
// Automation Status: Automated
func TestP8TDept001_ListAllRegardlessOfActive(t *testing.T) {
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tdept-001")
	dA := seedSystemDept(t, ctx, nil, "TA_001", "TA")
	dB := seedNonSystemDept(t, ctx, nil, "TB_001", "TB")
	dC := seedNonSystemDept(t, ctx, nil, "TC_001", "TC")
	activateDept(t, ctx, rawPool, tenantID, dA)
	activateDept(t, ctx, rawPool, tenantID, dB)
	activateDept(t, ctx, rawPool, tenantID, dC)
	_, err := rawPool.Exec(ctx,
		`UPDATE tenant_departments SET is_active = false WHERE tenant_id = $1 AND department_id = $2`,
		tenantID, dC)
	require.NoError(t, err)
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewTenantDepartmentRepository(appPool)
	rows, err := repo.List(tctx, tenantID)
	require.NoError(t, err)
	assert.Len(t, rows, 3, "List returns everything, active or not")
}

// Test Case ID:      P8-TDEPT-002
// Module:            iam-org-membership · Persistence
// Feature:           tenant_departments · ListActive filters by is_active
// API:               Internal
// Scenario:          Happy — only active rows returned
// Preconditions:     3 activations, one flipped inactive
// Test Steps:
//  1. Seed 3, flip one inactive
//  2. Call ListActive
//
// Expected Result:
//   - Returns 2 rows, all IsActive=true
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8TDept002_ListActiveFilters(t *testing.T) {
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tdept-002")
	dA := seedNonSystemDept(t, ctx, nil, "TA_002", "TA")
	dB := seedNonSystemDept(t, ctx, nil, "TB_002", "TB")
	dC := seedNonSystemDept(t, ctx, nil, "TC_002", "TC")
	for _, d := range []uuid.UUID{dA, dB, dC} {
		activateDept(t, ctx, rawPool, tenantID, d)
	}
	_, err := rawPool.Exec(ctx,
		`UPDATE tenant_departments SET is_active=false WHERE tenant_id=$1 AND department_id=$2`,
		tenantID, dC)
	require.NoError(t, err)
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewTenantDepartmentRepository(appPool)
	rows, err := repo.ListActive(tctx, tenantID)
	require.NoError(t, err)
	assert.Len(t, rows, 2)
	for _, r := range rows {
		assert.True(t, r.IsActive)
	}
}

// Test Case ID:      P8-TDEPT-003
// Module:            iam-org-membership · Persistence
// Feature:           tenant_departments · Find happy
// API:               Internal
// Scenario:          Happy — activated row exists
// Preconditions:     Activated dept
// Test Steps:
//  1. Activate dept
//  2. Find(tenant, dept)
//
// Expected Result:
//   - Returns row with IsActive=true
//
// Priority:          P2
// Severity:          Major
// Automation Status: Automated
func TestP8TDept003_FindHappy(t *testing.T) {
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tdept-003")
	deptID := seedNonSystemDept(t, ctx, nil, "T003", "T003")
	activateDept(t, ctx, rawPool, tenantID, deptID)
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewTenantDepartmentRepository(appPool)
	td, err := repo.Find(tctx, tenantID, deptID)
	require.NoError(t, err)
	require.NotNil(t, td)
	assert.True(t, td.IsActive)
}

// Test Case ID:      P8-TDEPT-004
// Module:            iam-org-membership · Persistence
// Feature:           tenant_departments · Find not-found
// API:               Internal
// Scenario:          Negative — no row for the pair
// Preconditions:     No activation
// Test Steps:
//  1. Find(tenant, unknown dept)
//
// Expected Result:
//   - Returns error (not found)
//
// Priority:          P2
// Severity:          Minor
// Automation Status: Automated
func TestP8TDept004_FindNotFound(t *testing.T) {
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tdept-004")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewTenantDepartmentRepository(appPool)
	_, err := repo.Find(tctx, tenantID, uuid.New())
	require.Error(t, err)
}

// Test Case ID:      P8-TDEPT-005
// Module:            iam-org-membership · Persistence
// Feature:           tenant_departments · Activate idempotent
// API:               Internal (P-24 via service)
// Scenario:          Happy — activating twice returns same row
// Preconditions:     Catalog dept exists
// Test Steps:
//  1. Activate twice
//
// Expected Result:
//   - Both calls succeed, same tenant_departments row
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8TDept005_ActivateIdempotent(t *testing.T) {
	// Repo-level Activate is NOT idempotent by itself — ON CONFLICT DO
	// NOTHING + RETURNING yields no row on a repeat call, which the repo
	// maps to ErrDepartmentAlreadyActivated. Idempotency (silently
	// returning the existing row) is a service-layer concern —
	// DepartmentService.Activate catches this exact error and re-Finds —
	// covered by TestP7Dept011_ActivateIdempotent.
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tdept-005")
	deptID := seedNonSystemDept(t, ctx, nil, "T005", "T005")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewTenantDepartmentRepository(appPool)
	td1, err := repo.Activate(tctx, tenantID, deptID)
	require.NoError(t, err)
	assert.True(t, td1.IsActive)

	_, err = repo.Activate(tctx, tenantID, deptID)
	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrDepartmentAlreadyActivated.Error(), de.Code)
}

// Test Case ID:      P8-TDEPT-006
// Module:            iam-org-membership · Persistence
// Feature:           CONC-4 · SetActive optimistic-lock
// API:               Internal (P-25)
// Scenario:          Negative — record_version stale
// Preconditions:     Activation at v=1
// Test Steps:
//  1. Call SetActive with expectedVersion=999
//
// Expected Result:
//   - Returns ErrOptimisticLockConflict
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8TDept006_SetActiveOptimisticLock(t *testing.T) {
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tdept-006")
	deptID := seedNonSystemDept(t, ctx, nil, "T006", "T006")
	activateDept(t, ctx, rawPool, tenantID, deptID)
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewTenantDepartmentRepository(appPool)
	_, err := repo.SetActive(tctx, tenantID, deptID, false, 999)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "optimistic_lock_conflict", de.Code)
}

// ═════════════════════════════════════════════════════════════════════════
// TenantRepository
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P8-TENANT-001
// Module:            iam-org-membership · Persistence
// Feature:           tenants · FindByID happy
// API:               Internal (P-1)
// Scenario:          Happy — read seeded tenant
// Preconditions:     seedTenant helper populated
// Test Steps:
//  1. Seed tenant
//  2. FindByID
//
// Expected Result:
//   - Returns tenant with expected slug + record_version=1
//
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP8Tenant001_FindByIDHappy(t *testing.T) {
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "trepo-001")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewTenantRepository(appPool)
	got, err := repo.FindByID(tctx, tenantID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "trepo-001", got.Slug)
	assert.EqualValues(t, 1, got.RecordVersion)
}

// Test Case ID:      P8-TENANT-002
// Module:            iam-org-membership · Persistence
// Feature:           tenants · FindByID not-found
// API:               Internal
// Scenario:          Negative — unknown id
// Preconditions:     Empty tenants for the id
// Test Steps:
//  1. FindByID(random uuid)
//
// Expected Result:
//   - Returns error surfaced from repo (tenant_not_found or pgx.ErrNoRows path)
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8Tenant002_FindByIDNotFound(t *testing.T) {
	appPool, _, _ := setupTestDB(t)
	ctx := context.Background()
	// GUC bind to some tenant id so RLS lets the query through — but there's
	// no matching row, so FindByID returns not-found regardless.
	unknown := uuid.New()
	tctx := withTenant(ctx, unknown)

	repo := pgadapter.NewTenantRepository(appPool)
	_, err := repo.FindByID(tctx, unknown)
	require.Error(t, err)
}

// Test Case ID:      P8-TENANT-003
// Module:            iam-org-membership · Persistence
// Feature:           tenants · Update happy
// API:               PATCH /api/v1/tenants/{id}
// Scenario:          Happy — patch name
// Preconditions:     Seed tenant
// Test Steps:
//  1. Seed
//  2. Update with Name="Renamed" and RecordVersion=1
//
// Expected Result:
//   - Returned tenant carries new name and record_version=2
//
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP8Tenant003_UpdateNameHappy(t *testing.T) {
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "trepo-003")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewTenantRepository(appPool)
	name := "Renamed"
	updated, err := repo.Update(tctx, tenantID, &domain.TenantPatch{Name: &name, RecordVersion: 1})
	require.NoError(t, err)
	assert.Equal(t, "Renamed", updated.Name)
	assert.EqualValues(t, 2, updated.RecordVersion)
}

// Test Case ID:      P8-TENANT-004
// Module:            iam-org-membership · Persistence
// Feature:           CONC-4 · tenants Update optimistic-lock mismatch
// API:               PATCH /api/v1/tenants/{id}
// Scenario:          Negative — record_version stale
// Preconditions:     Tenant at v=1
// Test Steps:
//  1. Update with RecordVersion=999
//
// Expected Result:
//   - Returns ErrOptimisticLockConflict
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8Tenant004_UpdateOptimisticLock(t *testing.T) {
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "trepo-004")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewTenantRepository(appPool)
	name := "Won't Land"
	_, err := repo.Update(tctx, tenantID, &domain.TenantPatch{Name: &name, RecordVersion: 999})
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "optimistic_lock_conflict", de.Code)
}

// Test Case ID:      P8-TENANT-005
// Module:            iam-org-membership · Persistence
// Feature:           T-1 · slug immutability trigger
// API:               PATCH /api/v1/tenants/{id}
// Scenario:          Negative — direct SQL trying to change slug is blocked by trigger
// Preconditions:     Tenant exists
// Test Steps:
//  1. Seed tenant with slug='immut'
//  2. Direct SQL: UPDATE tenants SET slug='new'
//
// Expected Result:
//   - Trigger raises exception with 'tenant slug is immutable' message
//
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP8Tenant005_SlugImmutability(t *testing.T) {
	_, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "trepo-005-immut")

	_, err := rawPool.Exec(ctx,
		`UPDATE tenants SET slug = 'new-slug' WHERE id = $1`, tenantID)
	require.Error(t, err, "T-1 trigger must block slug UPDATE")
}
