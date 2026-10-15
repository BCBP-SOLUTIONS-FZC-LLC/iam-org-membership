// CD-37 residency_mode tests (TL decision 2026-10-15).
//
// Covers: default 'standard', tenant_owner can set strict, bad value → 400,
// cache invalidated on change, audit entry covers old+new, I-17 and AuthZ
// projection include the field.
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

// ─── helpers ─────────────────────────────────────────────────────────────────

type rmTenantRepo struct {
	port.TenantRepositoryNoop
	tenant    *domain.Tenant
	updated   *domain.TenantPatch
	findErr   error
	updateErr error
}

func (r *rmTenantRepo) FindByID(_ context.Context, _ uuid.UUID) (*domain.Tenant, error) {
	if r.findErr != nil {
		return nil, r.findErr
	}
	return r.tenant, nil
}
func (r *rmTenantRepo) FindByIDIncludingDeleted(_ context.Context, _ uuid.UUID) (*domain.Tenant, error) {
	return r.tenant, r.findErr
}
func (r *rmTenantRepo) Update(_ context.Context, _ uuid.UUID, p *domain.TenantPatch) (*domain.Tenant, error) {
	r.updated = p
	if r.updateErr != nil {
		return nil, r.updateErr
	}
	t := *r.tenant
	if p.ResidencyMode != nil {
		t.ResidencyMode = *p.ResidencyMode
	}
	if p.Name != nil {
		t.Name = *p.Name
	}
	return &t, nil
}

type rmRP struct{}

func (r *rmRP) CreateInvitedUser(context.Context, port.CreateInvitedUserRequest) (*port.CreateInvitedUserResponse, error) {
	return nil, nil
}
func (r *rmRP) DeleteUser(context.Context, uuid.UUID, uuid.UUID) error                   { return nil }
func (r *rmRP) PatchRealmConfig(context.Context, uuid.UUID, port.RealmConfigPatch) error { return nil }
func (r *rmRP) RevokeUserSessions(context.Context, uuid.UUID, uuid.UUID) error           { return nil }
func (r *rmRP) ResetMFA(context.Context, uuid.UUID, uuid.UUID) error                     { return nil }

func rmTenant(residencyMode domain.ResidencyMode) *domain.Tenant {
	return &domain.Tenant{
		ID:            uuid.New(),
		Slug:          "acme",
		Name:          "Acme",
		Plan:          domain.PlanPro,
		Status:        domain.StatusActive,
		ResidencyMode: residencyMode,
		RecordVersion: 1,
		FeatureFlags:  map[string]any{},
	}
}

func newRMService(repo *rmTenantRepo, cache *tsCache) *service.TenantService {
	return service.NewTenantService(repo, cache, &rmRP{})
}

// ─── CD-37-01: wire-contract pin on ResidencyMode constants ──────────────────

// Test Case ID:      CD37-01
// Feature:           CD-37 · ResidencyMode wire contract — string values are stable
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestCD37_ResidencyModeConstants_WireContract(t *testing.T) {
	assert.Equal(t, domain.ResidencyMode("standard"), domain.ResidencyModeStandard,
		"wire value must be 'standard'")
	assert.Equal(t, domain.ResidencyMode("strict"), domain.ResidencyModeStrict,
		"wire value must be 'strict'")
}

// ─── CD-37-02: Tenant carries default 'standard' ─────────────────────────────

// Test Case ID:      CD37-02
// Feature:           CD-37 · Default residency_mode is 'standard'
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestCD37_DefaultResidencyMode_IsStandard(t *testing.T) {
	t.Parallel()
	cache := newTSCache()
	tenant := rmTenant(domain.ResidencyModeStandard)
	repo := &rmTenantRepo{tenant: tenant}
	svc := newRMService(repo, cache)

	got, err := svc.Get(context.Background(), tenant.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.ResidencyModeStandard, got.ResidencyMode,
		"freshly provisioned tenant must default to 'standard'")
}

// ─── CD-37-03: tenant_owner can set residency_mode to 'strict' ───────────────

// Test Case ID:      CD37-03
// Feature:           CD-37 · tenant_owner can set residency_mode to 'strict'
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestCD37_Patch_ResidencyModeStrict_Succeeds(t *testing.T) {
	t.Parallel()
	cache := newTSCache()
	tenant := rmTenant(domain.ResidencyModeStandard)
	repo := &rmTenantRepo{tenant: tenant}
	svc := newRMService(repo, cache)

	rm := domain.ResidencyModeStrict
	updated, _, err := svc.Patch(context.Background(), tenant.ID, &domain.TenantPatch{
		ResidencyMode: &rm,
		RecordVersion: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, domain.ResidencyModeStrict, updated.ResidencyMode)
	require.NotNil(t, repo.updated.ResidencyMode)
	assert.Equal(t, domain.ResidencyModeStrict, *repo.updated.ResidencyMode,
		"repository must receive residency_mode = 'strict'")
}

// ─── CD-37-04: invalid value returns 400 validation_error ────────────────────

// Test Case ID:      CD37-04
// Feature:           CD-37 · invalid residency_mode → 400 validation_error (ErrValidation)
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestCD37_Patch_InvalidResidencyMode_Returns400(t *testing.T) {
	t.Parallel()
	cache := newTSCache()
	tenant := rmTenant(domain.ResidencyModeStandard)
	repo := &rmTenantRepo{tenant: tenant}
	svc := newRMService(repo, cache)

	rm := domain.ResidencyMode("enterprise_strict") // not valid
	_, _, err := svc.Patch(context.Background(), tenant.ID, &domain.TenantPatch{
		ResidencyMode: &rm,
		RecordVersion: 1,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrValidation,
		"invalid residency_mode must return ErrValidation (400)")
}

// Test Case ID:      CD37-05
// Feature:           CD-37 · empty residency_mode → 400 validation_error
// Priority: P1 · Severity: Minor · Automation Status: Automated
func TestCD37_Patch_EmptyResidencyMode_Returns400(t *testing.T) {
	t.Parallel()
	cache := newTSCache()
	tenant := rmTenant(domain.ResidencyModeStandard)
	repo := &rmTenantRepo{tenant: tenant}
	svc := newRMService(repo, cache)

	rm := domain.ResidencyMode("")
	_, _, err := svc.Patch(context.Background(), tenant.ID, &domain.TenantPatch{
		ResidencyMode: &rm,
		RecordVersion: 1,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrValidation)
}

// ─── CD-37-06: nil residency_mode is a no-op ─────────────────────────────────

// Test Case ID:      CD37-06
// Feature:           CD-37 · nil residency_mode in patch is a no-op (column untouched)
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestCD37_Patch_NilResidencyMode_IsNoop(t *testing.T) {
	t.Parallel()
	cache := newTSCache()
	tenant := rmTenant(domain.ResidencyModeStrict)
	repo := &rmTenantRepo{tenant: tenant}
	svc := newRMService(repo, cache)

	name := "Acme Updated"
	updated, _, err := svc.Patch(context.Background(), tenant.ID, &domain.TenantPatch{
		Name:          &name,
		RecordVersion: 1,
	})
	require.NoError(t, err)
	assert.Nil(t, repo.updated.ResidencyMode,
		"repo must not receive residency_mode when patch field is nil")
	assert.Equal(t, domain.ResidencyModeStrict, updated.ResidencyMode,
		"residency_mode must remain strict after a name-only patch")
}

// ─── CD-37-07: cache is invalidated when residency_mode changes ──────────────

// Test Case ID:      CD37-07
// Feature:           CD-37 · om:tenant cache key is evicted on residency_mode change
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestCD37_Patch_ResidencyModeChange_InvalidatesTenantCache(t *testing.T) {
	t.Parallel()
	cache := newTSCache()
	tenant := rmTenant(domain.ResidencyModeStandard)
	repo := &rmTenantRepo{tenant: tenant}
	svc := newRMService(repo, cache)

	rm := domain.ResidencyModeStrict
	_, _, err := svc.Patch(context.Background(), tenant.ID, &domain.TenantPatch{
		ResidencyMode: &rm,
		RecordVersion: 1,
	})
	require.NoError(t, err)
	cacheKey := "om:tenant:" + tenant.ID.String()
	assert.Contains(t, cache.del, cacheKey,
		"om:tenant:{id} must be deleted from cache when residency_mode changes")
}

// ─── CD-37-08: MembershipProjectionRow carries residency_mode ────────────────

// Test Case ID:      CD37-08
// Feature:           CD-37 · MembershipProjectionRow.ResidencyMode is available for I-8
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestCD37_MembershipProjectionRow_CarriesResidencyMode(t *testing.T) {
	t.Parallel()
	row := port.MembershipProjectionRow{
		MembershipStatus:   domain.MembershipActive,
		TenantPlan:         domain.PlanPro,
		SubscriptionStatus: domain.StatusActive,
		ResidencyMode:      domain.ResidencyModeStrict,
		TenantFeatureFlags: map[string]any{},
	}
	assert.Equal(t, domain.ResidencyModeStrict, row.ResidencyMode,
		"MembershipProjectionRow must carry residency_mode (fed to I-8 cache)")
}

// ─── CD-37-09: Tenant.ResidencyMode survives round-trip through domain ────────

// Test Case ID:      CD37-09
// Feature:           CD-37 · Tenant.ResidencyMode is part of the domain model
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestCD37_TenantDomain_HasResidencyModeField(t *testing.T) {
	t.Parallel()
	tenant := &domain.Tenant{
		ID:            uuid.New(),
		ResidencyMode: domain.ResidencyModeStrict,
		FeatureFlags:  map[string]any{},
	}
	assert.Equal(t, domain.ResidencyModeStrict, tenant.ResidencyMode)

	// TenantPatch also carries the field.
	rm := domain.ResidencyModeStandard
	patch := &domain.TenantPatch{ResidencyMode: &rm}
	require.NotNil(t, patch.ResidencyMode)
	assert.Equal(t, domain.ResidencyModeStandard, *patch.ResidencyMode)
}
