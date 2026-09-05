// auth_missing_header_test.go — router-level tests for auth/header rejection
// scenarios that require the full gincommon middleware stack (RequireAuth).
// These cannot be exercised by calling handlers directly because the handler
// unit-test helpers bypass the middleware chain.
//
// Test Case IDs covered:
//   - I16-AUTH-01 : missing x-user-id → 401
//   - I16-AUTH-02 : missing x-tenant-id → 401
//   - I16-AUTH-05 : invalid UUID in x-tenant-id → 401
//   - I16-ERR-02  : service error propagation (pool/db failure) → 503
//   - P34-AUTH-02 : unauthenticated reset-mfa (no x-user-id) → 401
package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// ── shared router builder for auth tests ─────────────────────────────────────

// authTestLapseRepo is a minimal TenantRepository for the auth header tests.
type authTestLapseRepo struct {
	port.TenantRepositoryNoop
}

func (r *authTestLapseRepo) ListSubscriptionLapses(_ context.Context, _ int) ([]domain.Tenant, error) {
	return nil, nil
}

var _ port.TenantRepository = (*authTestLapseRepo)(nil)

// buildAuthTestRouter returns a Router wired with the I-16 handler and
// (optionally) a MembershipHandler for P-34 tests.
func buildAuthTestRouter(lapseSvc *service.SubscriptionLapseService, memH *MembershipHandler) *Router {
	internalH := NewInternalHandler(nil, nil, nil, nil, nil, nil, lapseSvc)
	return NewRouter(RouterConfig{
		GinConfig:         gincommon.Config{ServiceName: "iam-org-membership-test"},
		InternalHandler:   internalH,
		MembershipHandler: memH,
	})
}

// doRequest fires a request against the router and returns the recorder.
func doRequest(router *Router, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, http.NoBody)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, req)
	return w
}

// ── I16-AUTH-01 : missing x-user-id → 401 ────────────────────────────────────

// Test Case ID: I16-AUTH-01
// Scenario: No x-user-id header sent → gincommon RequireAuth rejects → 401
// missing_identity_headers
func TestListSubscriptionLapses_MissingUserID_401(t *testing.T) {
	lapseSvc := service.NewSubscriptionLapseService(&authTestLapseRepo{}, 30)
	router := buildAuthTestRouter(lapseSvc, nil)

	w := doRequest(router, http.MethodGet, "/api/v1/internal/subscription-lapses", map[string]string{
		// x-user-id deliberately omitted
		"x-tenant-id":    uuid.Nil.String(),
		"x-tenant-roles": "iam-system",
	})

	assert.Equal(t, http.StatusUnauthorized, w.Code,
		"I16-AUTH-01: missing x-user-id must be rejected with 401")
}

// ── I16-AUTH-02 : missing x-tenant-id → 401 ──────────────────────────────────

// Test Case ID: I16-AUTH-02
// Scenario: No x-tenant-id header → gincommon RequireAuth rejects → 401.
// This was the original bug — RP's first implementation omitted x-tenant-id,
// which 401'd every real poll until the sentinel UUID was added on RP's side.
func TestListSubscriptionLapses_MissingTenantID_401(t *testing.T) {
	lapseSvc := service.NewSubscriptionLapseService(&authTestLapseRepo{}, 30)
	router := buildAuthTestRouter(lapseSvc, nil)

	w := doRequest(router, http.MethodGet, "/api/v1/internal/subscription-lapses", map[string]string{
		"x-user-id":      "iam-system",
		"x-tenant-roles": "iam-system",
		// x-tenant-id deliberately omitted
	})

	assert.Equal(t, http.StatusUnauthorized, w.Code,
		"I16-AUTH-02: missing x-tenant-id must be rejected with 401")
}

// ── I16-AUTH-05 : invalid UUID in x-tenant-id → 401 ─────────────────────────

// Test Case ID: I16-AUTH-05
// Scenario: x-tenant-id contains a non-UUID string. platform-gincommon treats
// this as a missing/malformed identity → 401 missing_identity_headers
// (not 400 — this is a platform middleware behaviour, not an O&M validation).
func TestListSubscriptionLapses_InvalidTenantIDUUID_401(t *testing.T) {
	lapseSvc := service.NewSubscriptionLapseService(&authTestLapseRepo{}, 30)
	router := buildAuthTestRouter(lapseSvc, nil)

	w := doRequest(router, http.MethodGet, "/api/v1/internal/subscription-lapses", map[string]string{
		"x-user-id":      "iam-system",
		"x-tenant-id":    "not-a-uuid", // malformed UUID
		"x-tenant-roles": "iam-system",
	})

	assert.Equal(t, http.StatusUnauthorized, w.Code,
		"I16-AUTH-05: invalid UUID in x-tenant-id must be rejected with 401")
}

// ── I16-ERR-02 : sysPool / service DB error → 503 ────────────────────────────

// Test Case ID: I16-ERR-02
// Scenario: The subscription-lapse service returns a DB-unavailable error
// (simulating sysPool connection exhaustion or a transient DB failure).
// The handler must propagate it as 503 dependency_unavailable rather than
// hanging or panicking.

// errLapseRepo is a TenantRepository whose ListSubscriptionLapses always
// returns a db_unavailable domain error — simulating pool exhaustion.
type errLapseRepo struct {
	port.TenantRepositoryNoop
}

func (r *errLapseRepo) ListSubscriptionLapses(_ context.Context, _ int) ([]domain.Tenant, error) {
	return nil, domain.NewError(domain.ErrDBUnavailable,
		"connection pool exhausted — max_connections reached")
}

var _ port.TenantRepository = (*errLapseRepo)(nil)

func TestListSubscriptionLapses_SysPoolExhausted_503(t *testing.T) {
	lapseSvc := service.NewSubscriptionLapseService(&errLapseRepo{}, 30)
	router := buildAuthTestRouter(lapseSvc, nil)

	w := doRequest(router, http.MethodGet, "/api/v1/internal/subscription-lapses", map[string]string{
		"x-user-id":      "iam-system",
		"x-tenant-id":    uuid.Nil.String(),
		"x-tenant-roles": "iam-system",
	})

	assert.Equal(t, http.StatusServiceUnavailable, w.Code,
		"I16-ERR-02: DB/pool error must surface as 503")
}

// ── P34-AUTH-02 : unauthenticated reset-mfa → 401 ────────────────────────────

// Test Case ID: P34-AUTH-02
// Scenario: POST reset-mfa with no x-user-id header → gincommon RequireAuth
// rejects before the handler is reached → 401 missing_identity_headers.
func TestResetMFA_Auth_Unauthenticated_401(t *testing.T) {
	// Wire a minimal MembershipHandler — it must never be reached.
	mem := &mhMemRepo{}
	svc := service.NewMembershipService(
		mem, &mhRoleRepo{}, &drhDeptMemRepo{},
		&happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{},
		&happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30,
	)
	memH := NewMembershipHandler(svc)
	router := buildAuthTestRouter(nil, memH)

	tenantID := uuid.New()
	userID := uuid.New()
	path := "/api/v1/tenants/" + tenantID.String() + "/members/" + userID.String() + "/reset-mfa"

	w := doRequest(router, http.MethodPost, path, map[string]string{
		// x-user-id deliberately omitted → RequireAuth must reject
		"x-tenant-id":    tenantID.String(),
		"x-tenant-roles": "tenant_admin",
	})

	assert.Equal(t, http.StatusUnauthorized, w.Code,
		"P34-AUTH-02: missing x-user-id must be rejected with 401 before hitting handler")
}

// ── I16-RLS-02 : RLS misconfiguration — sysPool uses app DSN ─────────────────

// Test Case ID: I16-RLS-02
// Scenario: When SYSTEM_DATABASE_URL is not set, sysPool reuses the app DSN.
// The app role has no BYPASSRLS privilege, so ListSubscriptionLapses sees
// only rows matching the GUC tenant (RLS-filtered), not all tenants.
//
// This scenario is simulated by calling ListSubscriptionLapses with an
// app-pool-backed mock that only returns results for the GUC-scoped tenant —
// verifying the contract that without BYPASSRLS the result is silently
// filtered rather than raising an error (which would break RP polling).
//
// In the real environment, this means RP would get an empty list for tenants
// it is not the GUC tenant of — a silent misconfiguration hazard. The correct
// fix (SYSTEM_DATABASE_URL set to the BYPASSRLS role) is tested in
// I16-RLS-01 (TestSubscriptionLapse_CrossTenant_AllReturnedOrderedByCancelledAtASC).

// rlsFilteredLapseRepo simulates an app-pool repo with RLS active: only the
// "in-scope" tenant appears, all others are invisible.
type rlsFilteredLapseRepo struct {
	port.TenantRepositoryNoop
	inScopeTenantID uuid.UUID
	allTenants      []domain.Tenant
}

func (r *rlsFilteredLapseRepo) ListSubscriptionLapses(_ context.Context, graceDays int) ([]domain.Tenant, error) {
	// Simulate RLS: return only the row whose tenant_id matches the GUC.
	var filtered []domain.Tenant
	for _, t := range r.allTenants {
		if t.ID == r.inScopeTenantID {
			filtered = append(filtered, t)
		}
	}
	return filtered, nil
}

var _ port.TenantRepository = (*rlsFilteredLapseRepo)(nil)

func TestListSubscriptionLapses_RLSMisconfiguration_SilentlyFiltered(t *testing.T) {
	// Seed two cancelled tenants that would both appear if BYPASSRLS were set.
	tenantA := uuid.New()
	tenantB := uuid.New()
	tA := makeCancelledTenant("realm-a", domain.RealmShared, 35)
	tA.ID = tenantA
	tB := makeCancelledTenant("realm-b", domain.RealmShared, 40)
	tB.ID = tenantB

	// Repo only returns tenantA (GUC-scoped, simulating RLS without BYPASSRLS).
	repo := &rlsFilteredLapseRepo{
		inScopeTenantID: tenantA,
		allTenants:      []domain.Tenant{tA, tB},
	}
	lapseSvc := service.NewSubscriptionLapseService(repo, 30)
	router := buildAuthTestRouter(lapseSvc, nil)

	w := doRequest(router, http.MethodGet, "/api/v1/internal/subscription-lapses", map[string]string{
		"x-user-id":      "iam-system",
		"x-tenant-id":    tenantA.String(), // GUC tenant = tenantA only
		"x-tenant-roles": "iam-system",
	})

	assert.Equal(t, http.StatusOK, w.Code)
	// With RLS misconfiguration: only tenantA appears, tenantB is invisible.
	// This silent filtering is the exact risk I16-RLS-02 documents — RP
	// would never see tenantB and never suspend it.
	assert.Contains(t, w.Body.String(), tenantA.String(),
		"RLS-scoped tenant must appear")
	assert.NotContains(t, w.Body.String(), tenantB.String(),
		"I16-RLS-02: cross-tenant tenant must be silently excluded by RLS — "+
			"this is the misconfiguration risk documented by this test case")
}
