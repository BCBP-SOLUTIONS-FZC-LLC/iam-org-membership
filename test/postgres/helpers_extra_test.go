//go:build integration

// Phase 4 service-integration test helpers.
//
// Provides testFixtures that wires the full service graph (repos + TxRunner
// + outbox publisher) against the testcontainers Postgres from setupTestDB.
// Individual tests just call fx.Provisioning.TrialSignup(...) etc, then
// query outbox_events / dept_memberships / whatever to assert observable
// side-effects.
//
// Design note: we wire the REAL outbox publisher (via eventbusadapter.New
// with NoopCodec) so event enqueues actually land in outbox_events. We do
// NOT wire the outbox RUNNER (the goroutine that drains outbox → SNS) —
// that would introduce non-determinism. Tests inspect outbox_events
// directly to verify what a downstream SNS subscriber would eventually see.

package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/eventbus"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// testFixtures bundles a fully wired service graph plus the two pools so
// tests can seed data via rawPool (BYPASSRLS) and exercise services via
// the RLS-enforcing appPool.
type testFixtures struct {
	appPool *pgcommon.Pool
	rawPool *pgxpool.Pool

	// Repos (all directly usable in assertions).
	Tenants     port.TenantRepository
	Memberships port.MembershipRepository
	Roles       port.TenantRoleRepository
	DeptMems    port.DeptMembershipRepository
	Labels      port.DeptRoleLabelRepository
	TenantDepts port.TenantDepartmentRepository
	Delegations port.DelegationRepository
	ACLs        port.TenderACLRepository
	GroupMaps   port.GroupMappingRepository
	Invitations port.InvitationRepository

	// CatalogDepts/CatalogPlans stand in for the departments/plans catalog
	// now owned by the Catalog / Admin Config Service (migration-runbook
	// Phase 4 — LLD §12 step 4 dropped the local departments/plans tables).
	// Seeded with the same 5 system departments / 3 plan tiers the old
	// migration used to seed; tests needing an ad-hoc department register
	// one via CatalogDepts.add(...).
	CatalogDepts *fakeCatalogDepartments
	CatalogPlans *fakeCatalogPlans

	// Services under test.
	Provisioning   *service.ProvisioningService
	Membership     *service.MembershipService
	DeptMembership *service.DeptMembershipService
	Operator       *service.OperatorService
	GroupMapping   *service.GroupMappingService
	Invitation     *service.InvitationService
	AuthZ          *service.AuthZService
	Delegation     *service.DelegationService
	Tenant         *service.TenantService
	Department     *service.DepartmentService
	RoleLabel      *service.RoleLabelService

	// Overridable stubs — tests can reach in and swap behaviour (e.g.
	// fx.UP.FailNext = true to simulate a UP outage for CONS-2 tests).
	UP *fakeUserProfile
	RP *fakeRealmProvisioner
}

// buildTestFixtures wires everything on top of the containers Postgres.
// Fake outbound clients are used for RP/UP/Workflow — they always return
// no-op results so the service flows don't fail on external dependencies.
func buildTestFixtures(t testing.TB) *testFixtures {
	t.Helper()
	appPool, rawPool := setupTestDB(t)

	tenants := pgadapter.NewTenantRepository(appPool)
	memberships := pgadapter.NewMembershipRepository(appPool)
	roles := pgadapter.NewTenantRoleRepository(appPool)
	deptMems := pgadapter.NewDeptMembershipRepository(appPool)
	labels := pgadapter.NewDeptRoleLabelRepository(appPool)
	tenantDepts := pgadapter.NewTenantDepartmentRepository(appPool)
	delegations := pgadapter.NewDelegationRepository(appPool)
	acls := pgadapter.NewTenderACLRepository(appPool)
	groupMaps := pgadapter.NewGroupMappingRepository(appPool)
	invitations := pgadapter.NewInvitationRepository(appPool)

	// Outbox publisher — writes to outbox_events table on EnqueueCtx.
	// NoopCodec passes payloads through as JSON bytes.
	codec, err := eventbusadapter.NewValidatingCodec(eventbusadapter.NoopCodec{})
	require.NoError(t, err)
	outboxPub := eventbusadapter.New("iam-org-membership-test", codec)

	// TxRunner injects a tx-bound event publisher into ctx.
	txRunner := pgadapter.NewTxRunner(appPool, outboxPub)

	// catalogDepts/catalogPlans stand in for the departments/plans catalog
	// (migration-runbook Phase 4 — LLD §12 step 4: those tables and their
	// FKs are dropped; validation now happens app-side against a Catalog
	// service client, which these fakes stand in for so integration tests
	// don't need a running catalog-admin-config instance).
	catalogDepts := newFakeCatalogDepartments()
	catalogPlans := newFakeCatalogPlans()

	fx := &testFixtures{
		appPool:      appPool,
		rawPool:      rawPool,
		Tenants:      tenants,
		Memberships:  memberships,
		Roles:        roles,
		DeptMems:     deptMems,
		Labels:       labels,
		TenantDepts:  tenantDepts,
		Delegations:  delegations,
		ACLs:         acls,
		GroupMaps:    groupMaps,
		Invitations:  invitations,
		CatalogDepts: catalogDepts,
		CatalogPlans: catalogPlans,
	}

	fx.Provisioning = service.NewProvisioningService(
		appPool, tenants, memberships, roles, deptMems, labels,
		tenantDepts, catalogDepts, delegations, acls, catalogPlans,
		txRunner, nil, // cache=nil (advisory)
		&fakeRealmProvisioner{}, // no-op RP client
	)
	fx.DeptMembership = service.NewDeptMembershipService(
		deptMems, memberships, tenantDepts, catalogDepts, delegations,
		&fakeWorkflow{}, nil, txRunner,
	)
	fx.GroupMapping = service.NewGroupMappingService(
		groupMaps, memberships, roles, deptMems, catalogDepts, txRunner, nil,
	)
	fx.Membership = service.NewMembershipService(
		memberships, roles, deptMems, delegations, acls,
		tenants, invitations, nil, // cache
		&fakeRealmProvisioner{}, &fakeWorkflow{},
		txRunner, nil, 30, // seatOverageDays
	)
	fx.Operator = service.NewOperatorService(
		appPool, tenants, roles, memberships, nil, txRunner,
	)
	fx.Invitation = service.NewInvitationService(
		invitations, memberships, roles, deptMems, tenants,
		&fakeRealmProvisioner{}, nil, txRunner, nil, 7,
	)
	fx.AuthZ = service.NewAuthZService(appPool, catalogPlans, nil)
	// UP + RP clients are injected via fx.UP / fx.RP so individual tests can
	// override behaviour (fx.UP.FailNext = true / fx.RP.PatchRealmConfigFailNext = true).
	fx.UP = &fakeUserProfile{}
	fx.RP = &fakeRealmProvisioner{}
	fx.Delegation = service.NewDelegationService(
		delegations, memberships, fx.UP, nil, txRunner,
	)
	fx.Tenant = service.NewTenantService(tenants, nil, fx.RP)
	fx.Department = service.NewDepartmentService(catalogDepts, tenantDepts, nil)
	fx.RoleLabel = service.NewRoleLabelService(labels, nil)

	return fx
}

// fakeCatalogDepartments is an in-memory stand-in for the departments that
// used to live in this service's own `departments` table (dropped per
// migration-runbook Phase 4 — LLD §12 step 4, now owned by the Catalog /
// Admin Config Service). Seeded with the same 5 system departments the
// dropped migration used to seed, so integration tests keep exercising
// realistic catalog data. Tests needing an ad-hoc (usually non-system)
// department register one via add().
type fakeCatalogDepartments struct {
	mu       sync.Mutex
	rows     map[uuid.UUID]domain.Department
	failNext bool // fault-injection: next Departments() call returns an error
}

func newFakeCatalogDepartments() *fakeCatalogDepartments {
	f := &fakeCatalogDepartments{rows: map[uuid.UUID]domain.Department{}}
	for _, code := range []string{"ENGINEERING", "DESIGN", "PROCUREMENT", "FINANCE", "LEGAL"} {
		f.add(domain.Department{Code: code, Name: code, IsSystem: true, IsActive: true})
	}
	return f
}

// add registers a department, generating an ID/RecordVersion if unset, and
// returns the (possibly generated) ID.
func (f *fakeCatalogDepartments) add(d domain.Department) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	if d.RecordVersion == 0 {
		d.RecordVersion = 1
	}
	f.rows[d.ID] = d
	return d.ID
}

// byCode looks up a department by its catalog code (e.g. "ENGINEERING").
func (f *fakeCatalogDepartments) byCode(code string) (domain.Department, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.rows {
		if d.Code == code {
			return d, true
		}
	}
	return domain.Department{}, false
}

// failNextCall arms a one-shot fault: the next Departments() call returns an
// error instead of the catalog, mirroring the old ALTER-TABLE-rename
// fault-injection technique used before the table existed only as a fake.
func (f *fakeCatalogDepartments) failNextCall() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext = true
}

func (f *fakeCatalogDepartments) Departments(_ context.Context) ([]domain.Department, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext {
		f.failNext = false
		return nil, errors.New("catalog service unavailable (fault injection)")
	}
	out := make([]domain.Department, 0, len(f.rows))
	for _, d := range f.rows {
		out = append(out, d)
	}
	return out, nil
}

func (f *fakeCatalogDepartments) DepartmentByID(_ context.Context, id uuid.UUID) (*domain.Department, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.rows[id]
	if !ok {
		return nil, domain.NewError(domain.ErrDepartmentNotFound, "department not found")
	}
	return &d, nil
}

var _ port.DepartmentCatalogReader = (*fakeCatalogDepartments)(nil)

// fakeCatalogPlans is the plans-side equivalent of fakeCatalogDepartments,
// seeded with the same 3 plan tiers the dropped migration used to seed.
type fakeCatalogPlans struct {
	mu   sync.Mutex
	rows map[domain.TenantPlan]domain.Plan
}

func newFakeCatalogPlans() *fakeCatalogPlans {
	limit := func(n int) *int { return &n }
	return &fakeCatalogPlans{rows: map[domain.TenantPlan]domain.Plan{
		domain.PlanStarter: {
			Code: domain.PlanStarter, DisplayName: "Starter",
			WorkflowTemplateLimit: limit(5), TenderLimit: limit(10),
			TrialDurationDays: 30, CustomBranding: domain.BrandingNone,
			FeatureSet: map[string]any{}, RecordVersion: 1,
		},
		domain.PlanPro: {
			Code: domain.PlanPro, DisplayName: "Pro",
			WorkflowTemplateLimit: limit(50), TenderLimit: limit(100),
			TrialDurationDays: 30, CustomBranding: domain.BrandingLogo,
			FeatureSet: map[string]any{}, RecordVersion: 1,
		},
		domain.PlanEnterprise: {
			Code: domain.PlanEnterprise, DisplayName: "Enterprise",
			TrialDurationDays: 30, SSOEnabled: true, CustomBranding: domain.BrandingLogo,
			FeatureSet: map[string]any{"require_mfa_all_users_allowed": true}, RecordVersion: 1,
		},
	}}
}

func (f *fakeCatalogPlans) Plans(_ context.Context) ([]domain.Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.Plan, 0, len(f.rows))
	for _, p := range f.rows {
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeCatalogPlans) PlanByCode(_ context.Context, code domain.TenantPlan) (*domain.Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.rows[code]
	if !ok {
		return nil, domain.NewError(domain.ErrPlanNotFound, "plan not found")
	}
	return &p, nil
}

var _ port.PlanCatalogReader = (*fakeCatalogPlans)(nil)

// countOutboxEvents returns the number of rows currently in outbox_events
// for the given tenant. Uses rawPool (BYPASSRLS) so cross-tenant assertions
// remain possible.
func (fx *testFixtures) countOutboxEvents(t *testing.T, ctx context.Context, tenantID uuid.UUID) int {
	t.Helper()
	var n int
	err := fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE tenant_id = $1`,
		tenantID.String()).Scan(&n)
	require.NoError(t, err)
	return n
}

// outboxEventTypesForTenant returns every event_type row for the tenant,
// in insertion order. Duplicates preserved so per-delta counts hold.
func (fx *testFixtures) outboxEventTypesForTenant(t *testing.T, ctx context.Context, tenantID uuid.UUID) []string {
	t.Helper()
	rows, err := fx.rawPool.Query(ctx,
		`SELECT event_type FROM outbox_events
		 WHERE tenant_id = $1
		 ORDER BY created_at ASC, id ASC`,
		tenantID.String())
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var et string
		require.NoError(t, rows.Scan(&et))
		out = append(out, et)
	}
	require.NoError(t, rows.Err())
	return out
}

// ── fake outbound clients ──────────────────────────────────────────────

// fakeRealmProvisioner is a stub for tests that don't exercise the RP
// integration surface. Individual methods can be configured to fail via
// the *FailNext booleans (one-shot) so tests can exercise the T-15
// deferred-sync branch, PI-9 cleanup errors, etc.
type fakeRealmProvisioner struct {
	PatchRealmConfigFailNext   bool
	CreateInvitedUserFailNext  bool
	DeleteUserFailNext         bool
	RevokeUserSessionsFailNext bool

	PatchRealmConfigCalls []port.RealmConfigPatch
}

func (f *fakeRealmProvisioner) CreateInvitedUser(_ context.Context, req port.CreateInvitedUserRequest) (*port.CreateInvitedUserResponse, error) {
	if f.CreateInvitedUserFailNext {
		f.CreateInvitedUserFailNext = false
		return nil, errors.New("fake RP CreateInvitedUser outage")
	}
	return &port.CreateInvitedUserResponse{KeycloakUserID: uuid.New()}, nil
}
func (f *fakeRealmProvisioner) DeleteUser(_ context.Context, _, _ uuid.UUID) error {
	if f.DeleteUserFailNext {
		f.DeleteUserFailNext = false
		return errors.New("fake RP DeleteUser outage")
	}
	return nil
}
func (f *fakeRealmProvisioner) PatchRealmConfig(_ context.Context, _ uuid.UUID, p port.RealmConfigPatch) error {
	f.PatchRealmConfigCalls = append(f.PatchRealmConfigCalls, p)
	if f.PatchRealmConfigFailNext {
		f.PatchRealmConfigFailNext = false
		return errors.New("fake RP PatchRealmConfig outage")
	}
	return nil
}
func (f *fakeRealmProvisioner) RevokeUserSessions(_ context.Context, _, _ uuid.UUID) error {
	if f.RevokeUserSessionsFailNext {
		f.RevokeUserSessionsFailNext = false
		return errors.New("fake RP RevokeUserSessions outage")
	}
	return nil
}

// fakeUserProfile is a configurable stub for the UserProfileClient port.
// Default behaviour: SetAvailability succeeds. Tests can flip FailNext or
// set an ErrToReturn to simulate UP outages / non-2xx responses.
type fakeUserProfile struct {
	FailNext    bool
	ErrToReturn error
	Calls       []port.SetAvailabilityRequest
}

func (f *fakeUserProfile) SetAvailability(_ context.Context, req port.SetAvailabilityRequest) error {
	f.Calls = append(f.Calls, req)
	if f.FailNext {
		f.FailNext = false
		if f.ErrToReturn != nil {
			return f.ErrToReturn
		}
		return errors.New("fake UP outage")
	}
	return nil
}

// fakeWorkflow returns zero impact — matches the WFI-13 fail-open path
// so removal/dept-remove flows don't block on Workflow.
type fakeWorkflow struct{}

func (f *fakeWorkflow) GetDelegateImpact(_ context.Context, _, _ uuid.UUID, _ *uuid.UUID) (*port.DelegateImpact, error) {
	return &port.DelegateImpact{ActiveWorkflows: 0}, nil
}
func (f *fakeWorkflow) ReassignDelegate(_ context.Context, _, _, _ uuid.UUID, _ *uuid.UUID) error {
	return nil
}
func (f *fakeWorkflow) CancelByDelegate(_ context.Context, _, _ uuid.UUID, _ *uuid.UUID) error {
	return nil
}
