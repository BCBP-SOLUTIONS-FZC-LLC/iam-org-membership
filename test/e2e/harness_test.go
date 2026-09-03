//go:build e2e

// Package e2e_test spins up the full iam-org-membership stack against a real
// Postgres testcontainer and exposes it over an httptest.Server. Tests hit
// the running server via net/http with real gateway-forwarded identity
// headers — the same shape API Gateway sets in production.
//
// Design:
//   - Postgres 17 container per test (mirrors sibling test/postgres pattern)
//   - Full RLS-aware pgcommon.Pool (org_membership_app role, no BYPASSRLS)
//   - Every handler, every route, every middleware — same wiring as
//     cmd/server/main.go
//   - Fake outbound clients (RP / UP / Workflow) so the flow tests never
//     require a real Keycloak / User Profile / Workflow instance
//   - No outbox runner, no SNS/SQS consumer — the wire path has Phase 12
//     coverage; Phase 13 verifies the HTTP surface end-to-end
//
// Tests carry the P13-* stable ID namespace (mandatory metadata block in
// each test docstring per Test_cover.md Rule 3).
package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/http"
	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/eventbus"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/migrate"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

const (
	appRolePassword  = "apppassword-e2eonly"
	migratorPassword = "migratorpassword-e2eonly"
)

// noopPinger always reports healthy — this harness has no outbox runner
// and doesn't exercise /readyz, so a real Postgres/Valkey/outbox check
// isn't needed here.
type noopPinger struct{}

func (noopPinger) Health(context.Context) error { return nil }

// e2eEnv bundles a fully wired stack + a live httptest.Server so tests can
// issue real HTTP requests.
type e2eEnv struct {
	ctx     context.Context
	appPool *pgcommon.Pool
	rawPool *dbseed.Pool
	server  *httptest.Server
	baseURL string

	// Fake outbound clients — reachable from tests that want to simulate
	// outages / assert calls made.
	RP *fakeRealmProvisioner
	WF *fakeWorkflow
}

// newE2EEnv provisions PG + wires the stack + boots the httptest server.
func newE2EEnv(t *testing.T) *e2eEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}
	ctx := context.Background()

	appPool, rawPool := setupE2EDB(t, ctx)

	// Repositories.
	tenantRepo := pgadapter.NewTenantRepository(appPool)
	authzRepo := pgadapter.NewAuthZRepository(appPool)
	tenantDeptRepo := pgadapter.NewTenantDepartmentRepository(appPool)
	membershipRepo := pgadapter.NewMembershipRepository(appPool)
	tenantRoleRepo := pgadapter.NewTenantRoleRepository(appPool)
	deptMemRepo := pgadapter.NewDeptMembershipRepository(appPool)
	deptRoleLabelRepo := pgadapter.NewDeptRoleLabelRepository(appPool)
	invitationRepo := pgadapter.NewInvitationRepository(appPool)

	// Outbox publisher — writes to outbox_events without draining.
	rawCodec := eventbusadapter.Codec(eventbusadapter.NoopCodec{})
	codec, err := eventbusadapter.NewValidatingCodec(rawCodec)
	require.NoError(t, err)
	outboxPub := eventbusadapter.New("iam-org-membership-e2e", codec)
	txRunner := pgadapter.NewTxRunner(appPool, outboxPub)

	// Fake outbound clients.
	rp := &fakeRealmProvisioner{}
	wf := &fakeWorkflow{}

	// catalogDepts/catalogPlans stand in for the departments/plans catalog
	// (migration-runbook Phase 4 — LLD §12 step 4: those tables and their
	// FKs are dropped; validation now happens app-side against a Catalog
	// service client, which these fakes stand in for).
	catalogDepts := newFakeCatalogDepartments()
	catalogPlans := newFakeCatalogPlans()

	// Services.
	authzSvc := service.NewAuthZService(authzRepo, catalogPlans, catalogDepts, nil)
	provisioningSvc := service.NewProvisioningService(tenantRepo, membershipRepo, tenantRoleRepo, deptMemRepo, deptRoleLabelRepo, tenantDeptRepo, catalogDepts, catalogPlans, txRunner, nil, rp)
	tenantSvc := service.NewTenantService(tenantRepo, nil, rp)
	deptSvc := service.NewDepartmentService(catalogDepts, tenantDeptRepo, nil)
	membershipSvc := service.NewMembershipService(membershipRepo, tenantRoleRepo, deptMemRepo, tenantRepo, invitationRepo, nil, rp, wf, txRunner, nil, 30)
	deptMemSvc := service.NewDeptMembershipService(deptMemRepo, membershipRepo, tenantDeptRepo, catalogDepts, nil, wf, nil, txRunner)
	roleLabelSvc := service.NewRoleLabelService(deptRoleLabelRepo, nil)
	groupMappingSvc := service.NewGroupMappingService(membershipRepo, tenantRoleRepo, deptMemRepo, txRunner, nil, nil)
	invitationSvc := service.NewInvitationService(invitationRepo, membershipRepo, tenantRoleRepo, deptMemRepo, tenantRepo, rp, nil, txRunner, nil, 7)
	operatorSvc := service.NewOperatorService(tenantRepo, tenantRoleRepo, membershipRepo, nil, txRunner)

	// Handlers.
	tenantH := httpadapter.NewTenantHandler(tenantSvc)
	deptH := httpadapter.NewDepartmentHandler(deptSvc)
	membershipH := httpadapter.NewMembershipHandler(membershipSvc)
	deptMemH := httpadapter.NewDeptMembershipHandler(deptMemSvc)
	roleLabelH := httpadapter.NewRoleLabelHandler(roleLabelSvc)
	// DelegationHandler is no longer constructed/routed — mirrors
	// cmd/server/main.go's ADR-0008 v2 cutover (P-18/19/20/32/33 moved to
	// the standalone Delegation Service's DLG-1..5; IDs never reused).
	// ACLHandler is no longer constructed/routed — mirrors cmd/server/main.go's
	// ADR-0007 Wave 3 Phase 6 cutover (P-21/22/23/I-12 fully removed, moved
	// to iam-tender-acl's TAC-1/2/3/4; IDs never reused).
	invitationH := httpadapter.NewInvitationHandler(invitationSvc)
	operatorH := httpadapter.NewOperatorHandler(operatorSvc)
	internalH := httpadapter.NewInternalHandler(provisioningSvc, authzSvc, membershipSvc, invitationSvc, groupMappingSvc, tenantSvc, nil) // I-16 untested here — no sysPool in this harness

	// Router — the exact same constructor cmd/server/main.go calls, so this
	// harness can never drift from production routing again (it used to be
	// a hand-copied route table that silently missed routes/gates added to
	// main.go after the fact).
	gin.SetMode(gin.TestMode)
	router := httpadapter.NewRouter(httpadapter.RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-org-membership-e2e"},
		Docs:      httpadapter.DocsConfig{Environment: "test"},

		TenantRepo:     tenantRepo,
		MembershipRepo: membershipRepo,

		TenantHandler:         tenantH,
		DepartmentHandler:     deptH,
		MembershipHandler:     membershipH,
		DeptMembershipHandler: deptMemH,
		RoleLabelHandler:      roleLabelH,
		InvitationHandler:     invitationH,
		OperatorHandler:       operatorH,
		InternalHandler:       internalH,

		Postgres: noopPinger{},
		Cache:    noopPinger{},
		Outbox:   noopPinger{},
	})

	srv := httptest.NewServer(router.Handler())
	t.Cleanup(srv.Close)

	return &e2eEnv{
		ctx: ctx, appPool: appPool, rawPool: rawPool,
		server: srv, baseURL: srv.URL,
		RP: rp, WF: wf,
	}
}

// ── Postgres setup (mirrors sibling test/postgres pattern) ────────────────

func setupE2EDB(t *testing.T, ctx context.Context) (*pgcommon.Pool, *dbseed.Pool) {
	t.Helper()
	pgContainer, err := tcpostgres.Run(ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("org_membership"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("testpassword"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgContainer.Terminate(ctx) })

	superDSN, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	rawPool, err := dbseed.New(ctx, superDSN)
	require.NoError(t, err)
	t.Cleanup(rawPool.Close)

	_, err = rawPool.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE org_membership_app LOGIN PASSWORD '%s' NOBYPASSRLS`, appRolePassword))
	require.NoError(t, err)
	_, err = rawPool.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE org_membership_migrator LOGIN PASSWORD '%s' BYPASSRLS`, migratorPassword))
	require.NoError(t, err)

	// outbox.ApplySchema must run first so platform-events creates
	// outbox_events (jsonb payload) before domain migration 000010
	// converts it to TEXT.
	require.NoError(t, outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: superDSN}))
	require.NoError(t, pgadapter.RunMigrations(ctx, superDSN))

	grants := []string{
		`GRANT CONNECT ON DATABASE org_membership TO org_membership_app`,
		`GRANT USAGE ON SCHEMA public TO org_membership_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO org_membership_app`,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO org_membership_app`,
		`GRANT EXECUTE ON FUNCTION app_tenant_id()                    TO org_membership_app`,
		`GRANT EXECUTE ON FUNCTION log_rls_violation(text, uuid, text) TO org_membership_app`,
		`GRANT EXECUTE ON FUNCTION rls_check_tenant(uuid, text)       TO org_membership_app`,
	}
	for _, stmt := range grants {
		_, err = rawPool.Exec(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	appDSN := strings.Replace(superDSN, "postgres:testpassword@", "org_membership_app:"+appRolePassword+"@", 1)
	appPool, err := pgcommon.NewPool(ctx, pgcommon.Config{
		DSN:           appDSN,
		PGBouncerMode: false,
		GUCProvider:   pgcommon.GUCSetFromContext,
	})
	require.NoError(t, err)
	t.Cleanup(appPool.Close)

	return appPool, rawPool
}

// ── HTTP client helpers ───────────────────────────────────────────────────

// httpDo executes the request against the e2e server. Callers set headers +
// body via requestOpts. Returns status + parsed JSON body (or nil).
type reqOpts struct {
	method  string
	path    string
	headers map[string]string
	body    any
	rawBody []byte // when set, wins over body
}

// do executes a request and returns status, response headers, and the raw
// response body. Callers unmarshal the body when needed.
func (e *e2eEnv) do(t *testing.T, opts reqOpts) (int, http.Header, []byte) {
	t.Helper()
	var reqBody io.Reader
	switch {
	case opts.rawBody != nil:
		reqBody = bytes.NewReader(opts.rawBody)
	case opts.body != nil:
		b, err := json.Marshal(opts.body)
		require.NoError(t, err)
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(e.ctx, opts.method, e.baseURL+opts.path, reqBody)
	require.NoError(t, err)
	// Default Content-Type for anything with a body; caller can override
	// via headers to test negative content-type paths.
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range opts.headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "%s %s", opts.method, opts.path)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, body
}

// gatewayHeaders returns a header map with the three gateway-forwarded
// identity headers set. Roles is optional (comma-separated string).
func gatewayHeaders(userID, tenantID uuid.UUID, roles string) map[string]string {
	h := map[string]string{
		"x-user-id":   userID.String(),
		"x-tenant-id": tenantID.String(),
	}
	if roles != "" {
		h["x-tenant-roles"] = roles
	}
	return h
}

// ownerHeaders returns gateway headers for a tenant_owner in the given tenant.
func ownerHeaders(userID, tenantID uuid.UUID) map[string]string {
	return gatewayHeaders(userID, tenantID, "tenant_owner")
}

// operatorHeaders returns gateway headers for a platform_operator. TenantID
// still must be supplied because RequireAuth requires it.
func operatorHeaders(userID uuid.UUID) map[string]string {
	return gatewayHeaders(userID, uuid.New(), "platform_operator")
}

// systemHeaders returns gateway headers for the iam-system principal used by
// internal routes.
func systemHeaders(tenantID uuid.UUID) map[string]string {
	return gatewayHeaders(uuid.New(), tenantID, "iam-system")
}

// ── Seed helpers ──────────────────────────────────────────────────────────

// seedTenant inserts a minimal tenants row via the raw superuser pool.
func (e *e2eEnv) seedTenant(t *testing.T, slug string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := e.rawPool.Exec(e.ctx, `
		INSERT INTO tenants (id, slug, name, plan, status, trial_ends_at, licensed_seats)
		VALUES ($1, $2, $3, 'starter', 'trial', now() + interval '30 days', 50)`,
		id, slug, slug)
	require.NoError(t, err)
	return id
}

// seedOwner inserts an active tenant_memberships row + a tenant_owner
// tenant_roles grant. Returns the user id.
func (e *e2eEnv) seedOwner(t *testing.T, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	userID := uuid.New()
	membershipID := uuid.New()
	_, err := e.rawPool.Exec(e.ctx, `
		INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
		VALUES ($1, $2, $3, 'active')`, membershipID, tenantID, userID)
	require.NoError(t, err)
	_, err = e.rawPool.Exec(e.ctx, `
		INSERT INTO tenant_roles (id, tenant_id, tenant_membership_id, user_id, role_code, granted_by)
		VALUES ($1, $2, $3, $4, 'tenant_owner', $4)`,
		uuid.New(), tenantID, membershipID, userID)
	require.NoError(t, err)
	return userID
}

// seedActiveMember inserts an active tenant_memberships row without any
// tenant role — used as a delegate target.
func (e *e2eEnv) seedActiveMember(t *testing.T, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	userID := uuid.New()
	_, err := e.rawPool.Exec(e.ctx, `
		INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
		VALUES ($1, $2, $3, 'active')`, uuid.New(), tenantID, userID)
	require.NoError(t, err)
	return userID
}

// ── Fake outbound clients (mirror phase4_helpers pattern) ────────────────

type fakeRealmProvisioner struct {
	CreateInvitedUserFailNext  bool
	DeleteUserFailNext         bool
	PatchRealmConfigFailNext   bool
	RevokeUserSessionsFailNext bool
	ResetMFAFailNext           bool
	PatchRealmConfigCalls      []port.RealmConfigPatch
}

func (f *fakeRealmProvisioner) CreateInvitedUser(_ context.Context, _ port.CreateInvitedUserRequest) (*port.CreateInvitedUserResponse, error) {
	if f.CreateInvitedUserFailNext {
		f.CreateInvitedUserFailNext = false
		return nil, fmt.Errorf("fake RP CreateInvitedUser outage")
	}
	return &port.CreateInvitedUserResponse{KeycloakUserID: uuid.New()}, nil
}
func (f *fakeRealmProvisioner) DeleteUser(_ context.Context, _, _ uuid.UUID) error {
	if f.DeleteUserFailNext {
		f.DeleteUserFailNext = false
		return fmt.Errorf("fake RP DeleteUser outage")
	}
	return nil
}
func (f *fakeRealmProvisioner) PatchRealmConfig(_ context.Context, _ uuid.UUID, p port.RealmConfigPatch) error {
	f.PatchRealmConfigCalls = append(f.PatchRealmConfigCalls, p)
	if f.PatchRealmConfigFailNext {
		f.PatchRealmConfigFailNext = false
		return fmt.Errorf("fake RP PatchRealmConfig outage")
	}
	return nil
}
func (f *fakeRealmProvisioner) RevokeUserSessions(_ context.Context, _, _ uuid.UUID) error {
	if f.RevokeUserSessionsFailNext {
		f.RevokeUserSessionsFailNext = false
		return fmt.Errorf("fake RP RevokeUserSessions outage")
	}
	return nil
}
func (f *fakeRealmProvisioner) ResetMFA(_ context.Context, _, _ uuid.UUID) error {
	if f.ResetMFAFailNext {
		f.ResetMFAFailNext = false
		return fmt.Errorf("fake RP ResetMFA outage")
	}
	return nil
}

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

// fakeCatalogDepartments is an in-memory stand-in for the departments that
// used to live in this service's own `departments` table (dropped per
// migration-runbook Phase 4 — LLD §12 step 4, now owned by the Catalog /
// Admin Config Service). Seeded with the same 5 system departments the
// dropped migration used to seed.
type fakeCatalogDepartments struct {
	mu   sync.Mutex
	rows map[uuid.UUID]domain.Department
}

func newFakeCatalogDepartments() *fakeCatalogDepartments {
	f := &fakeCatalogDepartments{rows: map[uuid.UUID]domain.Department{}}
	for _, code := range []string{"ENGINEERING", "DESIGN", "PROCUREMENT", "FINANCE", "LEGAL"} {
		f.add(domain.Department{Code: code, Name: code, IsSystem: true, IsActive: true})
	}
	return f
}

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

func (f *fakeCatalogDepartments) Departments(_ context.Context) ([]domain.Department, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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

// unmarshalBody decodes response bytes into v, failing the test on error.
func unmarshalBody(t *testing.T, body []byte, v any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(body, v),
		"unmarshal response body (raw=%s)", string(body))
}

// Silence unused-var if a helper is trimmed later.
var _ = pgx.ErrNoRows
