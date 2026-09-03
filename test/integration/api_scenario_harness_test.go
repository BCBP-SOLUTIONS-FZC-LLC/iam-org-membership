//go:build integration

// Package integration_test — API scenario tests.
// Runs as part of make test-integration.
// No external mock servers required — all external service clients are
// replaced with Go-level interface fakes. PostgreSQL is a real testcontainer.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/http"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/migrate"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// ── Fixed department IDs — match the mockserver so seeded dept IDs are stable ──
var (
	DeptEngID  = uuid.MustParse("de010001-0000-0000-0000-000000000001")
	DeptDesID  = uuid.MustParse("de010002-0000-0000-0000-000000000002")
	DeptProID  = uuid.MustParse("de010003-0000-0000-0000-000000000003")
	DeptFinID  = uuid.MustParse("de010004-0000-0000-0000-000000000004")
	DeptLegID  = uuid.MustParse("de010005-0000-0000-0000-000000000005")
	DeptOpsID  = uuid.MustParse("de010006-0000-0000-0000-000000000006") // non-system, is_active=true
)

// ── Go-level fakes for all external service clients ───────────────────────────

// scenarioCatalogClient returns the same fixed dept/plan data as the mockserver.
type scenarioCatalogClient struct{}

func (c *scenarioCatalogClient) Departments(_ context.Context) ([]port.CatalogDepartment, error) {
	return []port.CatalogDepartment{
		{ID: DeptEngID, Code: "ENGINEERING", Name: "Engineering", IsSystem: true, IsActive: true, RecordVersion: 1},
		{ID: DeptDesID, Code: "DESIGN", Name: "Design", IsSystem: true, IsActive: true, RecordVersion: 1},
		{ID: DeptProID, Code: "PROCUREMENT", Name: "Procurement", IsSystem: true, IsActive: true, RecordVersion: 1},
		{ID: DeptFinID, Code: "FINANCE", Name: "Finance", IsSystem: true, IsActive: true, RecordVersion: 1},
		{ID: DeptLegID, Code: "LEGAL", Name: "Legal", IsSystem: true, IsActive: true, RecordVersion: 1},
		{ID: DeptOpsID, Code: "OPERATIONS", Name: "Operations", IsSystem: false, IsActive: true, RecordVersion: 1},
	}, nil
}

func (c *scenarioCatalogClient) Plans(_ context.Context) ([]port.CatalogPlan, error) {
	return []port.CatalogPlan{
		{Code: "starter", DisplayName: "Starter", TrialDurationDays: 30, SSOEnabled: false,
			CustomBranding: "none", FeatureSet: map[string]any{}, RecordVersion: 1},
		{Code: "pro", DisplayName: "Professional", TrialDurationDays: 30, SSOEnabled: true,
			CustomBranding: "logo", FeatureSet: map[string]any{"advanced_reporting": true}, RecordVersion: 1},
		{Code: "enterprise", DisplayName: "Enterprise", TrialDurationDays: 30, SSOEnabled: true,
			CustomBranding: "logo", FeatureSet: map[string]any{"advanced_reporting": true, "custom_roles": true}, RecordVersion: 1},
	}, nil
}

var _ port.CatalogAdminClient = (*scenarioCatalogClient)(nil)

// scenarioRPClient — Realm Provisioner fake. Returns plausible KC UUIDs, no-ops on revoke.
type scenarioRPClient struct{}

func (r *scenarioRPClient) CreateInvitedUser(_ context.Context, _ port.CreateInvitedUserRequest) (*port.CreateInvitedUserResponse, error) {
	return &port.CreateInvitedUserResponse{KeycloakUserID: uuid.New()}, nil
}
func (r *scenarioRPClient) DeleteUser(_ context.Context, _, _ uuid.UUID) error       { return nil }
func (r *scenarioRPClient) PatchRealmConfig(_ context.Context, _ uuid.UUID, _ port.RealmConfigPatch) error {
	return nil
}
func (r *scenarioRPClient) RevokeUserSessions(_ context.Context, _, _ uuid.UUID) error { return nil }
func (r *scenarioRPClient) ResetMFA(_ context.Context, _, _ uuid.UUID) error            { return nil }

var _ port.RealmProvisionerClient = (*scenarioRPClient)(nil)

// scenarioWorkflowClient — Workflow fake. Reports 0 active workflows (allows removal).
type scenarioWorkflowClient struct{}

func (w *scenarioWorkflowClient) GetDelegateImpact(_ context.Context, _, _ uuid.UUID, _ *uuid.UUID) (*port.DelegateImpact, error) {
	return &port.DelegateImpact{ActiveWorkflows: 0, WorkflowIDs: nil}, nil
}
func (w *scenarioWorkflowClient) ReassignDelegate(_ context.Context, _, _, _ uuid.UUID, _ *uuid.UUID) error {
	return nil
}
func (w *scenarioWorkflowClient) CancelByDelegate(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ *uuid.UUID) error {
	return nil
}

var _ port.WorkflowClient = (*scenarioWorkflowClient)(nil)

// scenarioDelegationCheckClient — fails open (returns nil delegate, no outage).
type scenarioDelegationCheckClient struct{}

func (d *scenarioDelegationCheckClient) DeptDelegate(_ context.Context, _, _, _ uuid.UUID) (*uuid.UUID, error) {
	return nil, nil
}

var _ port.DelegationCheckClient = (*scenarioDelegationCheckClient)(nil)

// scenarioGroupMappingClient — fails open (empty resolution).
type scenarioGroupMappingClient struct{}

func (g *scenarioGroupMappingClient) ResolveGroups(_ context.Context, _ uuid.UUID, _ []string) (*port.GroupResolution, error) {
	return &port.GroupResolution{}, nil
}

var _ port.GroupMappingClient = (*scenarioGroupMappingClient)(nil)

// ── Test server ───────────────────────────────────────────────────────────────

// apiTestEnv holds everything a scenario test needs.
type apiTestEnv struct {
	server  *httptest.Server
	rawPool *pgxpool.Pool // superuser — bypasses RLS for seeding
	client  *http.Client
}

// do executes an HTTP request against the test server and returns the response.
func (e *apiTestEnv) do(t *testing.T, method, path, body string, hdrs map[string]string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, e.server.URL+path, r)
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	require.NoError(t, err)
	return resp
}

// isSys returns headers for an iam-system caller.
func isSys(tenantID string) map[string]string {
	return map[string]string{
		"x-user-id":    "00000000-0000-0000-0000-000000000001",
		"x-tenant-id":  tenantID,
		"x-tenant-roles": "iam-system",
	}
}

// isOwner returns headers for a tenant_owner caller.
func isOwner(ownerID, tenantID string) map[string]string {
	return map[string]string{
		"x-user-id":    ownerID,
		"x-tenant-id":  tenantID,
		"x-tenant-roles": "tenant_owner",
	}
}

// isAdmin returns headers for a tenant_admin caller.
func isAdmin(adminID, tenantID string) map[string]string {
	return map[string]string{
		"x-user-id":    adminID,
		"x-tenant-id":  tenantID,
		"x-tenant-roles": "tenant_admin",
	}
}

// isMember returns headers for a plain member caller.
func isMember(userID, tenantID string) map[string]string {
	return map[string]string{
		"x-user-id":    userID,
		"x-tenant-id":  tenantID,
		"x-tenant-roles": "member",
	}
}

// isOperator returns headers for a platform_operator caller.
func isOperator(tenantID string) map[string]string {
	return map[string]string{
		"x-user-id":    "00000000-0000-0000-0000-000000000001",
		"x-tenant-id":  tenantID,
		"x-tenant-roles": "platform_operator",
	}
}

// noAuth returns headers without auth (simulates missing identity).
func noAuth() map[string]string { return map[string]string{} }

// json helpers
func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func rv(resp *http.Response) int64 {
	var b map[string]any
	body, _ := io.ReadAll(resp.Body)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	_ = json.Unmarshal(body, &b)
	if v, ok := b["record_version"]; ok {
		switch x := v.(type) {
		case float64:
			return int64(x)
		}
	}
	return 1
}

func parseBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	return out
}

// ── Test server construction ──────────────────────────────────────────────────

const (
	scenarioAppRolePassword  = "apppass-scenario"
	scenarioMigratorPassword = "migpass-scenario"
)

// newAPITestEnv boots a PostgreSQL testcontainer, applies migrations (with
// full RLS role setup), wires all service layers with Go-level fakes for
// external clients, and starts a local httptest.Server.
// No external mock servers are needed.
func newAPITestEnv(t *testing.T) *apiTestEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping API scenario test in short mode")
	}
	ctx := context.Background()

	// ── Postgres testcontainer ─────────────────────────────────────────
	pgC, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("org_membership"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("testpassword"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err, "start postgres testcontainer")
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })

	superDSN, err := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	rawPool, err := pgxpool.New(ctx, superDSN)
	require.NoError(t, err)
	t.Cleanup(rawPool.Close)

	// Create runtime roles before migrations
	_, err = rawPool.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE org_membership_app LOGIN PASSWORD '%s' NOBYPASSRLS`, scenarioAppRolePassword))
	require.NoError(t, err)
	_, err = rawPool.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE org_membership_migrator LOGIN PASSWORD '%s' BYPASSRLS`, scenarioMigratorPassword))
	require.NoError(t, err)

	// Apply outbox schema then domain migrations
	require.NoError(t, outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: superDSN}))
	require.NoError(t, pgadapter.RunMigrations(ctx, superDSN))

	// Grant privileges to app role
	for _, stmt := range []string{
		`GRANT CONNECT ON DATABASE org_membership TO org_membership_app`,
		`GRANT USAGE ON SCHEMA public TO org_membership_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO org_membership_app`,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO org_membership_app`,
		`GRANT EXECUTE ON FUNCTION app_tenant_id()                    TO org_membership_app`,
		`GRANT EXECUTE ON FUNCTION log_rls_violation(text, uuid, text) TO org_membership_app`,
		`GRANT EXECUTE ON FUNCTION rls_check_tenant(uuid, text)       TO org_membership_app`,
	} {
		_, err = rawPool.Exec(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	// RLS-enforcing app pool (mirrors production)
	appDSN := strings.Replace(superDSN, "postgres:testpassword@",
		"org_membership_app:"+scenarioAppRolePassword+"@", 1)
	appPool, err := pgcommon.NewPool(ctx, pgcommon.Config{
		DSN:           appDSN,
		PGBouncerMode: false,
		GUCProvider:   pgcommon.GUCSetFromContext,
	})
	require.NoError(t, err)
	t.Cleanup(appPool.Close)

	// Superuser pool (no GUC, no RLS — for iam-system paths)
	sysPool, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: superDSN})
	require.NoError(t, err)
	t.Cleanup(sysPool.Close)

	// ── External service fakes (zero external servers) ─────────────────
	catalogClient := &scenarioCatalogClient{}
	rpClient      := &scenarioRPClient{}
	wfClient      := &scenarioWorkflowClient{}
	delClient     := &scenarioDelegationCheckClient{}
	gmClient      := &scenarioGroupMappingClient{}

	// ── Repositories ───────────────────────────────────────────────────
	tenantRepo      := pgadapter.NewTenantRepository(appPool)
	tenantDeptRepo  := pgadapter.NewTenantDepartmentRepository(appPool)
	membershipRepo  := pgadapter.NewMembershipRepository(appPool)
	tenantRoleRepo  := pgadapter.NewTenantRoleRepository(appPool)
	deptMemRepo     := pgadapter.NewDeptMembershipRepository(appPool)
	deptRoleLabelRepo := pgadapter.NewDeptRoleLabelRepository(appPool)
	invitationRepo  := pgadapter.NewInvitationRepository(appPool)

	txRunner := pgadapter.NewTxRunner(appPool, nil) // nil publisher — outbox not needed for HTTP tests

	// ── Catalog + group mapping services (read-through cache) ──────────
	catalogSvc := service.NewCatalogService(catalogClient, nil) // nil cache → always hits fake client
	gmSvc      := service.NewGroupMappingService(membershipRepo, tenantRoleRepo, deptMemRepo, txRunner, nil, gmClient)

	// ── Core services ──────────────────────────────────────────────────
	const (
		invitationExpiryDays = 7
		seatOverageDays      = 30
	)

	authzSvc       := service.NewAuthZService(pgadapter.NewAuthZRepository(appPool), catalogSvc, catalogSvc, nil)
	provisioningSvc := service.NewProvisioningService(
		tenantRepo, membershipRepo, tenantRoleRepo, deptMemRepo,
		deptRoleLabelRepo, tenantDeptRepo, catalogSvc, catalogSvc,
		txRunner, nil, rpClient,
	)
	tenantSvc      := service.NewTenantService(tenantRepo, nil, rpClient)
	deptSvc        := service.NewDepartmentService(catalogSvc, tenantDeptRepo, nil)
	membershipSvc  := service.NewMembershipService(
		membershipRepo, tenantRoleRepo, deptMemRepo, tenantRepo,
		invitationRepo, nil, rpClient, wfClient, txRunner, nil, seatOverageDays,
	)
	deptMemSvc := service.NewDeptMembershipService(
		deptMemRepo, membershipRepo, tenantDeptRepo, catalogSvc,
		delClient, wfClient, nil, txRunner,
	)
	roleLabelSvc   := service.NewRoleLabelService(deptRoleLabelRepo, nil)
	invitationSvc  := service.NewInvitationService(
		invitationRepo, membershipRepo, tenantRoleRepo, deptMemRepo,
		tenantRepo, rpClient, nil, txRunner, nil, invitationExpiryDays,
	)
	operatorSvc := service.NewOperatorService(
		tenantRepo, tenantRoleRepo, membershipRepo, nil, txRunner,
	)

	// ── HTTP handlers + router ─────────────────────────────────────────
	tenantH    := httpadapter.NewTenantHandler(tenantSvc)
	deptH      := httpadapter.NewDepartmentHandler(deptSvc)
	memberH    := httpadapter.NewMembershipHandler(membershipSvc)
	deptMemH   := httpadapter.NewDeptMembershipHandler(deptMemSvc)
	roleLabelH := httpadapter.NewRoleLabelHandler(roleLabelSvc)
	inviteH    := httpadapter.NewInvitationHandler(invitationSvc)
	operatorH  := httpadapter.NewOperatorHandler(operatorSvc)
	subscriptionLapseSvc := service.NewSubscriptionLapseService(pgadapter.NewTenantRepository(sysPool), 30)
	internalH  := httpadapter.NewInternalHandler(
		provisioningSvc, authzSvc, membershipSvc, invitationSvc, gmSvc, tenantSvc, subscriptionLapseSvc,
	)

	_ = sysPool // available for future system-level ops in tests

	router := httpadapter.NewRouter(httpadapter.RouterConfig{
		GinConfig:             gincommon.Config{ServiceName: "iam-org-membership-test", BuildVersion: "test"},
		TenantRepo:            tenantRepo,
		MembershipRepo:        membershipRepo,
		TenantHandler:         tenantH,
		DepartmentHandler:     deptH,
		MembershipHandler:     memberH,
		DeptMembershipHandler: deptMemH,
		RoleLabelHandler:      roleLabelH,
		InvitationHandler:     inviteH,
		OperatorHandler:       operatorH,
		InternalHandler:       internalH,
	})

	srv := httptest.NewServer(router.Handler())
	t.Cleanup(srv.Close)

	return &apiTestEnv{
		server:  srv,
		rawPool: rawPool,
		client:  &http.Client{CheckRedirect: func(r *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

// ── Seeding helpers ───────────────────────────────────────────────────────────

// provisionTenant calls I-1 to create a tenant and returns its ID.
// Uses the iam-system header.
func (e *apiTestEnv) provisionTenant(t *testing.T, tenantID, ownerID, slug, name string) {
	t.Helper()
	body := toJSON(map[string]any{
		"tenant_id":      tenantID,
		"slug":           slug,
		"name":           name,
		"plan":           "starter",
		"licensed_seats": 10,
		"locale":         "en-US",
		"owner_user_id":  ownerID,
		"owner_email":    ownerID + "@test.com",
		"owner_name":     "Test Owner",
	})
	resp := e.do(t, http.MethodPost, "/api/v1/internal/tenants", body, isSys(tenantID))
	defer resp.Body.Close()
	require.True(t, resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK,
		"provisionTenant: got %d", resp.StatusCode)
}

// addMember calls I-3 to add a plain member and returns their record_version.
func (e *apiTestEnv) addMember(t *testing.T, tenantID, userID, email, fullName string) int64 {
	t.Helper()
	body := toJSON(map[string]any{
		"user_id":    userID,
		"email":      email,
		"full_name":  fullName,
	})
	resp := e.do(t, http.MethodPost, "/api/v1/internal/tenants/"+tenantID+"/members", body, isSys(tenantID))
	defer resp.Body.Close()
	require.True(t, resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK,
		"addMember %s: got %d", userID, resp.StatusCode)
	b := parseBody(t, resp)
	if v, ok := b["record_version"].(float64); ok {
		return int64(v)
	}
	return 1
}

// suspendMember calls I-4 to suspend a member.
func (e *apiTestEnv) suspendMember(t *testing.T, tenantID, userID string, recordVersion int64) {
	t.Helper()
	body := toJSON(map[string]any{"status": "suspended", "record_version": recordVersion})
	resp := e.do(t, http.MethodPatch, "/api/v1/internal/tenants/"+tenantID+"/members/"+userID,
		body, isSys(tenantID))
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "suspendMember %s", userID)
}

// grantRole calls P-28 (ReconcileRoles) to set roles for a member.
func (e *apiTestEnv) grantRole(t *testing.T, tenantID, ownerID, userID string, roles []string, recordVersion int64) {
	t.Helper()
	body := toJSON(map[string]any{"roles": roles, "record_version": recordVersion})
	resp := e.do(t, http.MethodPut, "/api/v1/tenants/"+tenantID+"/members/"+userID+"/roles",
		body, isOwner(ownerID, tenantID))
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "grantRole %v to %s", roles, userID)
}

// assignDept calls P-10 to assign a member to a department.
func (e *apiTestEnv) assignDept(t *testing.T, tenantID, ownerID, userID, deptID, level string) {
	t.Helper()
	body := toJSON(map[string]any{"level": level, "record_version": 1})
	resp := e.do(t, http.MethodPut,
		"/api/v1/tenants/"+tenantID+"/departments/"+deptID+"/members/"+userID,
		body, isOwner(ownerID, tenantID))
	defer resp.Body.Close()
	// 200 = updated, 201 = created — both are fine
	require.True(t, resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated,
		"assignDept %s → %s: got %d", userID, deptID, resp.StatusCode)
}

// getTenantRV returns the current record_version of a tenant (via P-1).
func (e *apiTestEnv) getTenantRV(t *testing.T, tenantID, callerID string) int64 {
	t.Helper()
	resp := e.do(t, http.MethodGet, "/api/v1/tenants/"+tenantID, "",
		isOwner(callerID, tenantID))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return rv(resp)
}

// getMemberRV returns the record_version of a member row (via P-5).
func (e *apiTestEnv) getMemberRV(t *testing.T, tenantID, callerID, userID string) int64 {
	t.Helper()
	resp := e.do(t, http.MethodGet,
		"/api/v1/tenants/"+tenantID+"/members/"+userID, "",
		isOwner(callerID, tenantID))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Try iam-system path (bypasses membership check)
		resp2 := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tenantID+"/members/"+userID, "",
			isSys(tenantID))
		defer resp2.Body.Close()
		return rv(resp2)
	}
	return rv(resp)
}

// freshID returns a new random UUID string.
func freshID() string { return uuid.New().String() }

// freshTenant returns a unique tenant UUID in the cccc* namespace.
var tenantCounter = 100

func freshTenantID() string {
	tenantCounter++
	return fmt.Sprintf("cccc%04d-0001-0001-0001-000000000001", tenantCounter)
}

// assertStatus asserts the HTTP status code and returns the parsed body.
func assertStatus(t *testing.T, resp *http.Response, want int) map[string]any {
	t.Helper()
	body := parseBody(t, resp)
	require.Equal(t, want, resp.StatusCode,
		"unexpected status — body: %v", body)
	return body
}

// domainMatcher checks that a body field matches an expected value.
func hasField(t *testing.T, body map[string]any, key string, want any) {
	t.Helper()
	got, ok := body[key]
	require.True(t, ok, "response missing field %q — body: %v", key, body)
	require.Equal(t, want, got, "field %q mismatch — body: %v", key, body)
}

// hasCode checks the error code in an error response.
func hasCode(t *testing.T, body map[string]any, wantCode string) {
	t.Helper()
	got, _ := body["code"].(string)
	require.Equal(t, wantCode, got, "error code mismatch — body: %v", body)
}

// ── Shared test fixtures ──────────────────────────────────────────────────────

// testTenant bundles a provisioned tenant with its owner, admin, member, and
// suspended member so scenario tests can reuse without re-seeding.
type testTenant struct {
	TenantID    string
	OwnerID     string
	AdminID     string
	MemberID    string
	SuspendedID string
}

// newTestTenant provisions a complete test tenant.
func newTestTenant(t *testing.T, e *apiTestEnv) *testTenant {
	t.Helper()
	tt := &testTenant{
		TenantID:    freshTenantID(),
		OwnerID:     freshID(),
		AdminID:     freshID(),
		MemberID:    freshID(),
		SuspendedID: freshID(),
	}

	// Provision tenant (also activates 5 system depts and seeds owner role)
	e.provisionTenant(t, tt.TenantID, tt.OwnerID, "test-"+tt.TenantID[:8], "Test Corp")

	// Add admin and plain member
	adminRV  := e.addMember(t, tt.TenantID, tt.AdminID,  "admin@test.com",  "Test Admin")
	memberRV := e.addMember(t, tt.TenantID, tt.MemberID, "member@test.com", "Test Member")
	suspRV   := e.addMember(t, tt.TenantID, tt.SuspendedID, "susp@test.com", "Suspended")

	// Grant admin role
	e.grantRole(t, tt.TenantID, tt.OwnerID, tt.AdminID, []string{"tenant_admin"}, adminRV)

	// Confirm member record_version (after potential role reconcile)
	_ = memberRV

	// Suspend the suspended user
	e.suspendMember(t, tt.TenantID, tt.SuspendedID, suspRV)

	return tt
}

// domainFromString returns a domain.TenantPlan from its code string.
func domainPlan(s string) domain.TenantPlan { return domain.TenantPlan(s) }
