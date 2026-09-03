// Unit tests for internal/core/service/authz_service.go (I-8, §8.3).
// Previously untestable without a real Postgres pool — AuthZService held
// *pgcommon.Pool directly and ran SQL itself. Now that it depends only on
// port.AuthZRepository, these compose over a fake with no DB at all.
package unit_test

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeAuthZRepo struct {
	findFn func(ctx context.Context, tenantID, userID uuid.UUID) (*port.MembershipProjectionRow, error)
}

func (f *fakeAuthZRepo) FindMembershipProjection(ctx context.Context, tenantID, userID uuid.UUID) (*port.MembershipProjectionRow, error) {
	return f.findFn(ctx, tenantID, userID)
}

var _ port.AuthZRepository = (*fakeAuthZRepo)(nil)

type fakePlanReader struct {
	plan *domain.Plan
	err  error
}

func (f *fakePlanReader) Plans(context.Context) ([]domain.Plan, error) { return nil, nil }
func (f *fakePlanReader) PlanByCode(context.Context, domain.TenantPlan) (*domain.Plan, error) {
	return f.plan, f.err
}

var _ port.PlanCatalogReader = (*fakePlanReader)(nil)

type fakeDeptReader struct {
	depts []domain.Department
}

func (f *fakeDeptReader) Departments(context.Context) ([]domain.Department, error) {
	return f.depts, nil
}
func (f *fakeDeptReader) DepartmentByID(context.Context, uuid.UUID) (*domain.Department, error) {
	return nil, nil
}

var _ port.DepartmentCatalogReader = (*fakeDeptReader)(nil)

func TestAuthZ_GetMembership_NotFound(t *testing.T) {
	repo := &fakeAuthZRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error) {
		return nil, nil
	}}
	svc := service.NewAuthZService(repo, &fakePlanReader{}, &fakeDeptReader{}, nil)

	_, err := svc.GetMembership(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrMemberNotFound, de.Cause)
}

func TestAuthZ_GetMembership_RepoErrorPropagates(t *testing.T) {
	repoErr := domain.NewError(domain.ErrDependencyUnavailable, "db down")
	repo := &fakeAuthZRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error) {
		return nil, repoErr
	}}
	svc := service.NewAuthZService(repo, &fakePlanReader{}, &fakeDeptReader{}, nil)

	_, err := svc.GetMembership(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrDependencyUnavailable)
}

func TestAuthZ_GetMembership_InjectsDerivedMemberRoleFirst(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	repo := &fakeAuthZRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error) {
		return &port.MembershipProjectionRow{
			MembershipStatus:   domain.MembershipActive,
			TenantPlan:         domain.PlanStarter,
			SubscriptionStatus: domain.StatusActive,
			Roles:              []domain.TenantRoleCode{domain.RoleTenantAdmin},
		}, nil
	}}
	svc := service.NewAuthZService(repo, &fakePlanReader{}, &fakeDeptReader{}, nil)

	proj, err := svc.GetMembership(context.Background(), tenantID, userID)
	require.NoError(t, err)
	require.Len(t, proj.Roles, 2)
	assert.Equal(t, domain.RoleMember, proj.Roles[0], "TR-7: derived 'member' must be injected first")
	assert.Equal(t, domain.RoleTenantAdmin, proj.Roles[1])
}

func TestAuthZ_GetMembership_ReadOnlyDerivedFromCancelledStatus(t *testing.T) {
	repo := &fakeAuthZRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error) {
		return &port.MembershipProjectionRow{
			MembershipStatus:   domain.MembershipActive,
			TenantPlan:         domain.PlanStarter,
			SubscriptionStatus: domain.StatusCancelled,
		}, nil
	}}
	svc := service.NewAuthZService(repo, &fakePlanReader{}, &fakeDeptReader{}, nil)

	proj, err := svc.GetMembership(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.True(t, proj.ReadOnly, "§16 A53: cancelled subscription_status must project read_only=true")
	assert.Equal(t, domain.StatusCancelled, proj.TenantStatus, "deprecated alias mirrors subscription_status")
}

func TestAuthZ_GetMembership_EffectiveFeatureFlagsMergePlanAndTenantOverride(t *testing.T) {
	repo := &fakeAuthZRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error) {
		return &port.MembershipProjectionRow{
			MembershipStatus:   domain.MembershipActive,
			TenantPlan:         domain.PlanEnterprise,
			SubscriptionStatus: domain.StatusActive,
			TenantFeatureFlags: map[string]any{"custom_branding": true},
		}, nil
	}}
	plans := &fakePlanReader{plan: &domain.Plan{
		Code:       domain.PlanEnterprise,
		FeatureSet: map[string]any{"sso_enabled": true, "extra_seats": false},
	}}
	svc := service.NewAuthZService(repo, plans, &fakeDeptReader{}, nil)

	proj, err := svc.GetMembership(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"sso_enabled", "custom_branding"}, proj.FeatureFlags,
		"PLAN-6: plan baseline ⊕ tenant override, false-valued flags excluded")
}

func TestAuthZ_GetMembership_PlanLookupFailureDegradesGracefully(t *testing.T) {
	repo := &fakeAuthZRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error) {
		return &port.MembershipProjectionRow{
			MembershipStatus:   domain.MembershipActive,
			TenantPlan:         domain.PlanStarter,
			SubscriptionStatus: domain.StatusActive,
			TenantFeatureFlags: map[string]any{"beta_ui": true},
		}, nil
	}}
	plans := &fakePlanReader{err: domain.NewError(domain.ErrCatalogUnavailable, "catalog down")}
	svc := service.NewAuthZService(repo, plans, &fakeDeptReader{}, nil)

	proj, err := svc.GetMembership(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err, "a catalog-admin-config outage must not fail I-8")
	assert.Equal(t, []string{"beta_ui"}, proj.FeatureFlags, "no plan baseline, tenant override still applies")
}

func TestAuthZ_GetMembership_DepartmentCodeEnrichment(t *testing.T) {
	deptID := uuid.New()
	repo := &fakeAuthZRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error) {
		return &port.MembershipProjectionRow{
			MembershipStatus:   domain.MembershipActive,
			TenantPlan:         domain.PlanStarter,
			SubscriptionStatus: domain.StatusActive,
			Departments: []domain.DeptMembershipView{
				{DepartmentID: deptID, RoleLevel: domain.DeptReviewer},
			},
		}, nil
	}}
	depts := &fakeDeptReader{depts: []domain.Department{{ID: deptID, Code: "ENGINEERING"}}}
	svc := service.NewAuthZService(repo, &fakePlanReader{}, depts, nil)

	proj, err := svc.GetMembership(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	require.Len(t, proj.Departments, 1)
	assert.Equal(t, "ENGINEERING", proj.Departments[0].Code)
}
