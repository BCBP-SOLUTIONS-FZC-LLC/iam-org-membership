// I-19 · CheckAssigneeEligibility — handler and route tests: response shape
// for eligible / not-eligible, never-404, query validation, the path tenant
// bound as the RLS tenant, dependency errors surfacing, and the internal-
// caller gate (iam-system only; commercial-system is not on the allow-list).
package http

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type elhMemRepo struct {
	port.MembershipRepository
	m      *domain.TenantMembership
	err    error
	gucTID string // app.tenant_id bound on the ctx the service passed in
}

func (r *elhMemRepo) FindByUserID(ctx context.Context, _, _ uuid.UUID) (*domain.TenantMembership, error) {
	if g, ok := pgcommon.GUCSetFromContext(ctx); ok {
		r.gucTID = g.TenantID
	}
	return r.m, r.err
}

type elhTenantDeptRepo struct {
	port.TenantDepartmentRepository
	active bool
}

func (r *elhTenantDeptRepo) Find(_ context.Context, tid, did uuid.UUID) (*domain.TenantDepartment, error) {
	return &domain.TenantDepartment{TenantID: tid, DepartmentID: did, IsActive: r.active}, nil
}

type elhDeptMemRepo struct {
	port.DeptMembershipRepository
	dms []domain.DeptMembership
}

func (r *elhDeptMemRepo) ListByUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return r.dms, nil
}

func newEligibilityHandler(mem *elhMemRepo, deptID uuid.UUID, held domain.DeptRole) *InternalHandler {
	dm := &elhDeptMemRepo{dms: []domain.DeptMembership{{DepartmentID: deptID, RoleLevel: held}}}
	svc := service.NewDeptMembershipService(dm, mem, &elhTenantDeptRepo{active: true}, nil, nil, nil, nil, nil)
	return &InternalHandler{deptMemberships: svc}
}

func activeMem() *elhMemRepo {
	return &elhMemRepo{m: &domain.TenantMembership{ID: uuid.New(), Status: domain.MembershipActive}}
}

func callEligibility(h *InternalHandler, tenantID, userID string, query string) *eligibilityResult {
	c, w := buildCtx(http.MethodGet, "/?"+query, ``, systemCtx())
	setParams(c, "id", tenantID, "user_id", userID)
	h.CheckAssigneeEligibility(c)
	return &eligibilityResult{code: w.Code, body: w.Body.Bytes(), raw: w.Body.String()}
}

type eligibilityResult struct {
	code int
	body []byte
	raw  string
}

func (r *eligibilityResult) decode(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(r.body, &out), r.raw)
	return out
}

// I19-H-01: eligible → 200 with the echoed inputs and the held level, no reason.
func TestCheckAssigneeEligibility_Eligible(t *testing.T) {
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	h := newEligibilityHandler(activeMem(), deptID, domain.DeptApprover)

	res := callEligibility(h, tenantID.String(), userID.String(), "department_id="+deptID.String()+"&required_level=reviewer")
	require.Equal(t, http.StatusOK, res.code, res.raw)
	body := res.decode(t)
	assert.Equal(t, true, body["eligible"])
	assert.Equal(t, "approver", body["role_level"])
	assert.Equal(t, "reviewer", body["required_level"])
	assert.Equal(t, tenantID.String(), body["tenant_id"])
	assert.Equal(t, userID.String(), body["user_id"])
	assert.Equal(t, deptID.String(), body["department_id"])
	assert.NotContains(t, body, "reason", "reason is omitted when eligible")
}

// I19-H-02: level below required → 200 eligible:false, reason, current level.
func TestCheckAssigneeEligibility_LevelInsufficient(t *testing.T) {
	deptID := uuid.New()
	h := newEligibilityHandler(activeMem(), deptID, domain.DeptPreparator)

	res := callEligibility(h, uuid.New().String(), uuid.New().String(), "department_id="+deptID.String()+"&required_level=approver")
	require.Equal(t, http.StatusOK, res.code, res.raw)
	body := res.decode(t)
	assert.Equal(t, false, body["eligible"])
	assert.Equal(t, "level_insufficient", body["reason"])
	assert.Equal(t, "preparator", body["role_level"])
}

// I19-H-03: not a member → 200 eligible:false reason not_member, never 404.
func TestCheckAssigneeEligibility_NotMember_200Not404(t *testing.T) {
	deptID := uuid.New()
	mem := &elhMemRepo{err: domain.NewError(domain.ErrMemberNotFound, "no membership")}
	h := newEligibilityHandler(mem, deptID, domain.DeptApprover)

	res := callEligibility(h, uuid.New().String(), uuid.New().String(), "department_id="+deptID.String()+"&required_level=preparator")
	require.Equal(t, http.StatusOK, res.code, res.raw)
	body := res.decode(t)
	assert.Equal(t, false, body["eligible"])
	assert.Equal(t, "not_member", body["reason"])
	assert.NotContains(t, body, "role_level")
}

// I19-RLS-01: the path tenant — not the caller's x-tenant-id — is bound as
// app.tenant_id for the reads.
func TestCheckAssigneeEligibility_BindsPathTenantAsRLSTenant(t *testing.T) {
	pathTenant, deptID := uuid.New(), uuid.New()
	mem := activeMem()
	h := newEligibilityHandler(mem, deptID, domain.DeptApprover)

	c, w := buildCtx(http.MethodGet, "/?department_id="+deptID.String()+"&required_level=reviewer", ``, systemCtx())
	// Caller's own GUC carries a different tenant, as GUCBridgeMiddleware
	// would set from a mismatched x-tenant-id header.
	g, _ := pgcommon.GUCSetFromContext(c.Request.Context())
	g.TenantID = uuid.New().String()
	c.Request = c.Request.WithContext(pgcommon.WithGUCSet(c.Request.Context(), g))
	setParams(c, "id", pathTenant.String(), "user_id", uuid.New().String())

	h.CheckAssigneeEligibility(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, pathTenant.String(), mem.gucTID, "path :id must be the RLS tenant")
}

// I19-VAL-01..04: malformed ids / level → 400 VALIDATION_ERROR.
func TestCheckAssigneeEligibility_Validation(t *testing.T) {
	deptID := uuid.New().String()
	cases := map[string]struct{ tenant, user, query string }{
		"bad_tenant_id":      {"not-a-uuid", uuid.New().String(), "department_id=" + deptID + "&required_level=reviewer"},
		"bad_user_id":        {uuid.New().String(), "nope", "department_id=" + deptID + "&required_level=reviewer"},
		"missing_department": {uuid.New().String(), uuid.New().String(), "required_level=reviewer"},
		"bad_level":          {uuid.New().String(), uuid.New().String(), "department_id=" + deptID + "&required_level=admin"},
		"missing_level":      {uuid.New().String(), uuid.New().String(), "department_id=" + deptID},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newEligibilityHandler(activeMem(), uuid.New(), domain.DeptApprover)
			c, w := buildCtx(http.MethodGet, "/?"+tc.query, ``, systemCtx())
			setParams(c, "id", tc.tenant, "user_id", tc.user)
			h.CheckAssigneeEligibility(c)
			assertErrorCode(t, w, http.StatusBadRequest, "VALIDATION_ERROR")
		})
	}
}

// I19-ERR-01: a DB outage surfaces as 503, never as eligible:false.
func TestCheckAssigneeEligibility_DBUnavailable_503(t *testing.T) {
	deptID := uuid.New()
	mem := &elhMemRepo{err: domain.NewError(domain.ErrDBUnavailable, "db down")}
	h := newEligibilityHandler(mem, deptID, domain.DeptApprover)

	c, w := buildCtx(http.MethodGet, "/?department_id="+deptID.String()+"&required_level=reviewer", ``, systemCtx())
	setParams(c, "id", uuid.New().String(), "user_id", uuid.New().String())
	h.CheckAssigneeEligibility(c)
	assertErrorCode(t, w, http.StatusServiceUnavailable, "DB_UNAVAILABLE")
}

// I19-AUTH-01: iam-system passes the internal gate; commercial-system does not
// (I-19 is not on commercialSystemAllowedRoutes).
func TestCheckAssigneeEligibility_InternalCallerGate(t *testing.T) {
	const route = "/api/v1/internal/tenants/:id/members/:user_id/eligibility"
	sys := simulateRoute(t, http.MethodGet, route, &requestctx.RequestContext{Roles: []string{"iam-system"}})
	assert.Equal(t, http.StatusOK, sys.Code, "iam-system must reach I-19")

	com := simulateRoute(t, http.MethodGet, route, &requestctx.RequestContext{Roles: []string{"commercial-system"}})
	assert.Equal(t, http.StatusForbidden, com.Code, "commercial-system must be 403 on I-19")
}
