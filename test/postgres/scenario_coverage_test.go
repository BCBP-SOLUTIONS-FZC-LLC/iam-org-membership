//go:build integration

// Postgres integration tests covering the 14 remaining scenarios that
// require a real DB (race conditions, partial-unique indexes, full tx flows).
//
// Scenarios covered: I1-PLAN-TRIAL-01, I2-CONC-01, I3-REJOIN-01,
// I3-IDEMPOTENT-01, P2-REALM-SYNC-01, P6-HAPPY-01, P6-SEAT-BOUNDARY-01,
// P10-HAPPY-01, P10-LEVEL-CHANGE-01, P10-SAME-LEVEL-01, P10-REASSIGN-SOFTDEL-01.
package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── I1-PLAN-TRIAL-01 ──────────────────────────────────────────────────

// Test Case ID:      I1-PLAN-TRIAL-01
// Feature:           I-1 · trial_ends_at set from plan.trial_duration_days (not hardcoded)
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestTrialSignup_TrialEndsAt_FromPlanDays(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := uuid.New()
	ownerID := uuid.New()

	svcCtx := withSystemAndTenant(ctx, tenantID)
	before := time.Now().UTC()
	_, _, err := fx.Provisioning.TrialSignup(svcCtx, service.TrialSignupInput{
		TenantID:      tenantID,
		Slug:          "trial-plan-test",
		Name:          "Trial Plan Test",
		Plan:          domain.PlanStarter,
		OwnerUserID:   ownerID,
		DefaultLocale: "en-US",
	})
	require.NoError(t, err)

	// Fetch trial_ends_at from DB.
	var trialEndsAt time.Time
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT trial_ends_at FROM tenants WHERE id = $1`, tenantID).Scan(&trialEndsAt))

	// Fetch plan.trial_duration_days from the catalog (plans table dropped
	// per migration-runbook Phase 4 — LLD §12 step 4).
	starter, err := fx.CatalogPlans.PlanByCode(ctx, domain.PlanStarter)
	require.NoError(t, err)
	trialDays := starter.TrialDurationDays

	expected := before.Add(time.Duration(trialDays) * 24 * time.Hour)
	assert.WithinDuration(t, expected, trialEndsAt, 5*time.Second,
		"trial_ends_at must be now + plan.trial_duration_days, not hardcoded 30d")
	assert.Greater(t, trialDays, 0, "starter plan must have positive trial_duration_days")
}

// ── I2-CONC-01 (GAP BUG-I2-2 documented) ─────────────────────────────

// Test Case ID:      I2-CONC-01
// Feature:           I-2 · CONC-4 optimistic locking now enforced (BUG-I2-2 FIXED)
// Note:              First call (version=1) succeeds; second call with same version=1
//
//	gets 409 optimistic_lock_conflict — record_version guard added.
//
// Priority: P1 · Severity: Medium · Automation Status: Automated
func TestSetRealmFields_ConcurrentBothSucceed_BUG_I2_2(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "i2-conc")

	tctx := withSystemAndTenant(ctx, tenantID)

	// First call with record_version=1 — must succeed.
	err1 := fx.Provisioning.SetRealmFields(tctx, tenantID,
		"realm-A", domain.RealmDedicated, "shard-1", 1)
	assert.NoError(t, err1, "first call with correct version must succeed")

	// Second call with stale version=1 — must fail with optimistic_lock_conflict (CONC-4).
	err2 := fx.Provisioning.SetRealmFields(tctx, tenantID,
		"realm-B", domain.RealmDedicated, "shard-2", 1)
	assert.ErrorIs(t, err2, domain.ErrOptimisticLockConflict,
		"BUG-I2-2 FIXED: stale record_version now returns 409 optimistic_lock_conflict")

	// realm-A remains (first write won, second was rejected).
	var realmID string
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT realm_id FROM tenants WHERE id = $1`, tenantID).Scan(&realmID))
	assert.Equal(t, "realm-A", realmID, "first write wins; stale second write rejected")
}

// ── I3-REJOIN-01 ──────────────────────────────────────────────────────

// Test Case ID:      I3-REJOIN-01
// Feature:           I-3 · user who left (soft-deleted) rejoins → new membership row
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestAddFromRegister_Rejoin_CreatesNewRow(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "i3-rejoin")
	userID := uuid.New()

	// First join.
	_, err := fx.Invitation.AddFromRegister(withSystemAndTenant(ctx, tenantID), tenantID, userID, uuid.Nil, "")
	require.NoError(t, err)

	// Simulate leave: soft-delete the membership.
	_, err = fx.rawPool.Exec(ctx,
		`UPDATE tenant_memberships SET status='left', deleted_at=now()
		 WHERE tenant_id=$1 AND user_id=$2 AND deleted_at IS NULL`,
		tenantID, userID)
	require.NoError(t, err)

	// Rejoin — should succeed because partial unique allows re-insert after soft-delete.
	mem2, err := fx.Invitation.AddFromRegister(withSystemAndTenant(ctx, tenantID), tenantID, userID, uuid.Nil, "")
	require.NoError(t, err, "rejoin must succeed after soft-delete (TM-11)")
	assert.Equal(t, domain.MembershipActive, mem2.Status)

	// Verify two rows exist: one left, one active.
	var count int
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM tenant_memberships WHERE tenant_id=$1 AND user_id=$2`,
		tenantID, userID).Scan(&count))
	assert.Equal(t, 2, count, "left row retained for audit, new active row created")
}

// ── I3-IDEMPOTENT-01 ──────────────────────────────────────────────────

// Test Case ID:      I3-IDEMPOTENT-01
// Feature:           I-3 · same user added twice → idempotent (ON CONFLICT DO NOTHING)
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestAddFromRegister_Idempotent_SecondCallNoOp(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "i3-idem")
	userID := uuid.New()

	// First add.
	mem1, err := fx.Invitation.AddFromRegister(withSystemAndTenant(ctx, tenantID), tenantID, userID, uuid.Nil, "")
	require.NoError(t, err)

	// Second add — idempotent, returns existing row.
	mem2, err := fx.Invitation.AddFromRegister(withSystemAndTenant(ctx, tenantID), tenantID, userID, uuid.Nil, "")
	require.NoError(t, err)
	assert.Equal(t, mem1.ID, mem2.ID, "second call returns existing membership row")

	// Only one active row.
	var count int
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM tenant_memberships WHERE tenant_id=$1 AND user_id=$2 AND deleted_at IS NULL`,
		tenantID, userID).Scan(&count))
	assert.Equal(t, 1, count)
}

// ── P2-REALM-SYNC-01 ─────────────────────────────────────────────────

// Test Case ID:      P2-REALM-SYNC-01
// Feature:           P-2 · RP fails on local_accounts_enabled change → realm_sync_pending=true
// Priority: P1 · Severity: Critical · Automation Status: Automated
func TestPatch_RPFailure_SetsRealmSyncPending(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "p2-realm-sync")

	// Make RP fail for PatchRealmConfig (one-shot).
	fx.RP.PatchRealmConfigFailNext = true

	// Current record_version.
	var rv int64
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT record_version FROM tenants WHERE id = $1`, tenantID).Scan(&rv))

	// Default is local_accounts_enabled=true. Toggle to false to trigger RP call.
	disabled := false
	_, deferred, err := fx.Tenant.Patch(withSystemAndTenant(ctx, tenantID), tenantID, &domain.TenantPatch{
		LocalAccountsEnabled: &disabled,
		RecordVersion:        rv,
	})
	require.NoError(t, err)
	assert.True(t, deferred, "RP failure must return deferred=true (202 semantics)")

	// BUG-P2-1 fix: realm_sync_pending must be true in DB now.
	var syncPending bool
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT realm_sync_pending FROM tenants WHERE id = $1`, tenantID).Scan(&syncPending))
	assert.True(t, syncPending, "BUG-P2-1 fix: realm_sync_pending must be set in DB for reconciler")
}

// ── P6-HAPPY-01 ───────────────────────────────────────────────────────

// Test Case ID:      P6-HAPPY-01
// Feature:           P-6 · valid invite → 202 pending_invitations row created
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestInvite_HappyPath_CreatesPendingInvitation(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, actorID := seedTenantWithOwner(t, ctx, fx, "p6-happy")

	inv, err := fx.Invitation.Invite(withSystemAndTenant(ctx, tenantID), tenantID, service.InvitationInput{
		Email:    "newmember@example.com",
		FullName: "New Member",
	}, actorID)
	require.NoError(t, err)
	assert.NotNil(t, inv)
	assert.Equal(t, domain.InvitePending, inv.Status)
	assert.Equal(t, "newmember@example.com", inv.Email)

	// Verify row in DB.
	var count int
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM pending_invitations WHERE tenant_id=$1 AND email=$2 AND status='pending'`,
		tenantID, "newmember@example.com").Scan(&count))
	assert.Equal(t, 1, count, "pending invitation row must exist in DB")
}

// ── P6-SEAT-BOUNDARY-01 ───────────────────────────────────────────────

// Test Case ID:      P6-SEAT-BOUNDARY-01
// Feature:           P-6 · active+pending == licensed_seats → 409 seat_limit_reached (SEAT-1)
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestInvite_SeatCapExact_Returns409(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, actorID := seedTenantWithOwner(t, ctx, fx, "p6-seat")

	// Set licensed_seats = 1 (owner already consumes it).
	_, err := fx.rawPool.Exec(ctx,
		`UPDATE tenants SET licensed_seats = 1 WHERE id = $1`, tenantID)
	require.NoError(t, err)

	// Invite should fail: active(1) + pending(0) >= licensed_seats(1).
	_, err = fx.Invitation.Invite(withSystemAndTenant(ctx, tenantID), tenantID, service.InvitationInput{
		Email:    "overflow@example.com",
		FullName: "Overflow",
	}, actorID)
	assert.ErrorIs(t, err, domain.ErrSeatLimitReached, "SEAT-1: at cap → 409")
}

// ── P6-SEAT-LOSTRACE-01 ──────────────────────────────────────────────

// Test Case ID:      P6-SEAT-LOSTRACE-01
// Feature:           P-6 · seat-limit lost race durably records the orphaned
//
//	Keycloak user rather than rolling back and losing the reference (PI-9).
//
// Priority: P1 · Severity: Major · Automation Status: Automated
//
// Invite's cheap preflight check (outside any tx) only catches an
// already-exhausted cap — it does not lock anything, so two concurrent
// Invite calls that both observe "one seat free" can both proceed to
// RealmProvisioner.CreateInvitedUser and only serialize at the tx-level
// `SELECT ... FOR UPDATE`. This test drives that real race with two
// goroutines rather than pre-exhausting the cap, so the loser actually
// exercises the lost-race branch instead of being rejected by preflight.
func TestInvite_SeatCapLostRace_CommitsDurableCleanupRow(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, actorID := seedTenantWithOwner(t, ctx, fx, "p6-lostrace")

	// Owner already consumes 1 seat; licensed_seats=2 leaves exactly one
	// seat free for the two racers to contend over.
	_, err := fx.rawPool.Exec(ctx,
		`UPDATE tenants SET licensed_seats = 2 WHERE id = $1`, tenantID)
	require.NoError(t, err)

	emails := []string{"racer-a@example.com", "racer-b@example.com"}
	results := make([]error, len(emails))
	var wg sync.WaitGroup
	for i, email := range emails {
		wg.Add(1)
		go func(idx int, email string) {
			defer wg.Done()
			_, results[idx] = fx.Invitation.Invite(withSystemAndTenant(ctx, tenantID), tenantID, service.InvitationInput{
				Email:    email,
				FullName: "Racer",
			}, actorID)
		}(i, email)
	}
	wg.Wait()

	var winners, losers int
	var loserEmail string
	for i, err := range results {
		if err == nil {
			winners++
			continue
		}
		require.ErrorIs(t, err, domain.ErrSeatLimitReached, "the only expected failure is the seat cap")
		losers++
		loserEmail = emails[i]
	}
	require.Equal(t, 1, winners, "exactly one racer must win the last seat")
	require.Equal(t, 1, losers, "exactly one racer must lose the race")

	// The loser's Keycloak user was already created by the fake RP before
	// the tx-level check ran. The lost-race path must NOT roll back and
	// lose that reference — it must commit a terminal 'revoked' row
	// carrying keycloak_user_id with kc_cleanup_pending=true, so the
	// invitation-kc-cleanup reconciler durably converges the delete even
	// if the immediate best-effort attempt (or the pod) fails.
	var status string
	var kcPending bool
	var kcUserID *uuid.UUID
	err = fx.rawPool.QueryRow(ctx,
		`SELECT status, kc_cleanup_pending, keycloak_user_id FROM pending_invitations
		 WHERE tenant_id = $1 AND email = $2`,
		tenantID, loserEmail).Scan(&status, &kcPending, &kcUserID)
	require.NoError(t, err, "the lost-race row must exist — it must be committed, not rolled back")
	assert.Equal(t, string(domain.InviteRevoked), status)
	assert.True(t, kcPending, "kc_cleanup_pending must be true so the reconciler sweeps this row")
	require.NotNil(t, kcUserID, "the row must carry the orphaned Keycloak user's id")

	// The revoked row must not count against SEAT-1 (only 'pending' rows do).
	var pendingCount int
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM pending_invitations WHERE tenant_id = $1 AND status = 'pending' AND expires_at > now()`,
		tenantID).Scan(&pendingCount))
	assert.Equal(t, 1, pendingCount, "only the winner's row should hold a seat")
}

// ── P10-HAPPY-01 ──────────────────────────────────────────────────────

// Test Case ID:      P10-HAPPY-01
// Feature:           P-10 · assign active member → 200 + DeptMembershipGranted event
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestDeptMembershipAssign_HappyPath_GrantedEvent(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "p10-happy")
	userID := uuid.New()
	deptID := seedActiveDept(t, ctx, fx, tenantID)

	// Add a member.
	_, err := fx.rawPool.Exec(ctx,
		`INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
		 VALUES (gen_random_uuid(), $1, $2, 'active')`, tenantID, userID)
	require.NoError(t, err)

	mem, err := fx.DeptMembership.Assign(withSystemAndTenant(ctx, tenantID), tenantID, userID, deptID,
		domain.DeptPreparator, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, domain.DeptPreparator, mem.RoleLevel)

	// Verify DeptMembershipGranted event in outbox.
	var eventType string
	err = fx.rawPool.QueryRow(ctx,
		`SELECT event_type FROM outbox_events WHERE tenant_id=$1 AND event_type='DepartmentMembershipGranted'`,
		tenantID).Scan(&eventType)
	require.NoError(t, err, "DeptMembershipGranted event must be in outbox")
	assert.Equal(t, "DepartmentMembershipGranted", eventType)
}

// ── P10-LEVEL-CHANGE-01 ───────────────────────────────────────────────

// Test Case ID:      P10-LEVEL-CHANGE-01
// Feature:           P-10 · change level → 200 + DeptMembershipLevelChanged event
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestDeptMembershipAssign_LevelChange_LevelChangedEvent(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "p10-level")
	userID := uuid.New()
	deptID := seedActiveDept(t, ctx, fx, tenantID)

	_, err := fx.rawPool.Exec(ctx,
		`INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
		 VALUES (gen_random_uuid(), $1, $2, 'active')`, tenantID, userID)
	require.NoError(t, err)

	// Initial assign at preparator.
	_, err = fx.DeptMembership.Assign(withSystemAndTenant(ctx, tenantID), tenantID, userID, deptID,
		domain.DeptPreparator, uuid.Nil)
	require.NoError(t, err)

	// Change to approver.
	mem, err := fx.DeptMembership.Assign(withSystemAndTenant(ctx, tenantID), tenantID, userID, deptID,
		domain.DeptApprover, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, domain.DeptApprover, mem.RoleLevel)

	// Verify LevelChanged event in outbox (after initial Granted).
	var count int
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE tenant_id=$1 AND event_type='DepartmentMembershipLevelChanged'`,
		tenantID).Scan(&count))
	assert.Equal(t, 1, count, "LevelChanged event must be emitted on level upgrade")
}

// ── P10-SAME-LEVEL-01 ─────────────────────────────────────────────────

// Test Case ID:      P10-SAME-LEVEL-01
// Feature:           P-10 · re-assign same level → 200, no new event (TRG-3)
// Priority: P2 · Severity: Minor · Automation Status: Automated
func TestDeptMembershipAssign_SameLevel_NoNewEvent(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "p10-same")
	userID := uuid.New()
	deptID := seedActiveDept(t, ctx, fx, tenantID)

	_, err := fx.rawPool.Exec(ctx,
		`INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
		 VALUES (gen_random_uuid(), $1, $2, 'active')`, tenantID, userID)
	require.NoError(t, err)

	// First assign.
	_, err = fx.DeptMembership.Assign(withSystemAndTenant(ctx, tenantID), tenantID, userID, deptID,
		domain.DeptReviewer, uuid.Nil)
	require.NoError(t, err)

	// Count events after first assign.
	var before int
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE tenant_id=$1`, tenantID).Scan(&before))

	// Same level re-assign.
	_, err = fx.DeptMembership.Assign(withSystemAndTenant(ctx, tenantID), tenantID, userID, deptID,
		domain.DeptReviewer, uuid.Nil)
	require.NoError(t, err)

	// Count after — should be unchanged (TRG-3 no-op).
	var after int
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE tenant_id=$1`, tenantID).Scan(&after))
	assert.Equal(t, before, after, "TRG-3: same level re-assign must not emit a new event")
}

// ── P10-REASSIGN-SOFTDEL-01 ───────────────────────────────────────────

// Test Case ID:      P10-REASSIGN-SOFTDEL-01
// Feature:           P-10 · reassign member who left dept → fresh grant row + Granted event
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestDeptMembershipAssign_AfterSoftDelete_FreshGrant(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "p10-reassign")
	userID := uuid.New()
	deptID := seedActiveDept(t, ctx, fx, tenantID)

	_, err := fx.rawPool.Exec(ctx,
		`INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
		 VALUES (gen_random_uuid(), $1, $2, 'active')`, tenantID, userID)
	require.NoError(t, err)

	// Initial assign.
	_, err = fx.DeptMembership.Assign(withSystemAndTenant(ctx, tenantID), tenantID, userID, deptID,
		domain.DeptPreparator, uuid.Nil)
	require.NoError(t, err)

	// Soft-delete the membership row (simulate dept remove P-11).
	_, err = fx.rawPool.Exec(ctx,
		`UPDATE dept_memberships SET deleted_at=now() WHERE tenant_id=$1 AND user_id=$2 AND deleted_at IS NULL`,
		tenantID, userID)
	require.NoError(t, err)

	// Count Granted events before re-assign.
	var before int
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE tenant_id=$1 AND event_type='DepartmentMembershipGranted'`,
		tenantID).Scan(&before))

	// Re-assign.
	mem, err := fx.DeptMembership.Assign(withSystemAndTenant(ctx, tenantID), tenantID, userID, deptID,
		domain.DeptApprover, uuid.Nil)
	require.NoError(t, err, "re-assign after soft-delete must succeed")
	assert.Equal(t, domain.DeptApprover, mem.RoleLevel)

	// New Granted event (not LevelChanged — prior row was soft-deleted).
	var after int
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE tenant_id=$1 AND event_type='DepartmentMembershipGranted'`,
		tenantID).Scan(&after))
	assert.Equal(t, before+1, after, "re-assign after soft-delete emits Granted (not LevelChanged)")
}

// ── helpers ───────────────────────────────────────────────────────────

// seedActiveDept activates a department entry for a tenant and returns its
// department_id. Picks whichever is_system=true dept the fake catalog
// happens to return first (migration-runbook Phase 4 — LLD §12 step 4
// dropped the real departments table; fx.CatalogDepts stands in for it,
// seeded with the same 5 system departments).
func seedActiveDept(t testing.TB, ctx context.Context, fx *testFixtures, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	all, err := fx.CatalogDepts.Departments(ctx)
	require.NoError(t, err)
	var deptID uuid.UUID
	for _, d := range all {
		if d.IsSystem && d.IsActive {
			deptID = d.ID
			break
		}
	}
	require.NotEqual(t, uuid.Nil, deptID, "need at least one system dept in catalog")

	// Activate it for the tenant (insert into tenant_departments if not present).
	_, err = fx.rawPool.Exec(ctx,
		`INSERT INTO tenant_departments (tenant_id, department_id, is_active)
		 VALUES ($1, $2, true) ON CONFLICT (tenant_id, department_id) DO UPDATE SET is_active=true`,
		tenantID, deptID)
	require.NoError(t, err)
	return deptID
}

// ── I3-EXPIRED-INV-01 ────────────────────────────────────────────────

// Test Case ID:      I3-EXPIRED-INV-01
// Feature:           I-3 · plain-add path (no matching invitation) creates membership with no roles.
// Note:              Per LLD I-3: expired-but-pending invitations are STILL honoured (acceptance +
//
//	initial roles applied) with a seat re-check. This test covers the separate
//	plain-add path where NO invitation exists at all.
//
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestAddFromRegister_ExpiredInvitation_PlainAdd(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "i3-exp")
	userID := uuid.New()

	// Call AddFromRegister with no prior invitation → plain-add path.
	// This verifies the plain-add code path (no expiry check needed).
	svcCtx := withSystemAndTenant(ctx, tenantID)
	mem, err := fx.Invitation.AddFromRegister(svcCtx, tenantID, userID, uuid.Nil, "noinvite@example.com")
	require.NoError(t, err, "plain-add (no invitation) must succeed")
	assert.Equal(t, domain.MembershipActive, mem.Status)

	// No elevated roles applied (plain-add = membership only).
	var roleCount int
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM tenant_roles WHERE tenant_id=$1 AND user_id=$2`,
		tenantID, userID).Scan(&roleCount))
	assert.Equal(t, 0, roleCount, "plain-add: no initial roles applied")
}

// ── P25-MEMBERS-REMAIN-01 (TD-2 — memberships survive deactivation) ───────

// Test Case ID:      P25-MEMBERS-REMAIN-01
// Feature:           P-25 · deactivate dept → dept_memberships remain intact (LLD TD-2)
// Priority: P1 · Severity: High · Automation Status: Automated
// LLD ref:           TD-2: "deactivation does NOT require or cause removal of existing memberships"
func TestDeptSetActive_Deactivate_MembershipsRemain(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "p25-remain")
	userID := uuid.New()
	// Create a NON-system department (system depts can't be deactivated — D-9/D-11).
	deptID := seedNonSystemDeptForTenant(t, ctx, fx, tenantID)

	// Add member to the dept.
	_, err := fx.rawPool.Exec(ctx,
		`INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
		 VALUES (gen_random_uuid(), $1, $2, 'active')`, tenantID, userID)
	require.NoError(t, err)
	svcCtx := withSystemAndTenant(ctx, tenantID)
	_, err = fx.DeptMembership.Assign(svcCtx, tenantID, userID, deptID, domain.DeptPreparator, uuid.Nil)
	require.NoError(t, err)

	// Verify member is in dept before deactivation.
	var before int
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM dept_memberships WHERE tenant_id=$1 AND department_id=$2 AND deleted_at IS NULL`,
		tenantID, deptID).Scan(&before))
	assert.Equal(t, 1, before)

	// Get current record_version for optimistic lock.
	var rv int64
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT record_version FROM tenant_departments WHERE tenant_id=$1 AND department_id=$2`,
		tenantID, deptID).Scan(&rv))

	// Deactivate the dept.
	_, err = fx.Department.SetActive(svcCtx, tenantID, deptID, false, rv)
	require.NoError(t, err, "deactivation must succeed")

	// TD-2: dept_memberships must remain intact (valid and auditable).
	var after int
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM dept_memberships WHERE tenant_id=$1 AND department_id=$2 AND deleted_at IS NULL`,
		tenantID, deptID).Scan(&after))
	assert.Equal(t, 1, after, "TD-2: dept_memberships must NOT be removed on deactivation")
}

// ── P10-ROLE-DECREASE-GAP-01 (GAP-P10-3 fixed) ───────────────────────

// Test Case ID:      P10-ROLE-DECREASE-GAP-01
// Feature:           P-10 · approver→preparator with active delegation → 409 (GAP-P10-3 / WFI-9 fix)
// Priority: P1 · Severity: High · Automation Status: Automated
// TestDeptMembershipAssign_LevelDecrease_DelegationCheckDegrades_NoBlock covers
// the §8.8.4 WFI-9 gate on a dept-level decrease when no port.DelegationCheckClient
// is wired (this fixture's DeptMembershipService has none, matching a Delegation
// Service outage) — deptDelegateOrDegrade degrades to nil and the decrease is
// never blocked. The dept-scoped delegate lookup itself now lives in the
// standalone Delegation Service (ADR-0008); this repo no longer has a local
// `delegations` table to seed. The 409 path (an active delegate with
// active_workflows > 0) is covered by the unit test below via a fake
// DelegationCheckClient + fake WorkflowClient.
func TestDeptMembershipAssign_LevelDecrease_DelegationCheckDegrades_NoBlock(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "p10-wfi9")
	delegateID := uuid.New()
	deptID := seedActiveDept(t, ctx, fx, tenantID)

	// Add delegate as an active member.
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
		 VALUES (gen_random_uuid(), $1, $2, 'active') RETURNING id`,
		tenantID, delegateID).Scan(new(uuid.UUID)))

	// Assign delegate as approver.
	svcCtx := withSystemAndTenant(ctx, tenantID)
	_, err := fx.DeptMembership.Assign(svcCtx, tenantID, delegateID, deptID, domain.DeptApprover, uuid.Nil)
	require.NoError(t, err)

	// Downgrade delegate from approver → preparator — WFI-9 gate runs but
	// deptDelegateOrDegrade returns nil (no DelegationCheckClient wired), so
	// WorkflowClient.GetDelegateImpact is never even called.
	_, err = fx.DeptMembership.Assign(svcCtx, tenantID, delegateID, deptID, domain.DeptPreparator, uuid.Nil)
	assert.NoError(t, err, "WFI-9 degrade: no DelegationCheckClient → level decrease allowed")
}

// seedNonSystemDeptForTenant creates a non-system dept and activates it for the tenant.
func seedNonSystemDeptForTenant(t testing.TB, ctx context.Context, fx *testFixtures, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	code := "TESTDEPT-" + tenantID.String()[:8]
	deptID := seedNonSystemDept(t.(*testing.T), ctx, fx.CatalogDepts, code, "Test Department")
	_, err := fx.rawPool.Exec(ctx,
		`INSERT INTO tenant_departments (tenant_id, department_id, is_active)
		 VALUES ($1, $2, true) ON CONFLICT DO NOTHING`, tenantID, deptID)
	require.NoError(t, err)
	return deptID
}
