// Unit tests for the migration-runbook Phase 2 rewrite of
// ProvisioningService.TrialSignup's system-department seeding step
// (internal/core/service/provisioning_service.go). Before the
// catalog-admin-config read cutover this called s.depts.List(txCtx, true)
// *inside* the running transaction; it now fetches the full catalog via
// s.catalog.Departments(ctx) *before* the transaction starts and filters
// in Go. These tests exist because that filter logic (IsSystem &&
// IsActive && code ∈ {ENGINEERING,DESIGN,PROCUREMENT,FINANCE,LEGAL}) had
// zero unit coverage — the only pre-existing coverage was an integration
// test requiring a real Postgres (test/postgres/services_test.go
// TestG1_TrialSignup_ActivatesExactly5NamedDepartments).
package unit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── fakes local to this file (ptd = ProvisioningTrialDept) ──────────────

type ptdCatalogDepts struct {
	departmentsFn func(context.Context) ([]domain.Department, error)
}

func (f *ptdCatalogDepts) Departments(ctx context.Context) ([]domain.Department, error) {
	return f.departmentsFn(ctx)
}
func (f *ptdCatalogDepts) DepartmentByID(context.Context, uuid.UUID) (*domain.Department, error) {
	return nil, errors.New("not used")
}

var _ port.DepartmentCatalogReader = (*ptdCatalogDepts)(nil)

type ptdCatalogPlans struct {
	planByCodeFn func(context.Context, domain.TenantPlan) (*domain.Plan, error)
}

func (f *ptdCatalogPlans) Plans(context.Context) ([]domain.Plan, error) {
	return nil, errors.New("not used")
}
func (f *ptdCatalogPlans) PlanByCode(ctx context.Context, code domain.TenantPlan) (*domain.Plan, error) {
	if f.planByCodeFn != nil {
		return f.planByCodeFn(ctx, code)
	}
	return &domain.Plan{Code: code, TrialDurationDays: 30}, nil
}

var _ port.PlanCatalogReader = (*ptdCatalogPlans)(nil)

// ptdTenantDeptRepo is a spy: it records every Activate call so tests can
// assert exactly which department IDs the filter logic selected, without
// needing a real Postgres tenant_departments table.
type ptdTenantDeptRepo struct {
	activated []uuid.UUID
}

func (r *ptdTenantDeptRepo) List(context.Context, uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *ptdTenantDeptRepo) ListActive(context.Context, uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *ptdTenantDeptRepo) Find(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
	return nil, nil
}
func (r *ptdTenantDeptRepo) Activate(_ context.Context, tenantID, deptID uuid.UUID) (*domain.TenantDepartment, error) {
	r.activated = append(r.activated, deptID)
	return &domain.TenantDepartment{TenantID: tenantID, DepartmentID: deptID, IsActive: true, RecordVersion: 1}, nil
}
func (r *ptdTenantDeptRepo) SetActive(context.Context, uuid.UUID, uuid.UUID, bool, int64) (*domain.TenantDepartment, error) {
	return nil, nil
}

var _ port.TenantDepartmentRepository = (*ptdTenantDeptRepo)(nil)

type ptdTenantRepo struct{}

func (f *ptdTenantRepo) FindByID(context.Context, uuid.UUID) (*domain.Tenant, error) { return nil, nil }
func (f *ptdTenantRepo) FindByIDIncludingDeleted(context.Context, uuid.UUID) (*domain.Tenant, error) {
	return nil, nil
}
func (f *ptdTenantRepo) Update(context.Context, uuid.UUID, *domain.TenantPatch) (*domain.Tenant, error) {
	return nil, nil
}
func (f *ptdTenantRepo) SetRealmSyncPending(context.Context, uuid.UUID) error { return nil }
func (f *ptdTenantRepo) Insert(_ context.Context, t *domain.Tenant) (*domain.Tenant, bool, error) {
	return t, true, nil
}

var _ port.TenantRepository = (*ptdTenantRepo)(nil)

type ptdMembershipRepo struct{}

func (f *ptdMembershipRepo) List(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
	return nil, nil
}
func (f *ptdMembershipRepo) FindByUserID(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
	return nil, nil
}
func (f *ptdMembershipRepo) Insert(_ context.Context, tm *domain.TenantMembership) (*domain.TenantMembership, error) {
	tm.ID = uuid.New()
	return tm, nil
}
func (f *ptdMembershipRepo) SetStatus(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
	return nil, nil
}
func (f *ptdMembershipRepo) SoftDelete(context.Context, uuid.UUID, uuid.UUID, int64) error {
	return nil
}
func (f *ptdMembershipRepo) CountActive(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (f *ptdMembershipRepo) ListActiveUserIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

var _ port.MembershipRepository = (*ptdMembershipRepo)(nil)

type ptdRoleRepo struct{}

func (f *ptdRoleRepo) ListByUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}
func (f *ptdRoleRepo) ListByRole(context.Context, uuid.UUID, domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (f *ptdRoleRepo) CountActiveOwners(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (f *ptdRoleRepo) Grant(_ context.Context, r *domain.TenantRole) (*domain.TenantRole, error) {
	return r, nil
}
func (f *ptdRoleRepo) Revoke(context.Context, uuid.UUID, uuid.UUID, domain.TenantRoleCode) (*domain.TenantRole, error) {
	return nil, nil
}
func (f *ptdRoleRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}

var _ port.TenantRoleRepository = (*ptdRoleRepo)(nil)

type ptdLabelRepo struct{}

func (f *ptdLabelRepo) List(context.Context, uuid.UUID) ([]domain.DeptRoleLabel, error) {
	return nil, nil
}
func (f *ptdLabelRepo) Update(context.Context, uuid.UUID, domain.DeptRole, string, int64) (*domain.DeptRoleLabel, error) {
	return nil, nil
}
func (f *ptdLabelRepo) Seed(context.Context, uuid.UUID) ([]domain.DeptRoleLabel, error) {
	return nil, nil
}

var _ port.DeptRoleLabelRepository = (*ptdLabelRepo)(nil)

// buildTrialProvisioningSvc wires a full happy-path ProvisioningService —
// every collaborator TrialSignup touches succeeds — except catalogDepts,
// which the caller controls, so tests can isolate the department
// fetch/filter behavior via the ptdTenantDeptRepo spy.
func buildTrialProvisioningSvc(catalogDepts port.DepartmentCatalogReader) (*service.ProvisioningService, *ptdTenantDeptRepo) {
	tenantDepts := &ptdTenantDeptRepo{}
	svc := service.NewProvisioningService(
		nil,                                                         // pool — unused by TrialSignup directly
		&ptdTenantRepo{}, &ptdMembershipRepo{}, &ptdRoleRepo{}, nil, /* deptMems: unused by TrialSignup */
		&ptdLabelRepo{}, tenantDepts, catalogDepts,
		&ptdCatalogPlans{},
		&passthroughTxRunner{}, nil /* cache */, nil, /* rp: unused by TrialSignup */
	)
	return svc, tenantDepts
}

func trialSignupInput() service.TrialSignupInput {
	return service.TrialSignupInput{
		TenantID:      uuid.New(),
		Slug:          "acme-trial",
		Name:          "Acme Trial",
		Plan:          domain.PlanStarter,
		OwnerUserID:   uuid.New(),
		DefaultLocale: "en-US",
	}
}

func sysDept(code string, isActive bool) domain.Department {
	return domain.Department{ID: uuid.New(), Code: code, Name: code, IsSystem: true, IsActive: isActive}
}

func nonSysDept(code string) domain.Department {
	return domain.Department{ID: uuid.New(), Code: code, Name: code, IsSystem: false, IsActive: true}
}

// ── Happy path: exactly the 5 named, active, system departments ────────

func TestTrialSignup_ActivatesExactlyTheFiveNamedActiveSystemDepartments(t *testing.T) {
	catalog := &ptdCatalogDepts{departmentsFn: func(context.Context) ([]domain.Department, error) {
		return []domain.Department{
			sysDept("ENGINEERING", true),
			sysDept("DESIGN", true),
			sysDept("PROCUREMENT", true),
			sysDept("FINANCE", true),
			sysDept("LEGAL", true),
		}, nil
	}}
	svc, tenantDepts := buildTrialProvisioningSvc(catalog)

	_, wasCreated, err := svc.TrialSignup(context.Background(), trialSignupInput())
	require.NoError(t, err)
	assert.True(t, wasCreated)
	assert.Len(t, tenantDepts.activated, 5, "exactly 5 departments must be activated")
}

// ── A 6th is_system department must NOT auto-activate (§8.1) ───────────

func TestTrialSignup_SixthSystemDepartment_NotAutoActivated(t *testing.T) {
	catalog := &ptdCatalogDepts{departmentsFn: func(context.Context) ([]domain.Department, error) {
		return []domain.Department{
			sysDept("ENGINEERING", true),
			sysDept("DESIGN", true),
			sysDept("PROCUREMENT", true),
			sysDept("FINANCE", true),
			sysDept("LEGAL", true),
			sysDept("MARKETING", true), // hypothetical future 6th system dept
		}, nil
	}}
	svc, tenantDepts := buildTrialProvisioningSvc(catalog)

	_, _, err := svc.TrialSignup(context.Background(), trialSignupInput())
	require.NoError(t, err)
	assert.Len(t, tenantDepts.activated, 5, "a 6th is_system dept must not auto-activate — opt-in only via P-24")
}

// ── Non-system departments sharing a trial code name are excluded ──────

func TestTrialSignup_NonSystemDepartmentWithMatchingCode_Excluded(t *testing.T) {
	catalog := &ptdCatalogDepts{departmentsFn: func(context.Context) ([]domain.Department, error) {
		return []domain.Department{
			sysDept("ENGINEERING", true),
			nonSysDept("LEGAL"), // operator-added, non-system dept that happens to share a trial code
		}, nil
	}}
	svc, tenantDepts := buildTrialProvisioningSvc(catalog)

	_, _, err := svc.TrialSignup(context.Background(), trialSignupInput())
	require.NoError(t, err)
	require.Len(t, tenantDepts.activated, 1, "only the IsSystem=true row activates, even though both share a trial code")
}

// ── A globally-retired (IsActive=false) system department is excluded ──
// This is the specific semantic the pre-cutover code expressed via
// activeOnly=true on the local repo call — the rewrite must preserve it
// even though the flat catalog-admin-config Departments() call has no
// server-side active_only filter of its own.

func TestTrialSignup_RetiredSystemDepartment_Excluded(t *testing.T) {
	catalog := &ptdCatalogDepts{departmentsFn: func(context.Context) ([]domain.Department, error) {
		return []domain.Department{
			sysDept("ENGINEERING", true),
			sysDept("LEGAL", false), // retired — must not be activated for a new trial tenant
		}, nil
	}}
	svc, tenantDepts := buildTrialProvisioningSvc(catalog)

	_, _, err := svc.TrialSignup(context.Background(), trialSignupInput())
	require.NoError(t, err)
	require.Len(t, tenantDepts.activated, 1)
}

// ── Empty catalog → zero activations, no error ──────────────────────────

func TestTrialSignup_EmptyCatalog_NoActivationsNoError(t *testing.T) {
	catalog := &ptdCatalogDepts{departmentsFn: func(context.Context) ([]domain.Department, error) {
		return nil, nil
	}}
	svc, tenantDepts := buildTrialProvisioningSvc(catalog)

	_, _, err := svc.TrialSignup(context.Background(), trialSignupInput())
	require.NoError(t, err)
	assert.Empty(t, tenantDepts.activated)
}

// ── Catalog client failure propagates and short-circuits before any tx ──

func TestTrialSignup_CatalogDepartmentsError_PropagatesBeforeTxStarts(t *testing.T) {
	catalogErr := errors.New("catalog-admin-config unreachable")
	catalog := &ptdCatalogDepts{departmentsFn: func(context.Context) ([]domain.Department, error) {
		return nil, catalogErr
	}}
	svc, tenantDepts := buildTrialProvisioningSvc(catalog)

	_, _, err := svc.TrialSignup(context.Background(), trialSignupInput())
	require.ErrorIs(t, err, catalogErr)
	assert.Empty(t, tenantDepts.activated, "no activation attempts once the pre-tx catalog fetch fails")
}

// ── Plan lookup happens BEFORE the department fetch — a plan-lookup
// failure must short-circuit without ever calling Departments() ─────────

func TestTrialSignup_PlanLookupError_NeverReachesDepartmentFetch(t *testing.T) {
	planErr := errors.New("plan not found")
	deptFetchCalled := false
	catalog := &ptdCatalogDepts{departmentsFn: func(context.Context) ([]domain.Department, error) {
		deptFetchCalled = true
		return nil, nil
	}}
	tenantDepts := &ptdTenantDeptRepo{}
	svc := service.NewProvisioningService(
		nil, &ptdTenantRepo{}, &ptdMembershipRepo{}, &ptdRoleRepo{}, nil,
		&ptdLabelRepo{}, tenantDepts, catalog,
		&ptdCatalogPlans{planByCodeFn: func(context.Context, domain.TenantPlan) (*domain.Plan, error) {
			return nil, planErr
		}},
		&passthroughTxRunner{}, nil, nil,
	)

	_, _, err := svc.TrialSignup(context.Background(), trialSignupInput())
	require.ErrorIs(t, err, planErr)
	assert.False(t, deptFetchCalled, "plan lookup precedes the department fetch — an error there must short-circuit first")
}

// ── Idempotent replay (tenant row already existed) skips department
// activation entirely — TrialSignup's existing ON CONFLICT (id) DO
// NOTHING short-circuit must still work with the new pre-tx fetch order ──

func TestTrialSignup_IdempotentReplay_SkipsDepartmentActivation(t *testing.T) {
	deptFetchCalled := false
	catalog := &ptdCatalogDepts{departmentsFn: func(context.Context) ([]domain.Department, error) {
		deptFetchCalled = true
		return []domain.Department{sysDept("ENGINEERING", true)}, nil
	}}
	tenantDepts := &ptdTenantDeptRepo{}
	tenants := &ptdTenantRepoReplay{}
	svc := service.NewProvisioningService(
		nil, tenants, &ptdMembershipRepo{}, &ptdRoleRepo{}, nil,
		&ptdLabelRepo{}, tenantDepts, catalog,
		&ptdCatalogPlans{}, &passthroughTxRunner{}, nil, nil,
	)

	_, wasCreated, err := svc.TrialSignup(context.Background(), trialSignupInput())
	require.NoError(t, err)
	assert.False(t, wasCreated, "idempotent replay must report wasCreated=false")
	// The department fetch happens before the tx (and before the
	// freshInsert check, which only runs inside the tx) — it is NOT
	// skipped by the replay short-circuit. This test documents that
	// fact rather than asserting the (arguably wasteful) fetch away,
	// since fixing it would change TrialSignup's behavior beyond the
	// scope of the read-cutover rewrite.
	assert.True(t, deptFetchCalled)
	assert.Empty(t, tenantDepts.activated, "replay must not activate any department a second time")
}

// ptdTenantRepoReplay simulates the ON CONFLICT (id) DO NOTHING replay
// path: Insert reports the row already existed (wasCreated=false).
type ptdTenantRepoReplay struct{}

func (f *ptdTenantRepoReplay) FindByID(context.Context, uuid.UUID) (*domain.Tenant, error) {
	return nil, nil
}
func (f *ptdTenantRepoReplay) FindByIDIncludingDeleted(context.Context, uuid.UUID) (*domain.Tenant, error) {
	return nil, nil
}
func (f *ptdTenantRepoReplay) Update(context.Context, uuid.UUID, *domain.TenantPatch) (*domain.Tenant, error) {
	return nil, nil
}
func (f *ptdTenantRepoReplay) SetRealmSyncPending(context.Context, uuid.UUID) error { return nil }
func (f *ptdTenantRepoReplay) Insert(_ context.Context, t *domain.Tenant) (*domain.Tenant, bool, error) {
	return t, false, nil // already existed
}

var _ port.TenantRepository = (*ptdTenantRepoReplay)(nil)
