package service

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── fakes ────────────────────────────────────────────────────────────────────

type pccPlanCatalog struct{ plans []domain.Plan }

func (f *pccPlanCatalog) Plans(_ context.Context) ([]domain.Plan, error) { return f.plans, nil }
func (f *pccPlanCatalog) PlanByCode(_ context.Context, code domain.TenantPlan) (*domain.Plan, error) {
	for i := range f.plans {
		if f.plans[i].Code == code {
			return &f.plans[i], nil
		}
	}
	return nil, domain.NewError(domain.ErrPlanNotFound, "plan not found")
}

type pccIDPReader struct {
	count int
	err   error
}

func (f *pccIDPReader) GetIDPCount(_ context.Context, _ uuid.UUID) (int, error) {
	return f.count, f.err
}

// pccTenantRepo embeds TenantRepositoryNoop and overrides only FindByID.
type pccTenantRepo struct {
	port.TenantRepositoryNoop
	tenant *domain.Tenant
	err    error
}

func (r *pccTenantRepo) FindByID(_ context.Context, _ uuid.UUID) (*domain.Tenant, error) {
	return r.tenant, r.err
}

// standard three-tier catalog: Starter has no SSO, Pro/Enterprise have SSO.
func pccPlans() *pccPlanCatalog {
	return &pccPlanCatalog{plans: []domain.Plan{
		{Code: domain.PlanStarter, SSOEnabled: false},
		{Code: domain.PlanPro, SSOEnabled: true},
		{Code: domain.PlanEnterprise, SSOEnabled: true},
	}}
}

func pccKnownTenant() *pccTenantRepo {
	return &pccTenantRepo{tenant: &domain.Tenant{ID: uuid.New(), Plan: domain.PlanPro}}
}

func pccUnknownTenant() *pccTenantRepo {
	return &pccTenantRepo{err: domain.NewError(domain.ErrTenantNotFound, "tenant not found")}
}

func buildPCC(repo *pccTenantRepo, plans port.PlanCatalogReader, idp port.RealmIDPReader) *TenantService {
	return NewTenantService(repo, nil, nil).
		WithPlanCatalog(plans).
		WithIDPReader(idp)
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestPlanChangeCheck_ProToStarter_NoIdPs_Allowed(t *testing.T) {
	svc := buildPCC(pccKnownTenant(), pccPlans(), &pccIDPReader{count: 0})
	result, err := svc.PlanChangeCheck(context.Background(), uuid.New(), domain.PlanStarter)

	require.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.Empty(t, result.Blockers)
}

func TestPlanChangeCheck_ProToStarter_OneIdP_Blocked(t *testing.T) {
	svc := buildPCC(pccKnownTenant(), pccPlans(), &pccIDPReader{count: 1})
	result, err := svc.PlanChangeCheck(context.Background(), uuid.New(), domain.PlanStarter)

	require.NoError(t, err)
	assert.False(t, result.Allowed)
	require.Len(t, result.Blockers, 1)
	assert.Equal(t, BlockerCodeSSOIdpConfigured, result.Blockers[0].Code)
	assert.Contains(t, result.Blockers[0].Detail, "1 active IdP(s)")
	assert.Contains(t, result.Blockers[0].Detail, string(domain.PlanStarter))
}

func TestPlanChangeCheck_StarterToPro_Upgrade_Allowed_RPNotCalled(t *testing.T) {
	// idpReader errors if called — upgrade (SSO=true target) must never reach RP.
	svc := buildPCC(pccKnownTenant(), pccPlans(),
		&pccIDPReader{err: errors.New("RP must not be called for SSO-enabled target")})

	result, err := svc.PlanChangeCheck(context.Background(), uuid.New(), domain.PlanPro)

	require.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.Empty(t, result.Blockers)
}

func TestPlanChangeCheck_SamePlan_Pro_Allowed_RPNotCalled(t *testing.T) {
	svc := buildPCC(pccKnownTenant(), pccPlans(),
		&pccIDPReader{err: errors.New("RP must not be called for SSO-enabled target")})

	result, err := svc.PlanChangeCheck(context.Background(), uuid.New(), domain.PlanPro)

	require.NoError(t, err)
	assert.True(t, result.Allowed)
}

func TestPlanChangeCheck_RPReturns500_503(t *testing.T) {
	svc := buildPCC(pccKnownTenant(), pccPlans(),
		&pccIDPReader{err: errors.New("rp: GetIDPCount returned 500")})

	_, err := svc.PlanChangeCheck(context.Background(), uuid.New(), domain.PlanStarter)

	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrRealmProvisionerUnavailable.Error(), de.Code)
}

func TestPlanChangeCheck_RPTimeout_503(t *testing.T) {
	svc := buildPCC(pccKnownTenant(), pccPlans(),
		&pccIDPReader{err: context.DeadlineExceeded})

	_, err := svc.PlanChangeCheck(context.Background(), uuid.New(), domain.PlanStarter)

	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrRealmProvisionerUnavailable.Error(), de.Code)
}

func TestPlanChangeCheck_RP404_503(t *testing.T) {
	svc := buildPCC(pccKnownTenant(), pccPlans(),
		&pccIDPReader{err: errors.New("rp: GetIDPCount returned 404")})

	_, err := svc.PlanChangeCheck(context.Background(), uuid.New(), domain.PlanStarter)

	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrRealmProvisionerUnavailable.Error(), de.Code)
}

func TestPlanChangeCheck_MissingTargetPlan_400(t *testing.T) {
	svc := buildPCC(pccKnownTenant(), pccPlans(), &pccIDPReader{})
	_, err := svc.PlanChangeCheck(context.Background(), uuid.New(), "")

	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrInvalidTargetPlan.Error(), de.Code)
}

func TestPlanChangeCheck_UnknownTargetPlan_400(t *testing.T) {
	svc := buildPCC(pccKnownTenant(), pccPlans(), &pccIDPReader{})
	_, err := svc.PlanChangeCheck(context.Background(), uuid.New(), "premium")

	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrInvalidTargetPlan.Error(), de.Code)
}

func TestPlanChangeCheck_UnknownTenant_404(t *testing.T) {
	svc := buildPCC(pccUnknownTenant(), pccPlans(), &pccIDPReader{})
	_, err := svc.PlanChangeCheck(context.Background(), uuid.New(), domain.PlanStarter)

	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrTenantNotFound.Error(), de.Code)
}

func TestPlanChangeCheck_MultipleIdPs_DetailMentionsCount(t *testing.T) {
	svc := buildPCC(pccKnownTenant(), pccPlans(), &pccIDPReader{count: 3})
	result, err := svc.PlanChangeCheck(context.Background(), uuid.New(), domain.PlanStarter)

	require.NoError(t, err)
	require.False(t, result.Allowed)
	assert.Contains(t, result.Blockers[0].Detail, "3 active IdP(s)")
}

func TestPlanChangeCheck_EnterpriseToStarter_OneIdP_Blocked(t *testing.T) {
	svc := buildPCC(pccKnownTenant(), pccPlans(), &pccIDPReader{count: 1})
	result, err := svc.PlanChangeCheck(context.Background(), uuid.New(), domain.PlanStarter)

	require.NoError(t, err)
	assert.False(t, result.Allowed)
	assert.Equal(t, BlockerCodeSSOIdpConfigured, result.Blockers[0].Code)
}

func TestPlanChangeCheck_NilIDPReader_WhenTargetDropsSSO_503(t *testing.T) {
	// nil idpReader must fail closed, not silently allow.
	svc := buildPCC(pccKnownTenant(), pccPlans(), nil)
	_, err := svc.PlanChangeCheck(context.Background(), uuid.New(), domain.PlanStarter)

	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrRealmProvisionerUnavailable.Error(), de.Code)
}

func TestPlanChangeCheck_BlockerCodeConstant(t *testing.T) {
	// Wire-contract guard: the blocker code is a stable string clients parse.
	assert.Equal(t, "sso_idp_configured", BlockerCodeSSOIdpConfigured)
}

// ── CD-20 review fix: catalog miss must NOT assume SSO=true ──────────────

// pccEmptyCatalog returns a catalog reader that finds no plan (simulates
// catalog miss or ErrPlanNotFound).
func pccEmptyCatalog() *pccPlanCatalog { return &pccPlanCatalog{plans: nil} }

// pccSSOOnlyPlanCatalog returns a catalog where EVERY plan has sso_enabled=true,
// ensuring the fast-path (no RP call) fires for any target plan.
func pccSSOOnlyPlanCatalog() *pccPlanCatalog {
	return &pccPlanCatalog{plans: []domain.Plan{
		{Code: domain.PlanStarter, SSOEnabled: true},
		{Code: domain.PlanPro, SSOEnabled: true},
		{Code: domain.PlanEnterprise, SSOEnabled: true},
	}}
}

// TestCD20_CatalogMiss_IdPPresent_Blocked: when catalog lookup misses and
// the tenant has an active IdP, the downgrade must be BLOCKED. The old
// "assume SSO=true on miss" logic would have returned allowed — this test
// catches the regression.
func TestCD20_CatalogMiss_IdPPresent_Blocked(t *testing.T) {
	svc := buildPCC(pccKnownTenant(), pccEmptyCatalog(), &pccIDPReader{count: 1})

	result, err := svc.PlanChangeCheck(context.Background(), uuid.New(), domain.PlanStarter)

	require.NoError(t, err)
	assert.False(t, result.Allowed,
		"catalog miss + idp_count>0 must be BLOCKED (CD-20 review fix)")
	require.Len(t, result.Blockers, 1)
	assert.Equal(t, BlockerCodeSSOIdpConfigured, result.Blockers[0].Code)
}

// TestCD20_CatalogMiss_NoIdP_Allowed: catalog miss + 0 IdPs = allowed.
func TestCD20_CatalogMiss_NoIdP_Allowed(t *testing.T) {
	svc := buildPCC(pccKnownTenant(), pccEmptyCatalog(), &pccIDPReader{count: 0})

	result, err := svc.PlanChangeCheck(context.Background(), uuid.New(), domain.PlanStarter)

	require.NoError(t, err)
	assert.True(t, result.Allowed, "catalog miss + idp_count=0 must be ALLOWED")
}

// TestCD20_CatalogMiss_RPError_503: catalog miss + RP failure = 503.
func TestCD20_CatalogMiss_RPError_503(t *testing.T) {
	svc := buildPCC(pccKnownTenant(), pccEmptyCatalog(),
		&pccIDPReader{err: errors.New("rp: GetIDPCount returned 503")})

	_, err := svc.PlanChangeCheck(context.Background(), uuid.New(), domain.PlanStarter)

	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrRealmProvisionerUnavailable.Error(), de.Code,
		"catalog miss + RP error must be 503 realm_provisioner_unavailable")
}

// TestCD20_CatalogConfirmsSSO_NoRPCall: when catalog CONFIRMS sso_enabled=true,
// RP must NOT be called (the fast-path still works correctly).
func TestCD20_CatalogConfirmsSSO_NoRPCall(t *testing.T) {
	// RP errors if called — this ensures no RP call fires.
	svc := buildPCC(pccKnownTenant(), pccSSOOnlyPlanCatalog(),
		&pccIDPReader{err: errors.New("RP must not be called when catalog confirms SSO")})

	result, err := svc.PlanChangeCheck(context.Background(), uuid.New(), domain.PlanPro)

	require.NoError(t, err)
	assert.True(t, result.Allowed,
		"catalog-confirmed SSO=true must allow without RP call")
}

// TestCD20_NilCatalog_RPDecides: when no catalog reader is wired (nil),
// the service must still consult RP rather than assuming allowed.
func TestCD20_NilCatalog_RPDecides_IdPPresent_Blocked(t *testing.T) {
	svc := buildPCC(pccKnownTenant(), nil, &pccIDPReader{count: 2})

	result, err := svc.PlanChangeCheck(context.Background(), uuid.New(), domain.PlanStarter)

	require.NoError(t, err)
	assert.False(t, result.Allowed,
		"nil catalog + idp_count>0 must be BLOCKED (cannot assume SSO on nil reader)")
}
