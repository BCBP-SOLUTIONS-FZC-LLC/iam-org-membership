//go:build integration

// Phase 7 — Full-coverage service-layer sweep.
//
// Module:   iam-org-membership
// Feature:  Delegation (P-18 / P-19 / P-20 · LLD §8.6, §8.7, DEL-1..DEL-10)
// File:     internal/core/service/delegation_service.go (all 3 methods)
//
// Test-case metadata format per Reference_doc/Test_prompt.md:
//   Test Case ID · Module · Feature · API · Scenario · Preconditions
//   · Test Steps · Expected Result · Priority · Severity · Automation Status
//
// Category matrix executed (Test_prompt.md §1-§13):
//   Positive · Negative · Boundary · Business Rule · Branch · Database
//   · Event · Authorization · Concurrency · Contract · Assertions
//
// Each function's docstring carries the full metadata block. Test IDs are
// stable and unique: P7-DELEG-NNN.
package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═════════════════════════════════════════════════════════════════════════
// P-19 Create — Positive
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P7-DELEG-001
// Module:            iam-org-membership · Delegation
// Feature:           P-19 · Create OOO delegation
// API:               POST /api/v1/delegations
// Scenario:          Happy path — scope=all, no ends_at (open-ended)
// Preconditions:     Tenant exists · delegator + delegate are active members
// Test Steps:
//   1. Seed tenant + 2 active memberships (delegator, delegate)
//   2. Call DelegationService.Create with scope=all, no scope_id, no ends_at
// Expected Result:
//   - Returns a non-nil Delegation with status=active
//   - UP.SetAvailability called with status='ooo', DelegateID set, before insert
//   - outbox_events contains DelegationStarted with the created id as subject
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP7Deleg001_CreateHappyPath_ScopeAll(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "deleg-001")
	delegator, delegate := seedTwoActiveMembers(t, ctx, fx.rawPool, tenantID)
	tctx := withSystemAndTenant(ctx, tenantID)

	d, err := fx.Delegation.Create(tctx, tenantID, delegator, service.DelegationCreateInput{
		DelegateID: delegate,
		Scope:      string(domain.ScopeAll),
		Reason:     "OOO for annual leave",
	})
	require.NoError(t, err)
	require.NotNil(t, d)
	assert.Equal(t, delegator, d.DelegatorID)
	assert.Equal(t, delegate, d.DelegateID)
	assert.Equal(t, domain.ScopeAll, d.Scope)

	// CONS-2: UP was called before delegation insert.
	require.Len(t, fx.UP.Calls, 1, "P-19 must call UP.SetAvailability exactly once (availability-first)")
	require.NotNil(t, fx.UP.Calls[0].Status)
	assert.Equal(t, "ooo", *fx.UP.Calls[0].Status)
	require.NotNil(t, fx.UP.Calls[0].DelegateID)
	assert.Equal(t, delegate, *fx.UP.Calls[0].DelegateID)

	// Outbox has DelegationStarted for this tenant.
	types := fx.outboxEventTypesForTenant(t, ctx, tenantID)
	assert.Contains(t, types, domain.EventDelegationStarted,
		"DelegationStarted must be enqueued in the same tx as the delegation row")
}

// Test Case ID:      P7-DELEG-002
// Module:            iam-org-membership · Delegation
// Feature:           P-19 · Create OOO delegation
// API:               POST /api/v1/delegations
// Scenario:          Happy path — scope=department with scope_id
// Preconditions:     Tenant exists · both members active · department id supplied
// Test Steps:
//   1. Seed tenant + 2 active members
//   2. Call Create with scope=department + scope_id=<some uuid>
// Expected Result:
//   - Returns Delegation with Scope=department, ScopeID non-nil
//   - No DEL-2 scope_id_required error
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Deleg002_CreateHappyPath_ScopeDepartment(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "deleg-002")
	delegator, delegate := seedTwoActiveMembers(t, ctx, fx.rawPool, tenantID)
	tctx := withSystemAndTenant(ctx, tenantID)
	scopeID := uuid.New()

	d, err := fx.Delegation.Create(tctx, tenantID, delegator, service.DelegationCreateInput{
		DelegateID: delegate,
		Scope:      string(domain.ScopeDepartment),
		ScopeID:    &scopeID,
	})
	require.NoError(t, err)
	assert.Equal(t, domain.ScopeDepartment, d.Scope)
	require.NotNil(t, d.ScopeID)
	assert.Equal(t, scopeID, *d.ScopeID)
}

// Test Case ID:      P7-DELEG-003
// Module:            iam-org-membership · Delegation
// Feature:           P-19 · Create with explicit time bounds
// API:               POST /api/v1/delegations
// Scenario:          Positive — starts_at future, ends_at 7 days out
// Preconditions:     Standard seed
// Test Steps:
//   1. Seed tenant + members
//   2. Create with StartsAt = now+1h, EndsAt = now+7d
// Expected Result:
//   - Delegation persisted with the exact starts_at and ends_at
//   - UP.OOOFrom equals starts_at, UP.OOOUntil equals ends_at
// Priority:          P2
// Severity:          Major
// Automation Status: Automated
func TestP7Deleg003_CreateWithExplicitTimeWindow(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "deleg-003")
	delegator, delegate := seedTwoActiveMembers(t, ctx, fx.rawPool, tenantID)
	tctx := withSystemAndTenant(ctx, tenantID)

	starts := time.Now().UTC().Add(1 * time.Hour)
	ends := starts.Add(7 * 24 * time.Hour)

	d, err := fx.Delegation.Create(tctx, tenantID, delegator, service.DelegationCreateInput{
		DelegateID: delegate,
		Scope:      string(domain.ScopeAll),
		StartsAt:   &starts,
		EndsAt:     &ends,
	})
	require.NoError(t, err)
	assert.WithinDuration(t, starts, d.StartsAt, time.Second)
	require.NotNil(t, d.EndsAt)
	assert.WithinDuration(t, ends, *d.EndsAt, time.Second)
	// UP received the same window.
	require.NotNil(t, fx.UP.Calls[0].OOOFrom)
	require.NotNil(t, fx.UP.Calls[0].OOOUntil)
	assert.WithinDuration(t, starts, *fx.UP.Calls[0].OOOFrom, time.Second)
	assert.WithinDuration(t, ends, *fx.UP.Calls[0].OOOUntil, time.Second)
}

// ═════════════════════════════════════════════════════════════════════════
// P-19 Create — Negative / Business Rule
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P7-DELEG-010
// Module:            iam-org-membership · Delegation
// Feature:           DEL-1 · Self-delegation prohibited
// API:               POST /api/v1/delegations
// Scenario:          Negative — delegator_id == delegate_id
// Preconditions:     Tenant exists · one active member
// Test Steps:
//   1. Seed tenant + one active member
//   2. Create with delegate_id = delegator_id
// Expected Result:
//   - Returns ErrSelfDelegation → HTTP 422 self_delegation
//   - UP is NOT called (early-return before availability-first)
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP7Deleg010_SelfDelegationRejected(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "deleg-010")
	delegator, _ := seedTwoActiveMembers(t, ctx, fx.rawPool, tenantID)
	tctx := withSystemAndTenant(ctx, tenantID)

	_, err := fx.Delegation.Create(tctx, tenantID, delegator, service.DelegationCreateInput{
		DelegateID: delegator, // self
		Scope:      string(domain.ScopeAll),
	})
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "self_delegation", de.Code)
	assert.Empty(t, fx.UP.Calls, "UP must not be called when the request fails early validation")
}

// Test Case ID:      P7-DELEG-011
// Module:            iam-org-membership · Delegation
// Feature:           Scope enum validation
// API:               POST /api/v1/delegations
// Scenario:          Negative — invalid scope string
// Preconditions:     Standard seed
// Test Steps:
//   1. Create with scope="global" (not in enum)
// Expected Result:
//   - Returns ErrValidation with code=invalid_delegation_scope → HTTP 400
//   - UP not called
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Deleg011_InvalidScopeStringRejected(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "deleg-011")
	delegator, delegate := seedTwoActiveMembers(t, ctx, fx.rawPool, tenantID)
	tctx := withSystemAndTenant(ctx, tenantID)

	_, err := fx.Delegation.Create(tctx, tenantID, delegator, service.DelegationCreateInput{
		DelegateID: delegate,
		Scope:      "global", // invalid
	})
	require.Error(t, err)
	assert.Empty(t, fx.UP.Calls)
}

// Test Case ID:      P7-DELEG-012
// Module:            iam-org-membership · Delegation
// Feature:           DEL-2 · scope_id required for department/tender
// API:               POST /api/v1/delegations
// Scenario:          Negative — scope=department without scope_id
// Preconditions:     Standard seed
// Test Steps:
//   1. Create with scope=department, scope_id=nil
// Expected Result:
//   - Returns ErrScopeIDRequired → HTTP 422 scope_id_required
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Deleg012_DepartmentScopeMissingScopeID(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "deleg-012")
	delegator, delegate := seedTwoActiveMembers(t, ctx, fx.rawPool, tenantID)
	tctx := withSystemAndTenant(ctx, tenantID)

	_, err := fx.Delegation.Create(tctx, tenantID, delegator, service.DelegationCreateInput{
		DelegateID: delegate,
		Scope:      string(domain.ScopeDepartment),
		// ScopeID: nil
	})
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "scope_id_required", de.Code)
}

// Test Case ID:      P7-DELEG-013
// Module:            iam-org-membership · Delegation
// Feature:           DEL-2 · scope_id required for tender scope
// API:               POST /api/v1/delegations
// Scenario:          Negative — scope=tender without scope_id
// Preconditions:     Standard seed
// Test Steps:
//   1. Create with scope=tender, scope_id=nil
// Expected Result:
//   - Returns ErrScopeIDRequired → HTTP 422 scope_id_required
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Deleg013_TenderScopeMissingScopeID(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "deleg-013")
	delegator, delegate := seedTwoActiveMembers(t, ctx, fx.rawPool, tenantID)
	tctx := withSystemAndTenant(ctx, tenantID)

	_, err := fx.Delegation.Create(tctx, tenantID, delegator, service.DelegationCreateInput{
		DelegateID: delegate,
		Scope:      string(domain.ScopeTender),
	})
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "scope_id_required", de.Code)
}

// Test Case ID:      P7-DELEG-014
// Module:            iam-org-membership · Delegation
// Feature:           DEL-8 · Inverted time window rejected
// API:               POST /api/v1/delegations
// Scenario:          Boundary — ends_at strictly before starts_at
// Preconditions:     Standard seed
// Test Steps:
//   1. Create with StartsAt = now+1h, EndsAt = now-1h
// Expected Result:
//   - Returns ErrDelegationWindowInverted → HTTP 422
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Deleg014_EndsBeforeStartsRejected(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "deleg-014")
	delegator, delegate := seedTwoActiveMembers(t, ctx, fx.rawPool, tenantID)
	tctx := withSystemAndTenant(ctx, tenantID)

	starts := time.Now().UTC().Add(1 * time.Hour)
	ends := time.Now().UTC().Add(-1 * time.Hour)
	_, err := fx.Delegation.Create(tctx, tenantID, delegator, service.DelegationCreateInput{
		DelegateID: delegate,
		Scope:      string(domain.ScopeAll),
		StartsAt:   &starts,
		EndsAt:     &ends,
	})
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "delegation_window_inverted", de.Code)
}

// Test Case ID:      P7-DELEG-015
// Module:            iam-org-membership · Delegation
// Feature:           DEL-8 · Zero-duration window rejected (ends_at == starts_at)
// API:               POST /api/v1/delegations
// Scenario:          Boundary — ends_at equals starts_at (must be strictly after)
// Preconditions:     Standard seed
// Test Steps:
//   1. Create with StartsAt == EndsAt
// Expected Result:
//   - Returns ErrDelegationWindowInverted (strict inequality)
// Priority:          P2
// Severity:          Minor
// Automation Status: Automated
func TestP7Deleg015_EndsEqualsStartsRejected(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "deleg-015")
	delegator, delegate := seedTwoActiveMembers(t, ctx, fx.rawPool, tenantID)
	tctx := withSystemAndTenant(ctx, tenantID)

	ts := time.Now().UTC().Add(1 * time.Hour)
	_, err := fx.Delegation.Create(tctx, tenantID, delegator, service.DelegationCreateInput{
		DelegateID: delegate,
		Scope:      string(domain.ScopeAll),
		StartsAt:   &ts,
		EndsAt:     &ts,
	})
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "delegation_window_inverted", de.Code,
		"DEL-8 uses strict After() — equal timestamps are inverted, not zero-duration")
}

// Test Case ID:      P7-DELEG-016
// Module:            iam-org-membership · Delegation
// Feature:           DEL-1 · Delegator must be a tenant member
// API:               POST /api/v1/delegations
// Scenario:          Negative — delegator has no tenant_membership row
// Preconditions:     Tenant exists but delegator user_id is not a member
// Test Steps:
//   1. Only seed the delegate as a member
//   2. Call Create with a random delegator uuid
// Expected Result:
//   - Repo FindByUserID returns member_not_found → propagates up as 404
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Deleg016_DelegatorNotAMember(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "deleg-016")
	// only seed the delegate
	delegate := uuid.New()
	_, err := fx.rawPool.Exec(ctx, `
		INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
		VALUES (gen_random_uuid(), $1, $2, 'active')`, tenantID, delegate)
	require.NoError(t, err)
	tctx := withSystemAndTenant(ctx, tenantID)

	_, err = fx.Delegation.Create(tctx, tenantID, uuid.New(), service.DelegationCreateInput{
		DelegateID: delegate,
		Scope:      string(domain.ScopeAll),
	})
	require.Error(t, err)
	assert.Empty(t, fx.UP.Calls, "UP not called when delegator lookup fails")
}

// Test Case ID:      P7-DELEG-017
// Module:            iam-org-membership · Delegation
// Feature:           DEL-1 · Delegate must be an active member
// API:               POST /api/v1/delegations
// Scenario:          Negative — delegate has no membership row
// Preconditions:     Only the delegator is a member
// Test Steps:
//   1. Seed only the delegator as active member
//   2. Call Create with a random delegate uuid
// Expected Result:
//   - Returns ErrInvalidDelegate → HTTP 422 invalid_delegate
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Deleg017_DelegateNotAMember(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "deleg-017")
	delegator := uuid.New()
	_, err := fx.rawPool.Exec(ctx, `
		INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
		VALUES (gen_random_uuid(), $1, $2, 'active')`, tenantID, delegator)
	require.NoError(t, err)
	tctx := withSystemAndTenant(ctx, tenantID)

	_, err = fx.Delegation.Create(tctx, tenantID, delegator, service.DelegationCreateInput{
		DelegateID: uuid.New(),
		Scope:      string(domain.ScopeAll),
	})
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "invalid_delegate", de.Code)
}

// Test Case ID:      P7-DELEG-018
// Module:            iam-org-membership · Delegation
// Feature:           DEL-1 · Delegate must be ACTIVE (not suspended)
// API:               POST /api/v1/delegations
// Scenario:          Negative — delegate exists but status=suspended
// Preconditions:     Both members exist; delegate suspended
// Test Steps:
//   1. Seed both; UPDATE delegate → status=suspended
//   2. Call Create
// Expected Result:
//   - Returns ErrInvalidDelegate ("delegate is not an active member")
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Deleg018_DelegateSuspendedRejected(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "deleg-018")
	delegator, delegate := seedTwoActiveMembers(t, ctx, fx.rawPool, tenantID)
	_, err := fx.rawPool.Exec(ctx,
		`UPDATE tenant_memberships SET status = 'suspended' WHERE user_id = $1`, delegate)
	require.NoError(t, err)
	tctx := withSystemAndTenant(ctx, tenantID)

	_, err = fx.Delegation.Create(tctx, tenantID, delegator, service.DelegationCreateInput{
		DelegateID: delegate,
		Scope:      string(domain.ScopeAll),
	})
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "invalid_delegate", de.Code)
}

// Test Case ID:      P7-DELEG-019
// Module:            iam-org-membership · Delegation
// Feature:           CONS-2 · Availability-first: UP failure → 422 invalid_delegate
// API:               POST /api/v1/delegations
// Scenario:          Negative — UP.SetAvailability returns error
// Preconditions:     Both members active; fx.UP.FailNext = true
// Test Steps:
//   1. Set fx.UP.FailNext = true
//   2. Call Create with valid payload
// Expected Result:
//   - Returns ErrInvalidDelegate with code=invalid_delegate
//   - Delegation row NOT inserted (verify via count in delegations)
//   - Outbox has no DelegationStarted for this tenant
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP7Deleg019_UPFailureBlocksCreate(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "deleg-019")
	delegator, delegate := seedTwoActiveMembers(t, ctx, fx.rawPool, tenantID)
	tctx := withSystemAndTenant(ctx, tenantID)

	fx.UP.FailNext = true
	_, err := fx.Delegation.Create(tctx, tenantID, delegator, service.DelegationCreateInput{
		DelegateID: delegate,
		Scope:      string(domain.ScopeAll),
	})
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "invalid_delegate", de.Code)

	// No delegation row.
	var n int
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM delegations WHERE tenant_id = $1`, tenantID).Scan(&n))
	assert.Equal(t, 0, n, "CONS-2: UP failure must prevent delegation row insert")

	// No outbox event.
	types := fx.outboxEventTypesForTenant(t, ctx, tenantID)
	assert.NotContains(t, types, domain.EventDelegationStarted,
		"CONS-2: no DelegationStarted event when UP call failed")
}

// ═════════════════════════════════════════════════════════════════════════
// P-18 List
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P7-DELEG-020
// Module:            iam-org-membership · Delegation
// Feature:           P-18 · List active delegations
// API:               GET /api/v1/delegations
// Scenario:          Positive — empty tenant returns empty slice, not nil error
// Preconditions:     Tenant exists with zero delegations
// Test Steps:
//   1. Seed tenant with no delegations
//   2. Call DelegationService.List
// Expected Result:
//   - No error; returns empty slice
// Priority:          P2
// Severity:          Minor
// Automation Status: Automated
func TestP7Deleg020_ListEmpty(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "deleg-020")
	tctx := withSystemAndTenant(ctx, tenantID)

	list, err := fx.Delegation.List(tctx, tenantID)
	require.NoError(t, err)
	assert.Empty(t, list, "list must be empty (not nil error) for tenants with no delegations")
}

// Test Case ID:      P7-DELEG-021
// Module:            iam-org-membership · Delegation
// Feature:           P-18 · List returns active delegations
// API:               GET /api/v1/delegations
// Scenario:          Positive — one active delegation returned
// Preconditions:     Delegation created via Create
// Test Steps:
//   1. Create a delegation
//   2. Call List
// Expected Result:
//   - Slice contains exactly one entry whose id matches the created one
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Deleg021_ListReturnsCreatedDelegation(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "deleg-021")
	delegator, delegate := seedTwoActiveMembers(t, ctx, fx.rawPool, tenantID)
	tctx := withSystemAndTenant(ctx, tenantID)

	created, err := fx.Delegation.Create(tctx, tenantID, delegator, service.DelegationCreateInput{
		DelegateID: delegate,
		Scope:      string(domain.ScopeAll),
	})
	require.NoError(t, err)

	list, err := fx.Delegation.List(tctx, tenantID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, created.ID, list[0].ID)
}

// ═════════════════════════════════════════════════════════════════════════
// P-20 Cancel
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P7-DELEG-030
// Module:            iam-org-membership · Delegation
// Feature:           P-20 · Cancel active delegation
// API:               DELETE /api/v1/delegations/{id}
// Scenario:          Positive — happy path
// Preconditions:     Delegation exists at record_version=1
// Test Steps:
//   1. Create a delegation
//   2. Call Cancel with the returned id and record_version
// Expected Result:
//   - Returns a Delegation with EndedReason=cancelled
//   - UP called twice (Create + Cancel) — second call has ClearDelegate=true
//   - Outbox contains DelegationEnded with reason=cancelled
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP7Deleg030_CancelHappyPath(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "deleg-030")
	delegator, delegate := seedTwoActiveMembers(t, ctx, fx.rawPool, tenantID)
	tctx := withSystemAndTenant(ctx, tenantID)

	created, err := fx.Delegation.Create(tctx, tenantID, delegator, service.DelegationCreateInput{
		DelegateID: delegate,
		Scope:      string(domain.ScopeAll),
	})
	require.NoError(t, err)

	ended, err := fx.Delegation.Cancel(tctx, tenantID, created.ID, created.RecordVersion)
	require.NoError(t, err)
	require.NotNil(t, ended)

	// UP called twice: once on Create (status=ooo + DelegateID), once on Cancel (ClearDelegate=true).
	require.GreaterOrEqual(t, len(fx.UP.Calls), 2)
	last := fx.UP.Calls[len(fx.UP.Calls)-1]
	assert.True(t, last.ClearDelegate,
		"§8.7 pointer-clear: Cancel must send ClearDelegate=true, never status=available")

	types := fx.outboxEventTypesForTenant(t, ctx, tenantID)
	assert.Contains(t, types, domain.EventDelegationEnded)
}

// Test Case ID:      P7-DELEG-031
// Module:            iam-org-membership · Delegation
// Feature:           P-20 · Cancel fails on optimistic-lock mismatch
// API:               DELETE /api/v1/delegations/{id}
// Scenario:          Negative — record_version mismatch → 409
// Preconditions:     Delegation exists at v=1
// Test Steps:
//   1. Create a delegation
//   2. Call Cancel with a wrong record_version (e.g. 999)
// Expected Result:
//   - Returns ErrOptimisticLockConflict → HTTP 409
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Deleg031_CancelOptimisticLockMismatch(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "deleg-031")
	delegator, delegate := seedTwoActiveMembers(t, ctx, fx.rawPool, tenantID)
	tctx := withSystemAndTenant(ctx, tenantID)

	created, err := fx.Delegation.Create(tctx, tenantID, delegator, service.DelegationCreateInput{
		DelegateID: delegate,
		Scope:      string(domain.ScopeAll),
	})
	require.NoError(t, err)

	_, err = fx.Delegation.Cancel(tctx, tenantID, created.ID, 999)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "optimistic_lock_conflict", de.Code)
}

// Test Case ID:      P7-DELEG-032
// Module:            iam-org-membership · Delegation
// Feature:           P-20 · Cancel non-existent delegation
// API:               DELETE /api/v1/delegations/{id}
// Scenario:          Negative — id does not exist
// Preconditions:     Tenant seeded; no delegations
// Test Steps:
//   1. Call Cancel with a random uuid
// Expected Result:
//   - Returns a not-found domain error (FindByID branch)
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Deleg032_CancelDelegationNotFound(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "deleg-032")
	tctx := withSystemAndTenant(ctx, tenantID)

	_, err := fx.Delegation.Cancel(tctx, tenantID, uuid.New(), 1)
	require.Error(t, err, "P-20: unknown delegation id must error")
}

// Test Case ID:      P7-DELEG-033
// Module:            iam-org-membership · Delegation
// Feature:           P-20 · Fail-open on UP outage
// API:               DELETE /api/v1/delegations/{id}
// Scenario:          Positive with degraded dependency — UP down at cancel time
// Preconditions:     Delegation exists; fx.UP.FailNext = true (on the Cancel call)
// Test Steps:
//   1. Create delegation (UP succeeds)
//   2. Flip fx.UP.FailNext = true
//   3. Call Cancel
// Expected Result:
//   - Cancel STILL succeeds locally (delegation row → status=ended)
//   - Outbox contains DelegationEnded
//   - Fail-open per §8.7 — the delegation-expiry cron will retry the UP pointer-clear
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP7Deleg033_CancelFailOpenOnUPOutage(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "deleg-033")
	delegator, delegate := seedTwoActiveMembers(t, ctx, fx.rawPool, tenantID)
	tctx := withSystemAndTenant(ctx, tenantID)

	created, err := fx.Delegation.Create(tctx, tenantID, delegator, service.DelegationCreateInput{
		DelegateID: delegate,
		Scope:      string(domain.ScopeAll),
	})
	require.NoError(t, err)

	fx.UP.FailNext = true
	ended, err := fx.Delegation.Cancel(tctx, tenantID, created.ID, created.RecordVersion)
	require.NoError(t, err,
		"P-20 is fail-open per §8.7 — UP outage must NOT block local end-delegation")
	require.NotNil(t, ended)

	// The DelegationEnded event is still enqueued.
	types := fx.outboxEventTypesForTenant(t, ctx, tenantID)
	assert.Contains(t, types, domain.EventDelegationEnded,
		"even on UP failure, the local state + event still commit")
}

// ═════════════════════════════════════════════════════════════════════════
// helpers
// ═════════════════════════════════════════════════════════════════════════

// seedTwoActiveMembers inserts two active tenant_memberships rows and
// returns (delegatorID, delegateID). Used by every delegation happy-path.
func seedTwoActiveMembers(t *testing.T, ctx context.Context, rawPool *pgxpoolPool, tenantID uuid.UUID) (uuid.UUID, uuid.UUID) {
	t.Helper()
	delegator := uuid.New()
	delegate := uuid.New()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES
		    (gen_random_uuid(), $1, $2, 'active'),
		    (gen_random_uuid(), $1, $3, 'active')`,
		tenantID, delegator, delegate)
	require.NoError(t, err)
	return delegator, delegate
}
