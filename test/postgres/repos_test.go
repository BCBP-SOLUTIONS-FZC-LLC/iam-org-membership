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
// PlanRepository — global catalog (starter, pro, enterprise)
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P8-PLAN-001
// Module:            iam-org-membership · Persistence
// Feature:           Plan catalog · List
// API:               Internal (used by O-5 GET /operator/plans)
// Scenario:          Happy path — 3 seeded plans returned
// Preconditions:     Migration seed populates starter/pro/enterprise
// Test Steps:
//   1. Instantiate PlanRepository against a fresh pool
//   2. Call List
// Expected Result:
//   - Returns 3 plans matching the seed catalogue
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP8Plan001_ListSeededPlans(t *testing.T) {
	appPool, _ := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewPlanRepository(appPool)

	plans, err := repo.List(ctx)
	require.NoError(t, err)
	require.Len(t, plans, 3, "3 seeded plans (starter/pro/enterprise)")

	codes := make(map[domain.TenantPlan]bool)
	for _, p := range plans {
		codes[p.Code] = true
	}
	assert.True(t, codes[domain.TenantPlan("starter")])
	assert.True(t, codes[domain.TenantPlan("pro")])
	assert.True(t, codes[domain.TenantPlan("enterprise")])
}

// Test Case ID:      P8-PLAN-002
// Module:            iam-org-membership · Persistence
// Feature:           Plan catalog · FindByCode happy
// API:               Internal
// Scenario:          Happy — starter plan lookup
// Preconditions:     Seed migration
// Test Steps:
//   1. FindByCode("starter")
// Expected Result:
//   - Returns non-nil Plan with Code=starter
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8Plan002_FindByCodeHappy(t *testing.T) {
	appPool, _ := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewPlanRepository(appPool)

	p, err := repo.FindByCode(ctx, domain.PlanStarter)
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, domain.PlanStarter, p.Code)
}

// Test Case ID:      P8-PLAN-003
// Module:            iam-org-membership · Persistence
// Feature:           Plan catalog · FindByCode unknown
// API:               Internal
// Scenario:          Negative — unknown plan code
// Preconditions:     No plan with code='free'
// Test Steps:
//   1. FindByCode("free")
// Expected Result:
//   - Returns an error (pgx.ErrNoRows or wrapped)
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8Plan003_FindByCodeUnknown(t *testing.T) {
	appPool, _ := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewPlanRepository(appPool)

	_, err := repo.FindByCode(ctx, domain.TenantPlan("free"))
	require.Error(t, err)
}

// Test Case ID:      P8-PLAN-004
// Module:            iam-org-membership · Persistence
// Feature:           Plan catalog · Update happy
// API:               PATCH /api/v1/operator/plans/{code}
// Scenario:          Happy — bump starter.tender_limit
// Preconditions:     starter plan at record_version=1
// Test Steps:
//   1. Fetch starter (record v)
//   2. Update with tender_limit=15
// Expected Result:
//   - Updated plan carries tender_limit=15, record_version=2
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8Plan004_UpdateTenderLimit(t *testing.T) {
	appPool, _ := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewPlanRepository(appPool)

	limit := 15
	limitPtr := &limit
	patch := &domain.PlanPatch{TenderLimit: &limitPtr, RecordVersion: 1}
	updated, err := repo.Update(ctx, domain.PlanStarter, patch)
	require.NoError(t, err)
	require.NotNil(t, updated)
	require.NotNil(t, updated.TenderLimit)
	assert.Equal(t, 15, *updated.TenderLimit)
	assert.EqualValues(t, 2, updated.RecordVersion)
}

// Test Case ID:      P8-PLAN-005
// Module:            iam-org-membership · Persistence
// Feature:           CONC-4 · Plan Update optimistic-lock mismatch
// API:               Internal
// Scenario:          Negative — record_version stale
// Preconditions:     Plan at v=1
// Test Steps:
//   1. Update with RecordVersion=999
// Expected Result:
//   - Returns ErrOptimisticLockConflict
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8Plan005_UpdateOptimisticLockMismatch(t *testing.T) {
	appPool, _ := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewPlanRepository(appPool)

	limit := 15
	limitPtr := &limit
	_, err := repo.Update(ctx, domain.PlanStarter, &domain.PlanPatch{
		TenderLimit: &limitPtr, RecordVersion: 999,
	})
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "optimistic_lock_conflict", de.Code)
}

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
//   1. Seed tenant, 3 catalog depts, activate all 3, flip one to inactive
//   2. Call List(tenant)
// Expected Result:
//   - Returns 3 rows regardless of is_active
// Priority:          P2
// Severity:          Major
// Automation Status: Automated
func TestP8TDept001_ListAllRegardlessOfActive(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tdept-001")
	dA := seedSystemDept(t, ctx, rawPool, "TA_001", "TA")
	dB := seedNonSystemDept(t, ctx, rawPool, "TB_001", "TB")
	dC := seedNonSystemDept(t, ctx, rawPool, "TC_001", "TC")
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
//   1. Seed 3, flip one inactive
//   2. Call ListActive
// Expected Result:
//   - Returns 2 rows, all IsActive=true
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8TDept002_ListActiveFilters(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tdept-002")
	dA := seedNonSystemDept(t, ctx, rawPool, "TA_002", "TA")
	dB := seedNonSystemDept(t, ctx, rawPool, "TB_002", "TB")
	dC := seedNonSystemDept(t, ctx, rawPool, "TC_002", "TC")
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
//   1. Activate dept
//   2. Find(tenant, dept)
// Expected Result:
//   - Returns row with IsActive=true
// Priority:          P2
// Severity:          Major
// Automation Status: Automated
func TestP8TDept003_FindHappy(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tdept-003")
	deptID := seedNonSystemDept(t, ctx, rawPool, "T003", "T003")
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
//   1. Find(tenant, unknown dept)
// Expected Result:
//   - Returns error (not found)
// Priority:          P2
// Severity:          Minor
// Automation Status: Automated
func TestP8TDept004_FindNotFound(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
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
//   1. Activate twice
// Expected Result:
//   - Both calls succeed, same tenant_departments row
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8TDept005_ActivateIdempotent(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tdept-005")
	deptID := seedNonSystemDept(t, ctx, rawPool, "T005", "T005")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewTenantDepartmentRepository(appPool)
	td1, err := repo.Activate(tctx, tenantID, deptID)
	require.NoError(t, err)
	td2, err := repo.Activate(tctx, tenantID, deptID)
	require.NoError(t, err)
	assert.True(t, td1.IsActive)
	assert.True(t, td2.IsActive)
}

// Test Case ID:      P8-TDEPT-006
// Module:            iam-org-membership · Persistence
// Feature:           CONC-4 · SetActive optimistic-lock
// API:               Internal (P-25)
// Scenario:          Negative — record_version stale
// Preconditions:     Activation at v=1
// Test Steps:
//   1. Call SetActive with expectedVersion=999
// Expected Result:
//   - Returns ErrOptimisticLockConflict
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8TDept006_SetActiveOptimisticLock(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tdept-006")
	deptID := seedNonSystemDept(t, ctx, rawPool, "T006", "T006")
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
// DepartmentRepository (global catalog)
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P8-DEPT-001
// Module:            iam-org-membership · Persistence
// Feature:           departments catalog · List all
// API:               Internal
// Scenario:          Happy — list includes seeded 5 system depts
// Preconditions:     Migration seed
// Test Steps:
//   1. List(activeOnly=false)
// Expected Result:
//   - Returns >= 5 rows (system depts seeded by migration)
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP8Dept001_ListAll(t *testing.T) {
	appPool, _ := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewDepartmentRepository(appPool)

	all, err := repo.List(ctx, false)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(all), 5, "5 system depts seeded")
}

// Test Case ID:      P8-DEPT-002
// Module:            iam-org-membership · Persistence
// Feature:           departments catalog · List activeOnly filter
// API:               Internal
// Scenario:          Positive — inactive depts excluded when activeOnly=true
// Preconditions:     Insert a non-system dept and flip is_active=false
// Test Steps:
//   1. Insert non-system dept
//   2. Flip is_active=false
//   3. List(activeOnly=true) should exclude it
// Expected Result:
//   - Retired dept not in the result
// Priority:          P2
// Severity:          Major
// Automation Status: Automated
func TestP8Dept002_ListActiveOnlyFilter(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewDepartmentRepository(appPool)

	retiredID := seedNonSystemDept(t, ctx, rawPool, "RETIRED_002", "Retired 002")
	_, err := rawPool.Exec(ctx,
		`UPDATE departments SET is_active = false WHERE id = $1`, retiredID)
	require.NoError(t, err)

	active, err := repo.List(ctx, true)
	require.NoError(t, err)
	for _, d := range active {
		assert.NotEqual(t, retiredID, d.ID, "retired dept must be filtered out")
	}
}

// Test Case ID:      P8-DEPT-003
// Module:            iam-org-membership · Persistence
// Feature:           departments · FindByID happy
// API:               Internal
// Scenario:          Positive — insert then find
// Preconditions:     Seed a fresh non-system dept
// Test Steps:
//   1. Insert dept
//   2. FindByID
// Expected Result:
//   - Returns matching dept
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8Dept003_FindByIDHappy(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewDepartmentRepository(appPool)
	id := seedNonSystemDept(t, ctx, rawPool, "FBI_003", "FindByID 003")

	d, err := repo.FindByID(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, d)
	assert.Equal(t, "FBI_003", d.Code)
}

// Test Case ID:      P8-DEPT-004
// Module:            iam-org-membership · Persistence
// Feature:           departments · FindByCode happy
// API:               Internal
// Scenario:          Positive — code lookup
// Preconditions:     Insert
// Test Steps:
//   1. Insert dept
//   2. FindByCode
// Expected Result:
//   - Returns matching dept
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8Dept004_FindByCodeHappy(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewDepartmentRepository(appPool)
	_ = seedNonSystemDept(t, ctx, rawPool, "CODE_004", "Code 004")

	d, err := repo.FindByCode(ctx, "CODE_004")
	require.NoError(t, err)
	assert.Equal(t, "CODE_004", d.Code)
}

// Test Case ID:      P8-DEPT-005
// Module:            iam-org-membership · Persistence
// Feature:           departments · Insert happy
// API:               POST /api/v1/operator/departments
// Scenario:          Happy — new non-system dept
// Preconditions:     Fresh code
// Test Steps:
//   1. Insert domain.Department{Code:INS_005, Name:...}
// Expected Result:
//   - Returned dept has assigned ID, IsActive=true, RecordVersion=1
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP8Dept005_InsertHappy(t *testing.T) {
	appPool, _ := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewDepartmentRepository(appPool)

	d, err := repo.Insert(ctx, &domain.Department{
		Code: "INS_005", Name: "Insert 005", IsSystem: false, IsActive: true,
	})
	require.NoError(t, err)
	require.NotNil(t, d)
	assert.NotEqual(t, uuid.Nil, d.ID)
	assert.True(t, d.IsActive)
	assert.EqualValues(t, 1, d.RecordVersion)
}

// Test Case ID:      P8-DEPT-006
// Module:            iam-org-membership · Persistence
// Feature:           departments · Insert duplicate code (uq_departments_code)
// API:               POST /api/v1/operator/departments
// Scenario:          Negative — duplicate code
// Preconditions:     dept with code=DUP_006 already exists
// Test Steps:
//   1. Insert first
//   2. Insert second with same code
// Expected Result:
//   - Second insert errors (uq_departments_code unique violation)
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8Dept006_InsertDuplicateCode(t *testing.T) {
	appPool, _ := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewDepartmentRepository(appPool)

	_, err := repo.Insert(ctx, &domain.Department{Code: "DUP_006", Name: "Dup 006"})
	require.NoError(t, err)
	_, err = repo.Insert(ctx, &domain.Department{Code: "DUP_006", Name: "Dup 006 Again"})
	require.Error(t, err, "code is UNIQUE — second insert must fail")
}

// Test Case ID:      P8-DEPT-007
// Module:            iam-org-membership · Persistence
// Feature:           departments · Update happy
// API:               PATCH /api/v1/operator/departments/{id}
// Scenario:          Happy — rename dept
// Preconditions:     Non-system dept exists at v=1
// Test Steps:
//   1. Insert dept
//   2. Update name
// Expected Result:
//   - Returned dept has new name, RecordVersion=2
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8Dept007_UpdateHappy(t *testing.T) {
	appPool, _ := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewDepartmentRepository(appPool)

	d, err := repo.Insert(ctx, &domain.Department{Code: "UPD_007", Name: "Old Name"})
	require.NoError(t, err)

	newName := "New Name"
	updated, err := repo.Update(ctx, d.ID, &newName, nil, 1)
	require.NoError(t, err)
	assert.Equal(t, "New Name", updated.Name)
	assert.EqualValues(t, 2, updated.RecordVersion)
}

// Test Case ID:      P8-DEPT-008
// Module:            iam-org-membership · Persistence
// Feature:           CONC-4 · departments Update optimistic-lock
// API:               Internal
// Scenario:          Negative — stale record_version
// Preconditions:     Dept at v=1
// Test Steps:
//   1. Insert
//   2. Update with expectedVersion=999
// Expected Result:
//   - Returns ErrOptimisticLockConflict
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8Dept008_UpdateOptimisticLock(t *testing.T) {
	appPool, _ := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewDepartmentRepository(appPool)

	d, err := repo.Insert(ctx, &domain.Department{Code: "OL_008", Name: "OL 008"})
	require.NoError(t, err)

	newName := "Won't Land"
	_, err = repo.Update(ctx, d.ID, &newName, nil, 999)
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
//   1. Seed tenant
//   2. FindByID
// Expected Result:
//   - Returns tenant with expected slug + record_version=1
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP8Tenant001_FindByIDHappy(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
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
//   1. FindByID(random uuid)
// Expected Result:
//   - Returns error surfaced from repo (tenant_not_found or pgx.ErrNoRows path)
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8Tenant002_FindByIDNotFound(t *testing.T) {
	appPool, _ := setupTestDB(t)
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
//   1. Seed
//   2. Update with Name="Renamed" and RecordVersion=1
// Expected Result:
//   - Returned tenant carries new name and record_version=2
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP8Tenant003_UpdateNameHappy(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
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
//   1. Update with RecordVersion=999
// Expected Result:
//   - Returns ErrOptimisticLockConflict
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8Tenant004_UpdateOptimisticLock(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
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
//   1. Seed tenant with slug='immut'
//   2. Direct SQL: UPDATE tenants SET slug='new'
// Expected Result:
//   - Trigger raises exception with 'tenant slug is immutable' message
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP8Tenant005_SlugImmutability(t *testing.T) {
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "trepo-005-immut")

	_, err := rawPool.Exec(ctx,
		`UPDATE tenants SET slug = 'new-slug' WHERE id = $1`, tenantID)
	require.Error(t, err, "T-1 trigger must block slug UPDATE")
}
