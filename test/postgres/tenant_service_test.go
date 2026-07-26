//go:build integration

// Phase 7 — Full-coverage service-layer sweep.
//
// Module:   iam-org-membership
// Feature:  Tenant (P-1 / P-2 · LLD §5.6, §6.1, T-10 MFA range, T-15
//           realm-sync backstop, CACHE-6/CACHE-9 invalidation)
// File:     internal/core/service/tenant_service.go (Get + Patch)
//
// Test-case metadata format per Reference_doc/Test_prompt.md.
// Test IDs stable and unique: P7-TENANT-NNN.
package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═════════════════════════════════════════════════════════════════════════
// P-1 Get — Positive
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P7-TENANT-001
// Module:            iam-org-membership · Tenant
// Feature:           P-1 · Get tenant
// API:               GET /api/v1/tenants/{id}
// Scenario:          Happy path — existing tenant returned via DB (cache=nil)
// Preconditions:     Tenant seeded, no cache configured
// Test Steps:
//   1. Seed a tenant
//   2. Call TenantService.Get with matching tenant_id
// Expected Result:
//   - Returns non-nil *Tenant with matching id + slug
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP7Tenant001_GetHappyPath(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "tenant-001")
	tctx := withSystemAndTenant(ctx, tenantID)

	got, err := fx.Tenant.Get(tctx, tenantID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, tenantID, got.ID)
	assert.Equal(t, "tenant-001", got.Slug)
}

// Test Case ID:      P7-TENANT-002
// Module:            iam-org-membership · Tenant
// Feature:           P-1 · Get tenant — not-found handling
// API:               GET /api/v1/tenants/{id}
// Scenario:          Negative — no tenant row for the id → repository error
// Preconditions:     Zero rows in tenants for the queried id
// Test Steps:
//   1. Call Get with a random uuid
// Expected Result:
//   - Returns error surfaced from FindByID (repo-level not-found)
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Tenant002_GetNotFound(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	unknownID := uuid.New()
	tctx := withSystemAndTenant(ctx, unknownID)

	_, err := fx.Tenant.Get(tctx, unknownID)
	require.Error(t, err, "P-1: unknown tenant id must error")
}

// ═════════════════════════════════════════════════════════════════════════
// P-2 Patch — Positive
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P7-TENANT-010
// Module:            iam-org-membership · Tenant
// Feature:           P-2 · Patch tenant name
// API:               PATCH /api/v1/tenants/{id}
// Scenario:          Happy path — name update
// Preconditions:     Tenant seeded
// Test Steps:
//   1. Seed tenant with name "Old Name"
//   2. Patch with new name "New Name"
// Expected Result:
//   - Updated tenant returned with new name
//   - deferredSync is false (no realm-sync needed)
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP7Tenant010_PatchNameHappy(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "tenant-010")
	tctx := withSystemAndTenant(ctx, tenantID)

	newName := "Renamed Acme"
	updated, deferredSync, err := fx.Tenant.Patch(tctx, tenantID, &domain.TenantPatch{
		Name: &newName, RecordVersion: 1,
	})
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, newName, updated.Name)
	assert.False(t, deferredSync, "name-only patch never triggers realm-sync")
	assert.Empty(t, fx.RP.PatchRealmConfigCalls, "no RP call for name-only patch")
}

// Test Case ID:      P7-TENANT-011
// Module:            iam-org-membership · Tenant
// Feature:           T-10 · MFA freshness lower bound (60 s)
// API:               PATCH /api/v1/tenants/{id}
// Scenario:          Boundary — exact minimum (60)
// Preconditions:     Tenant seeded
// Test Steps:
//   1. Patch mfa_freshness_seconds = 60
// Expected Result:
//   - Update succeeds, tenants.mfa_freshness_seconds = 60
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Tenant011_MFAFreshnessMinBoundary(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "tenant-011")
	tctx := withSystemAndTenant(ctx, tenantID)
	v := 60
	updated, _, err := fx.Tenant.Patch(tctx, tenantID, &domain.TenantPatch{MFAFreshnessSeconds: &v, RecordVersion: 1})
	require.NoError(t, err)
	assert.Equal(t, 60, updated.MFAFreshnessSeconds)
}

// Test Case ID:      P7-TENANT-012
// Module:            iam-org-membership · Tenant
// Feature:           T-10 · MFA freshness upper bound (900 s)
// API:               PATCH /api/v1/tenants/{id}
// Scenario:          Boundary — exact maximum (900)
// Preconditions:     Tenant seeded
// Test Steps:
//   1. Patch mfa_freshness_seconds = 900
// Expected Result:
//   - Update succeeds, tenants.mfa_freshness_seconds = 900
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Tenant012_MFAFreshnessMaxBoundary(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "tenant-012")
	tctx := withSystemAndTenant(ctx, tenantID)
	v := 900
	updated, _, err := fx.Tenant.Patch(tctx, tenantID, &domain.TenantPatch{MFAFreshnessSeconds: &v, RecordVersion: 1})
	require.NoError(t, err)
	assert.Equal(t, 900, updated.MFAFreshnessSeconds)
}

// Test Case ID:      P7-TENANT-013
// Module:            iam-org-membership · Tenant
// Feature:           P-2 · Locale update
// API:               PATCH /api/v1/tenants/{id}
// Scenario:          Happy path — default_locale flip
// Preconditions:     Tenant seeded with locale en-US
// Test Steps:
//   1. Patch default_locale = "en-IN"
// Expected Result:
//   - Update succeeds, locale flipped
// Priority:          P2
// Severity:          Major
// Automation Status: Automated
func TestP7Tenant013_LocaleUpdate(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "tenant-013")
	tctx := withSystemAndTenant(ctx, tenantID)
	locale := "en-IN"
	updated, _, err := fx.Tenant.Patch(tctx, tenantID, &domain.TenantPatch{DefaultLocale: &locale, RecordVersion: 1})
	require.NoError(t, err)
	assert.Equal(t, "en-IN", updated.DefaultLocale)
}

// Test Case ID:      P7-TENANT-014
// Module:            iam-org-membership · Tenant
// Feature:           T-15 · Realm-sync happy path
// API:               PATCH /api/v1/tenants/{id}
// Scenario:          Positive — local_accounts_enabled toggle → RP.PatchRealmConfig succeeds
// Preconditions:     Tenant seeded with LocalAccountsEnabled=true
// Test Steps:
//   1. Patch local_accounts_enabled = false
// Expected Result:
//   - Update succeeds, deferredSync = false
//   - RP.PatchRealmConfig called exactly once with LocalAccountsEnabled=&false
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Tenant014_RealmSyncHappy(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "tenant-014")
	tctx := withSystemAndTenant(ctx, tenantID)

	// Seed defaults local_accounts_enabled=true (per migration 000001).
	enabled := false
	_, deferredSync, err := fx.Tenant.Patch(tctx, tenantID, &domain.TenantPatch{LocalAccountsEnabled: &enabled, RecordVersion: 1})
	require.NoError(t, err)
	assert.False(t, deferredSync, "RP happy path → no deferred sync")
	require.Len(t, fx.RP.PatchRealmConfigCalls, 1, "T-15: exactly one RP call on flag change")
	require.NotNil(t, fx.RP.PatchRealmConfigCalls[0].LocalAccountsEnabled)
	assert.False(t, *fx.RP.PatchRealmConfigCalls[0].LocalAccountsEnabled)
}

// Test Case ID:      P7-TENANT-015
// Module:            iam-org-membership · Tenant
// Feature:           T-15 · Deferred sync on RP outage (LLD §16 A58 Option A)
// API:               PATCH /api/v1/tenants/{id}
// Scenario:          Positive with degraded dep — RP returns non-nil error
// Preconditions:     Tenant seeded; fx.RP.PatchRealmConfigFailNext = true
// Test Steps:
//   1. Set fx.RP.PatchRealmConfigFailNext = true
//   2. Patch local_accounts_enabled = false
// Expected Result:
//   - Local write commits (no error returned)
//   - deferredSync = true (handler translates to HTTP 202)
//   - RP was still called (attempt made)
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Tenant015_RealmSyncDeferredOnRPOutage(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "tenant-015")
	tctx := withSystemAndTenant(ctx, tenantID)

	fx.RP.PatchRealmConfigFailNext = true
	enabled := false
	_, deferredSync, err := fx.Tenant.Patch(tctx, tenantID, &domain.TenantPatch{LocalAccountsEnabled: &enabled, RecordVersion: 1})
	require.NoError(t, err, "T-15: local-first — RP failure must NOT fail the local update")
	assert.True(t, deferredSync,
		"T-15: RP failure must set deferredSync=true so the handler emits 202")
	assert.Len(t, fx.RP.PatchRealmConfigCalls, 1, "RP was attempted before the failure")
}

// Test Case ID:      P7-TENANT-016
// Module:            iam-org-membership · Tenant
// Feature:           T-15 · No RP call when local_accounts_enabled is unchanged
// API:               PATCH /api/v1/tenants/{id}
// Scenario:          Positive — patch sets the flag to its current value
// Preconditions:     Tenant seeded with LocalAccountsEnabled=true
// Test Steps:
//   1. Patch local_accounts_enabled = true (no change)
// Expected Result:
//   - No RP call, deferredSync=false
// Priority:          P2
// Severity:          Minor
// Automation Status: Automated
func TestP7Tenant016_RealmSyncSkippedWhenUnchanged(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "tenant-016")
	tctx := withSystemAndTenant(ctx, tenantID)

	enabled := true // matches default
	_, deferredSync, err := fx.Tenant.Patch(tctx, tenantID, &domain.TenantPatch{LocalAccountsEnabled: &enabled, RecordVersion: 1})
	require.NoError(t, err)
	assert.False(t, deferredSync)
	assert.Empty(t, fx.RP.PatchRealmConfigCalls,
		"T-15: no RP call when value is unchanged (avoid noise on the KC side)")
}

// ═════════════════════════════════════════════════════════════════════════
// P-2 Patch — Negative / Boundary (validation)
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P7-TENANT-020
// Module:            iam-org-membership · Tenant
// Feature:           API contract · patch payload required
// API:               PATCH /api/v1/tenants/{id}
// Scenario:          Negative — nil TenantPatch
// Preconditions:     Tenant seeded
// Test Steps:
//   1. Call Patch with patch = nil
// Expected Result:
//   - Returns ErrValidation ("patch is required")
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Tenant020_PatchNilRejected(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "tenant-020")
	tctx := withSystemAndTenant(ctx, tenantID)

	_, _, err := fx.Tenant.Patch(tctx, tenantID, nil)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "validation_error", de.Code)
}

// Test Case ID:      P7-TENANT-021
// Module:            iam-org-membership · Tenant
// Feature:           T-10 · MFA freshness below minimum
// API:               PATCH /api/v1/tenants/{id}
// Scenario:          Boundary — mfa_freshness=59 (one below the 60 min)
// Preconditions:     Tenant seeded
// Test Steps:
//   1. Patch mfa_freshness_seconds = 59
// Expected Result:
//   - Returns ErrValidation ("mfa_freshness_seconds must be between 60 and 900")
//   - Details.code = invalid_mfa_freshness_seconds
//   - No RP call
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Tenant021_MFAFreshnessBelowMin(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "tenant-021")
	tctx := withSystemAndTenant(ctx, tenantID)
	v := 59
	_, _, err := fx.Tenant.Patch(tctx, tenantID, &domain.TenantPatch{MFAFreshnessSeconds: &v, RecordVersion: 1})
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "validation_error", de.Code)
	assert.Equal(t, "invalid_mfa_freshness_seconds", de.Details["code"])
	assert.Empty(t, fx.RP.PatchRealmConfigCalls)
}

// Test Case ID:      P7-TENANT-022
// Module:            iam-org-membership · Tenant
// Feature:           T-10 · MFA freshness above maximum
// API:               PATCH /api/v1/tenants/{id}
// Scenario:          Boundary — mfa_freshness=901 (one above the 900 max)
// Preconditions:     Tenant seeded
// Test Steps:
//   1. Patch mfa_freshness_seconds = 901
// Expected Result:
//   - Returns ErrValidation with details.code=invalid_mfa_freshness_seconds
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Tenant022_MFAFreshnessAboveMax(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "tenant-022")
	tctx := withSystemAndTenant(ctx, tenantID)
	v := 901
	_, _, err := fx.Tenant.Patch(tctx, tenantID, &domain.TenantPatch{MFAFreshnessSeconds: &v, RecordVersion: 1})
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "invalid_mfa_freshness_seconds", de.Details["code"])
}

// Test Case ID:      P7-TENANT-023
// Module:            iam-org-membership · Tenant
// Feature:           T-10 · MFA freshness zero rejected
// API:               PATCH /api/v1/tenants/{id}
// Scenario:          Boundary — mfa_freshness=0
// Preconditions:     Tenant seeded
// Test Steps:
//   1. Patch mfa_freshness_seconds = 0
// Expected Result:
//   - Rejected (0 < 60 lower bound)
// Priority:          P2
// Severity:          Major
// Automation Status: Automated
func TestP7Tenant023_MFAFreshnessZero(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "tenant-023")
	tctx := withSystemAndTenant(ctx, tenantID)
	v := 0
	_, _, err := fx.Tenant.Patch(tctx, tenantID, &domain.TenantPatch{MFAFreshnessSeconds: &v, RecordVersion: 1})
	require.Error(t, err)
}

// Test Case ID:      P7-TENANT-024
// Module:            iam-org-membership · Tenant
// Feature:           T-10 · MFA freshness negative
// API:               PATCH /api/v1/tenants/{id}
// Scenario:          Boundary — mfa_freshness = -1
// Preconditions:     Tenant seeded
// Test Steps:
//   1. Patch mfa_freshness_seconds = -1
// Expected Result:
//   - Rejected
// Priority:          P2
// Severity:          Minor
// Automation Status: Automated
func TestP7Tenant024_MFAFreshnessNegative(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "tenant-024")
	tctx := withSystemAndTenant(ctx, tenantID)
	v := -1
	_, _, err := fx.Tenant.Patch(tctx, tenantID, &domain.TenantPatch{MFAFreshnessSeconds: &v, RecordVersion: 1})
	require.Error(t, err)
}

// Test Case ID:      P7-TENANT-025
// Module:            iam-org-membership · Tenant
// Feature:           Locale sanity — empty string rejected
// API:               PATCH /api/v1/tenants/{id}
// Scenario:          Negative — default_locale = ""
// Preconditions:     Tenant seeded
// Test Steps:
//   1. Patch default_locale = ""
// Expected Result:
//   - Returns ErrValidation with details.code=invalid_locale
// Priority:          P2
// Severity:          Minor
// Automation Status: Automated
func TestP7Tenant025_EmptyLocaleRejected(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "tenant-025")
	tctx := withSystemAndTenant(ctx, tenantID)
	loc := ""
	_, _, err := fx.Tenant.Patch(tctx, tenantID, &domain.TenantPatch{DefaultLocale: &loc, RecordVersion: 1})
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "invalid_locale", de.Details["code"])
}

// Test Case ID:      P7-TENANT-026
// Module:            iam-org-membership · Tenant
// Feature:           Not-found propagation on local_accounts_enabled patch
// API:               PATCH /api/v1/tenants/{id}
// Scenario:          Negative — flip local_accounts_enabled but the tenant doesn't exist
// Preconditions:     No tenant row for the id
// Test Steps:
//   1. Call Patch on a random uuid with LocalAccountsEnabled=&false
// Expected Result:
//   - FindByID (needed for change-detection) returns error → propagated
//   - RP was NOT called (early return)
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Tenant026_PatchNotFoundOnFlagChange(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	unknown := uuid.New()
	tctx := withSystemAndTenant(ctx, unknown)
	enabled := false
	_, _, err := fx.Tenant.Patch(tctx, unknown, &domain.TenantPatch{LocalAccountsEnabled: &enabled, RecordVersion: 1})
	require.Error(t, err, "Patch on unknown id must propagate FindByID error")
	assert.Empty(t, fx.RP.PatchRealmConfigCalls, "no RP call when tenant lookup fails")
}
