// Unit tests for internal/core/service/session_context_service.go (P-35,
// GET /api/v1/me/session-context). Built over a real AuthZService (I-8)
// with fake repositories, so the assertions pin P-35 to the projection AuthZ
// Enrichment builds x-tenant-roles / x-departments / x-plan /
// x-feature-flags from — not to a re-implementation of it.
package unit_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scTenantGetter is the TenantService slice SessionContextService reads.
type scTenantGetter struct {
	tenant *domain.Tenant
	err    error
	calls  int
}

func (g *scTenantGetter) Get(context.Context, uuid.UUID) (*domain.Tenant, error) {
	g.calls++
	return g.tenant, g.err
}

func scRow(status domain.MembershipStatus, sub domain.SubscriptionStatus) *port.MembershipProjectionRow {
	return &port.MembershipProjectionRow{
		MembershipStatus:   status,
		TenantPlan:         domain.PlanStarter,
		SubscriptionStatus: sub,
		// I-8's own per-user snapshot of these two — P-35 must not use them.
		Locale:              "fr",
		MFAFreshnessSeconds: 900,
		ResidencyMode:       domain.ResidencyMode("strict"),
		TenantFeatureFlags:  map[string]any{"beta_x": true, "off_flag": false},
		Roles:               []domain.TenantRoleCode{domain.RoleTenantAdmin},
	}
}

func scService(row *port.MembershipProjectionRow, tenants *scTenantGetter) *service.SessionContextService {
	repo := &fakeAuthZRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error) {
		return row, nil
	}}
	plans := &fakePlanReader{plan: &domain.Plan{Code: domain.PlanStarter, FeatureSet: map[string]any{"ai_drafting": true}}}
	authz := service.NewAuthZService(repo, plans, &fakeDeptReader{}, nil)
	return service.NewSessionContextService(authz, tenants)
}

func scTenant() *scTenantGetter {
	return &scTenantGetter{tenant: &domain.Tenant{MFAFreshnessSeconds: 300, DefaultLocale: "en", Slug: "acme"}}
}

// The values AuthZ Enrichment injects for the same caller: x-tenant-roles is
// I-8's roles joined (derived "member" first, TR-7), x-departments is
// "<department_id>:<role_level>", x-plan is plan, x-feature-flags is I-8's
// feature_flags. mfa_freshness_seconds / locale come from the tenant row
// (I-14 / I-9's source), not I-8's snapshot.
func TestSessionContext_MatchesI8Projection(t *testing.T) {
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	row := scRow(domain.MembershipActive, domain.StatusActive)
	row.Departments = []domain.DeptMembershipView{{DepartmentID: deptID, RoleLevel: domain.DeptApprover}}
	svc := scService(row, scTenant())

	sc, err := svc.Get(context.Background(), tenantID, userID)
	require.NoError(t, err)

	assert.Equal(t, tenantID, sc.TenantID)
	roles := make([]string, len(sc.Roles))
	for i, r := range sc.Roles {
		roles[i] = string(r)
	}
	assert.Equal(t, "member,tenant_admin", strings.Join(roles, ","), "x-tenant-roles")
	require.Len(t, sc.Departments, 1)
	assert.Equal(t, deptID.String()+":approver",
		sc.Departments[0].DepartmentID.String()+":"+string(sc.Departments[0].RoleLevel), "x-departments")
	assert.Equal(t, domain.PlanStarter, sc.Plan, "x-plan")
	assert.Equal(t, []string{"ai_drafting", "beta_x"}, sc.FeatureFlags, "x-feature-flags (sorted, enabled only)")
	assert.False(t, sc.ReadOnly)
	assert.Equal(t, 300, sc.MFAFreshnessSeconds, "from the tenant row (I-14), not I-8's snapshot")
	assert.Equal(t, "en", sc.Locale, "from the tenant row (I-9), not I-8's snapshot")
	assert.Equal(t, "acme", sc.Slug, "the caller's own workspace slug, from the tenant row")
}

// A cancelled tenant is read-only (§16 A53); AuthZ Enrichment appends
// "read_only" to x-feature-flags, and so does P-35.
func TestSessionContext_ReadOnlyAppendsFlag(t *testing.T) {
	svc := scService(scRow(domain.MembershipActive, domain.StatusCancelled), scTenant())
	sc, err := svc.Get(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.True(t, sc.ReadOnly)
	assert.Equal(t, []string{"ai_drafting", "beta_x", "read_only"}, sc.FeatureFlags)
}

// A tenant flag literally named read_only is not listed twice.
func TestSessionContext_ReadOnlyFlagNotDuplicated(t *testing.T) {
	row := scRow(domain.MembershipActive, domain.StatusCancelled)
	row.TenantFeatureFlags = map[string]any{"read_only": true}
	svc := scService(row, scTenant())
	sc, err := svc.Get(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Equal(t, []string{"ai_drafting", "read_only"}, sc.FeatureFlags)
}

// No roles / departments are [] (never nil), so the JSON is never null.
func TestSessionContext_EmptyCollectionsNotNil(t *testing.T) {
	row := scRow(domain.MembershipActive, domain.StatusTrial)
	row.Roles = nil
	row.TenantFeatureFlags = nil
	repo := &fakeAuthZRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error) {
		return row, nil
	}}
	authz := service.NewAuthZService(repo, &fakePlanReader{err: errors.New("catalog down")}, &fakeDeptReader{}, nil)
	sc, err := service.NewSessionContextService(authz, scTenant()).Get(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Equal(t, []domain.TenantRoleCode{domain.RoleMember}, sc.Roles, "derived member role only")
	assert.NotNil(t, sc.Departments)
	assert.Empty(t, sc.Departments)
	assert.NotNil(t, sc.FeatureFlags)
	assert.Empty(t, sc.FeatureFlags)
}

// No membership row: I-8's answer, member_not_found — and the tenant row is
// never read.
func TestSessionContext_NoMembership_MemberNotFound(t *testing.T) {
	tenants := scTenant()
	svc := scService(nil, tenants)
	_, err := svc.Get(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrMemberNotFound)
	assert.Zero(t, tenants.calls)
}

// A suspended member gets RequireActiveMembership's 403 (AUTH-1), not the
// roles I-8 still returns for them.
func TestSessionContext_SuspendedMember_Forbidden(t *testing.T) {
	tenants := scTenant()
	svc := scService(scRow(domain.MembershipSuspended, domain.StatusActive), tenants)
	_, err := svc.Get(context.Background(), uuid.New(), uuid.New())
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.ErrorIs(t, err, domain.ErrInsufficientRole)
	assert.Equal(t, "suspended members cannot access tenant APIs", de.Message)
	assert.Zero(t, tenants.calls)
}

func TestSessionContext_OtherInactiveStatus_Forbidden(t *testing.T) {
	svc := scService(scRow(domain.MembershipStatus("left"), domain.StatusActive), scTenant())
	_, err := svc.Get(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrInsufficientRole)
}

func TestSessionContext_I8ErrorPropagates(t *testing.T) {
	repo := &fakeAuthZRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error) {
		return nil, domain.NewError(domain.ErrDBUnavailable, "db down")
	}}
	authz := service.NewAuthZService(repo, &fakePlanReader{}, &fakeDeptReader{}, nil)
	_, err := service.NewSessionContextService(authz, scTenant()).Get(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrDBUnavailable)
}

func TestSessionContext_TenantReadErrorPropagates(t *testing.T) {
	tenants := &scTenantGetter{err: domain.NewError(domain.ErrTenantNotFound, "tenant not found")}
	svc := scService(scRow(domain.MembershipActive, domain.StatusActive), tenants)
	_, err := svc.Get(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrTenantNotFound)
}

// scProjector hands SessionContextService a projection directly, to reach
// the nil-collection guards a real AuthZService never trips.
type scProjector struct{ proj *service.MembershipProjection }

func (p scProjector) GetMembership(context.Context, uuid.UUID, uuid.UUID) (*service.MembershipProjection, error) {
	return p.proj, nil
}

func TestSessionContext_NilCollectionsFromProjection(t *testing.T) {
	proj := &service.MembershipProjection{Status: domain.MembershipActive}
	sc, err := service.NewSessionContextService(scProjector{proj}, scTenant()).Get(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.NotNil(t, sc.Roles)
	assert.NotNil(t, sc.Departments)
	assert.NotNil(t, sc.FeatureFlags)
}

// Changing the returned lists never changes the cached projection: roles,
// departments and flags are all copies.
func TestSessionContext_DoesNotAliasProjection(t *testing.T) {
	deptID := uuid.New()
	proj := &service.MembershipProjection{
		Status:       domain.MembershipActive,
		Roles:        []domain.TenantRoleCode{domain.RoleMember, domain.RoleTenantAdmin},
		Departments:  []domain.DeptMembershipView{{DepartmentID: deptID, RoleLevel: domain.DeptReviewer}},
		FeatureFlags: []string{"ai_drafting"},
	}
	svc := service.NewSessionContextService(scProjector{proj}, scTenant())

	sc, err := svc.Get(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	sc.Roles[0] = domain.RoleTenantOwner
	sc.Departments[0].RoleLevel = domain.DeptApprover
	sc.FeatureFlags[0] = "changed"

	assert.Equal(t, domain.RoleMember, proj.Roles[0])
	assert.Equal(t, domain.DeptReviewer, proj.Departments[0].RoleLevel)
	assert.Equal(t, "ai_drafting", proj.FeatureFlags[0])
}
