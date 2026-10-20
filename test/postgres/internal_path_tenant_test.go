//go:build integration

// Package postgres_test — regression suite for the path-tenant binding on
// /api/v1/internal/tenants/:id/* (BindPathTenant) and I-8's ?tenant_id=,
// through the real router and real Postgres with RLS on.
//
// Every case puts the data in tenant B, names B in the path (or I-8's
// ?tenant_id=) and sends x-tenant-id: A. Repositories filter on the path
// tenant while RLS filtered on the header tenant, so before the binding each
// read found no rows (false 404 / active:false / not_member) and each write
// failed the RLS WITH CHECK (logged to rls_violation_log). After it, every
// route answers for B and no RLS violation is recorded.
package postgres_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/http"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
)

func pathTenantRouter(fx *testFixtures, strict bool) http.Handler {
	return httpadapter.NewRouter(httpadapter.RouterConfig{
		GinConfig: gincommon.Config{Domain: "iam", Environment: "test", ServiceName: "iam-org-membership"},
		InternalHandler: httpadapter.NewInternalHandler(fx.Provisioning, fx.AuthZ, fx.Membership, fx.Invitation,
			fx.GroupMapping, fx.Tenant, fx.SubscriptionLapse, fx.DeptMembership),
		StrictTenantPathValidation: strict,
	}).Handler()
}

// callInternal sends an iam-system request whose x-tenant-id is headerTenant.
func callInternal(t *testing.T, h http.Handler, method, path string, headerTenant uuid.UUID, body any) (int, map[string]any) {
	t.Helper()
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, bytes.NewReader(buf))
	req.Header.Set("x-user-id", "iam-system")
	req.Header.Set("x-tenant-id", headerTenant.String())
	req.Header.Set("x-tenant-roles", "iam-system")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func rlsViolationCount(t *testing.T, fx *testFixtures) int {
	t.Helper()
	var n int
	require.NoError(t, fx.rawPool.QueryRow(context.Background(), `SELECT count(*) FROM rls_violation_log`).Scan(&n))
	return n
}

// pathTenantSetup: tenant B with an owner holding approver in an active
// department; tenant A is only ever the (wrong) header tenant.
type pathTenantSetup struct {
	headerA, tenantB, ownerB, deptB uuid.UUID
}

func setupPathTenant(t *testing.T, fx *testFixtures, slug string) pathTenantSetup {
	t.Helper()
	ctx := context.Background()
	tenantA, _ := seedTenantWithOwner(t, ctx, fx, slug+"-a")
	tenantB, ownerB := seedTenantWithOwner(t, ctx, fx, slug+"-b")
	deptB := uuid.New()
	_, err := fx.rawPool.Exec(ctx, `INSERT INTO tenant_departments (tenant_id, department_id) VALUES ($1, $2)`, tenantB, deptB)
	require.NoError(t, err)
	_, err = fx.rawPool.Exec(ctx, `
		INSERT INTO dept_memberships (tenant_id, user_id, tenant_membership_id, department_id, role_level, granted_by)
		SELECT $1, $2, tm.id, $3, 'approver', $2 FROM tenant_memberships tm WHERE tm.tenant_id = $1 AND tm.user_id = $2`,
		tenantB, ownerB, deptB)
	require.NoError(t, err)
	return pathTenantSetup{headerA: tenantA, tenantB: tenantB, ownerB: ownerB, deptB: deptB}
}

func tenantPath(id uuid.UUID, suffix string) string {
	return "/api/v1/internal/tenants/" + id.String() + suffix
}

// Reads: each answers for the path tenant despite x-tenant-id: A.
func TestPathTenant_Reads_MismatchedHeader(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	s := setupPathTenant(t, fx, "pt-reads")
	h := pathTenantRouter(fx, false)

	cases := []struct {
		name, path string
		check      func(t *testing.T, code int, body map[string]any)
	}{
		{"I-8 memberships", "/api/v1/internal/users/" + s.ownerB.String() + "/memberships?tenant_id=" + s.tenantB.String(),
			func(t *testing.T, code int, body map[string]any) {
				require.Equal(t, http.StatusOK, code, body)
				assert.Equal(t, "active", body["status"])
			}},
		{"I-9 locale", tenantPath(s.tenantB, "/locale"),
			func(t *testing.T, code int, body map[string]any) { require.Equal(t, http.StatusOK, code, body) }},
		{"I-11 seat-usage", tenantPath(s.tenantB, "/seat-usage"),
			func(t *testing.T, code int, body map[string]any) {
				require.Equal(t, http.StatusOK, code, body)
				assert.EqualValues(t, 1, body["active_users"], "the owner is B's one active member: %v", body)
			}},
		{"I-14 mfa-freshness", tenantPath(s.tenantB, "/mfa-freshness"),
			func(t *testing.T, code int, body map[string]any) { require.Equal(t, http.StatusOK, code, body) }},
		{"I-17 plan-context", tenantPath(s.tenantB, "/plan-context"),
			func(t *testing.T, code int, body map[string]any) {
				require.Equal(t, http.StatusOK, code, body)
				assert.Equal(t, s.tenantB.String(), body["tenant_id"])
			}},
		// The fixture's RP fake has no IdP count, so a found tenant reaches
		// step 4 and answers 503; before the binding it stopped at step 2
		// with 404 TENANT_NOT_FOUND.
		{"I-18 plan-change-check", tenantPath(s.tenantB, "/plan-change-check?target_plan=starter"),
			func(t *testing.T, code int, body map[string]any) {
				assert.NotEqual(t, "TENANT_NOT_FOUND", body["errorCode"], body)
				assert.Equal(t, "REALM_PROVISIONER_UNAVAILABLE", body["errorCode"], body)
			}},
		{"I-15 exists", tenantPath(s.tenantB, "/members/"+s.ownerB.String()+"/exists"),
			func(t *testing.T, code int, body map[string]any) {
				require.Equal(t, http.StatusOK, code, body)
				assert.Equal(t, true, body["active"])
			}},
		{"I-19 eligibility", tenantPath(s.tenantB, "/members/"+s.ownerB.String()+"/eligibility?department_id="+s.deptB.String()+"&required_level=reviewer"),
			func(t *testing.T, code int, body map[string]any) {
				require.Equal(t, http.StatusOK, code, body)
				assert.Equal(t, true, body["eligible"], body)
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := callInternal(t, h, http.MethodGet, tc.path, s.headerA, nil)
			tc.check(t, code, body)
		})
	}
}

// Writes: succeed in the path tenant and record no RLS violation.
func TestPathTenant_Writes_MismatchedHeader_NoRLSViolation(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	s := setupPathTenant(t, fx, "pt-writes")
	h := pathTenantRouter(fx, false)
	ctx := context.Background()
	before := rlsViolationCount(t, fx)

	t.Run("I-3 add member", func(t *testing.T) {
		newUser := uuid.New()
		code, body := callInternal(t, h, http.MethodPost, tenantPath(s.tenantB, "/members"), s.headerA,
			map[string]any{"user_id": newUser, "keycloak_user_id": newUser, "email": "pt-" + newUser.String()[:8] + "@example.com"})
		require.Equal(t, http.StatusCreated, code, body)
		var n int
		require.NoError(t, fx.rawPool.QueryRow(ctx,
			`SELECT count(*) FROM tenant_memberships WHERE tenant_id = $1 AND user_id = $2`, s.tenantB, newUser).Scan(&n))
		assert.Equal(t, 1, n, "the member must land in the path tenant")
	})
	t.Run("I-10 SAML JIT", func(t *testing.T) {
		jitDept := uuid.New()
		_, err := fx.rawPool.Exec(ctx, `INSERT INTO tenant_departments (tenant_id, department_id) VALUES ($1, $2)`, s.tenantB, jitDept)
		require.NoError(t, err)
		fx.GroupMappingClient.resolveFn = func(context.Context, uuid.UUID, []string) (*port.GroupResolution, error) {
			return &port.GroupResolution{
				DeptMappings:     []port.ResolvedDeptMapping{{KeycloakGroupName: "eng", DepartmentID: jitDept}},
				DeptRoleMappings: []port.ResolvedDeptRoleMapping{{KeycloakGroupName: "eng", RoleCode: domain.DeptReviewer}},
			}, nil
		}
		code, body := callInternal(t, h, http.MethodPost, tenantPath(s.tenantB, "/dept-memberships"), s.headerA,
			map[string]any{"user_id": s.ownerB, "groups": []string{"eng"}})
		require.Equal(t, http.StatusOK, code, body)
		var n int
		require.NoError(t, fx.rawPool.QueryRow(ctx,
			`SELECT count(*) FROM dept_memberships WHERE tenant_id = $1 AND user_id = $2 AND department_id = $3 AND deleted_at IS NULL`,
			s.tenantB, s.ownerB, jitDept).Scan(&n))
		assert.Equal(t, 1, n, "the JIT grant must land in the path tenant")
	})
	t.Run("I-13 assignee override", func(t *testing.T) {
		code, body := callInternal(t, h, http.MethodPost, tenantPath(s.tenantB, "/tenders/"+uuid.New().String()+"/assignee-override"), s.headerA,
			map[string]any{"new_user_id": s.ownerB, "department_id": s.deptB, "required_level": "reviewer", "actor_id": s.ownerB})
		require.Equal(t, http.StatusOK, code, body)
		assert.Equal(t, true, body["eligible"])
	})

	assert.Equal(t, before, rlsViolationCount(t, fx), "a mismatched x-tenant-id must no longer produce RLS violations")
}

// The binding never widens access: path tenant A, data in B, header B —
// answered for A (not a member), never with B's data.
func TestPathTenant_NoCrossTenantAnswer(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	s := setupPathTenant(t, fx, "pt-cross")
	h := pathTenantRouter(fx, false)

	code, body := callInternal(t, h, http.MethodGet, tenantPath(s.headerA, "/members/"+s.ownerB.String()+"/exists"), s.tenantB, nil)
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, false, body["active"])

	code, body = callInternal(t, h, http.MethodGet,
		tenantPath(s.headerA, "/members/"+s.ownerB.String()+"/eligibility?department_id="+s.deptB.String()+"&required_level=preparator"), s.tenantB, nil)
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, "not_member", body["reason"])
}

// Strict mode: the mismatch is rejected before the handler — no write.
func TestPathTenant_Strict_RejectsBeforeHandler(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	s := setupPathTenant(t, fx, "pt-strict")
	h := pathTenantRouter(fx, true)

	newUser := uuid.New()
	code, body := callInternal(t, h, http.MethodPost, tenantPath(s.tenantB, "/members"), s.headerA,
		map[string]any{"user_id": newUser, "keycloak_user_id": newUser, "email": "strict@example.com"})
	require.Equal(t, http.StatusBadRequest, code, body)
	assert.Equal(t, "TENANT_MISMATCH", body["errorCode"])

	var n int
	require.NoError(t, fx.rawPool.QueryRow(context.Background(),
		`SELECT count(*) FROM tenant_memberships WHERE user_id = $1`, newUser).Scan(&n))
	assert.Zero(t, n, "strict mode must reject before the handler writes anything")

	// A matching header is served normally in strict mode.
	code, body = callInternal(t, h, http.MethodGet, tenantPath(s.tenantB, "/locale"), s.tenantB, nil)
	assert.Equal(t, http.StatusOK, code, body)
}
