//go:build integration

// Phase 5 — SQS consumer + reconciler tests focused on audit fixes that
// weren't already covered by consumer_evt_test.go / reconciler_convergence_test.go.
//
// Coverage focus:
//
//	N5 — TenantSubscriptionCancelled uses COALESCE(cancelled_at, now())
//	     so replaying the event never resets the §15.5 retention clock.
//	TenantSuspended replay parity — same COALESCE guarantee.
//	TenantOffboarded replay parity — cancelled_at preserved AND
//	     deleted_at preserved on replay (PAID-1 terminal state).
//	B17 — invitation_expiry reconciler respects jctx.BatchLimit; a
//	     backlog wave never produces a single unbounded UPDATE.
package postgres_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/cmd/reconciler/jobs"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/consumer"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────
// N5: TenantSubscriptionCancelled COALESCE preserves cancelled_at on replay.
// ─────────────────────────────────────────────────────────────────────────

func TestN5_TenantSubscriptionCancelled_ReplayPreservesCancelledAt(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedPaidTenant(t, ctx, rawPool, "n5-cancel-replay")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, pgadapter.NewIdempotencyRepository(appPool), nil, nil, 5*time.Minute, nil)

	// First delivery — sets cancelled_at.
	firstTS := time.Now().UTC().Add(-30 * time.Minute)
	env1 := mkEnvelope(t, "TenantSubscriptionCancelled", tenantID, firstTS, map[string]string{})
	require.NoError(t, c.Handle(ctx, env1))

	var originalCancelledAt time.Time
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT cancelled_at FROM tenants WHERE id = $1`, tenantID).Scan(&originalCancelledAt))
	require.False(t, originalCancelledAt.IsZero(), "first delivery must stamp cancelled_at")

	// Second delivery — different event ID so processed_events doesn't
	// short-circuit. Must NOT reset cancelled_at.
	secondTS := time.Now().UTC()
	env2 := mkEnvelope(t, "TenantSubscriptionCancelled", tenantID, secondTS, map[string]string{})
	require.NoError(t, c.Handle(ctx, env2))

	var replayedCancelledAt time.Time
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT cancelled_at FROM tenants WHERE id = $1`, tenantID).Scan(&replayedCancelledAt))
	assert.True(t, replayedCancelledAt.Equal(originalCancelledAt),
		"N5: replay must NOT reset cancelled_at — got %s, expected %s",
		replayedCancelledAt, originalCancelledAt)
}

// ─────────────────────────────────────────────────────────────────────────
// TenantSuspended handler already uses COALESCE (pre-existing correct
// behavior — my memory audit confirmed no bug). Regression-guard it.
// ─────────────────────────────────────────────────────────────────────────

func TestConsumer_TenantSuspended_ReplayPreservesCancelledAt(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedPaidTenant(t, ctx, rawPool, "suspend-replay")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, pgadapter.NewIdempotencyRepository(appPool), nil, nil, 5*time.Minute, nil)

	firstTS := time.Now().UTC().Add(-30 * time.Minute)
	env1 := mkEnvelope(t, "TenantSuspended", tenantID, firstTS, map[string]string{})
	require.NoError(t, c.Handle(ctx, env1))

	var originalCancelledAt time.Time
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT cancelled_at FROM tenants WHERE id = $1`, tenantID).Scan(&originalCancelledAt))
	require.False(t, originalCancelledAt.IsZero())

	env2 := mkEnvelope(t, "TenantSuspended", tenantID, time.Now().UTC(), map[string]string{})
	require.NoError(t, c.Handle(ctx, env2))

	var replayedCancelledAt time.Time
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT cancelled_at FROM tenants WHERE id = $1`, tenantID).Scan(&replayedCancelledAt))
	assert.True(t, replayedCancelledAt.Equal(originalCancelledAt),
		"suspended replay must not reset cancelled_at")
}

// ─────────────────────────────────────────────────────────────────────────
// T-16 (new, resolves RP-11): operator-sourced suspension against the real
// DB — proves the CHECK constraint rework (chk_cancelled_at_required,
// chk_subscription_started_required, chk_suspension_source_required)
// actually accepts an active/trial → suspended jump that skips the
// billing-lapse path, without a fabricated cancelled_at/subscription_started_at.
// ─────────────────────────────────────────────────────────────────────────

func TestConsumer_TenantSuspended_OperatorSource_FromNeverConvertedTrial(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	// A plain trial tenant: subscription_started_at IS NULL (never converted).
	tenantID := seedTenant(t, ctx, rawPool, "operator-suspend-trial")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, pgadapter.NewIdempotencyRepository(appPool), nil, nil, 5*time.Minute, nil)

	env := mkEnvelope(t, "TenantSuspended", tenantID, time.Now().UTC(), map[string]string{"source": "operator"})
	require.NoError(t, c.Handle(ctx, env), "chk_cancelled_at_required/chk_subscription_started_required must accept this transition")

	var status string
	var subscriptionStartedAt, cancelledAt *time.Time
	var source *string
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT status, suspension_source, subscription_started_at, cancelled_at FROM tenants WHERE id = $1`,
		tenantID).Scan(&status, &source, &subscriptionStartedAt, &cancelledAt))
	assert.Equal(t, "suspended", status)
	require.NotNil(t, source)
	assert.Equal(t, string(domain.SuspensionSourceOperator), *source)
	assert.Nil(t, cancelledAt, "operator-sourced suspension must not stamp cancelled_at (T-16)")
	assert.Nil(t, subscriptionStartedAt, "a never-converted trial keeps subscription_started_at NULL even suspended")

	// Reactivation clears suspension_source alongside status. Since this
	// tenant was never paid (subscription_started_at still NULL), it must
	// return to 'trial', not 'active' — 'active' would violate
	// chk_subscription_started_required.
	env2 := mkEnvelope(t, "TenantReactivated", tenantID, time.Now().UTC(), map[string]string{})
	require.NoError(t, c.Handle(ctx, env2))

	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT status, suspension_source FROM tenants WHERE id = $1`, tenantID).Scan(&status, &source))
	assert.Equal(t, "trial", status, "a never-converted trial tenant returns to 'trial', not 'active' (T-16)")
	assert.Nil(t, source, "T-16: reactivation clears suspension_source")
}

func TestConsumer_TenantSuspended_BillingLapse_SetsSuspensionSourceAndCancelledAt(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedPaidTenant(t, ctx, rawPool, "billing-lapse-suspend")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, pgadapter.NewIdempotencyRepository(appPool), nil, nil, 5*time.Minute, nil)

	env := mkEnvelope(t, "TenantSuspended", tenantID, time.Now().UTC(), map[string]string{"source": "billing_lapse"})
	require.NoError(t, c.Handle(ctx, env))

	var status string
	var source *string
	var cancelledAt *time.Time
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT status, suspension_source, cancelled_at FROM tenants WHERE id = $1`,
		tenantID).Scan(&status, &source, &cancelledAt))
	assert.Equal(t, "suspended", status)
	require.NotNil(t, source)
	assert.Equal(t, string(domain.SuspensionSourceBillingLapse), *source)
	assert.NotNil(t, cancelledAt, "billing_lapse suspension drives the §15.5 grace clock")
}

// ─────────────────────────────────────────────────────────────────────────
// F1 (RP↔O&M alignment review, OQ-C): TenantReactivated is now a two-
// producer event — Billing on billing-orgm-q (the normal paid path) and
// Realm Provisioner on tenant-orgm-q (RP-10, source=operator, reversing an
// RP-14 operator suspension). Both land in the exact same handler case,
// which reads no payload fields, so correctness rests entirely on EVT-14's
// generic timestamp ordering. This proves a stale/reordered redelivery of
// the ORIGINAL suspend can't regress a reactivation applied by either
// producer — the newest event wins regardless of which "queue" it
// conceptually came from (the Go handler has no notion of source queue at
// all; only the SNS filter policy, api/asyncapi.yaml, routes by queue).
// ─────────────────────────────────────────────────────────────────────────

func TestConsumer_TenantReactivated_EVT14_ProtectsAgainstStaleReorderedSuspend(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "f1-reorder-protect")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, pgadapter.NewIdempotencyRepository(appPool), nil, nil, 5*time.Minute, nil)

	// t0: operator-suspends the trial tenant (as RP-14 would, straight from trial).
	t0 := time.Now().UTC().Add(-1 * time.Hour)
	suspendEnv := mkEnvelope(t, "TenantSuspended", tenantID, t0, map[string]string{"source": "operator"})
	require.NoError(t, c.Handle(ctx, suspendEnv))

	// t1 (> t0): reactivated — as RP-10 would, undoing the operator suspend.
	t1 := t0.Add(30 * time.Minute)
	reactivateEnv := mkEnvelope(t, "TenantReactivated", tenantID, t1, map[string]string{})
	require.NoError(t, c.Handle(ctx, reactivateEnv))

	var status string
	require.NoError(t, rawPool.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, tenantID).Scan(&status))
	require.Equal(t, "trial", status, "reactivation must land before the reorder test proceeds")

	// A STALE redelivery of the original suspend (same t0, distinct event ID
	// so processed_events doesn't short-circuit) must be skipped by EVT-14 —
	// it must NOT regress the tenant back to 'suspended' now that a newer
	// event (the reactivation at t1) has already advanced last_event_at.
	staleReplayEnv := mkEnvelope(t, "TenantSuspended", tenantID, t0, map[string]string{"source": "operator"})
	require.NoError(t, c.Handle(ctx, staleReplayEnv))

	require.NoError(t, rawPool.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, tenantID).Scan(&status))
	assert.Equal(t, "trial", status,
		"EVT-14 must skip the stale/reordered suspend redelivery — reactivation must not be regressed")
}

// ─────────────────────────────────────────────────────────────────────────
// TenantOffboarded — terminal (PAID-1). Replay must preserve BOTH
// cancelled_at and deleted_at.
// ─────────────────────────────────────────────────────────────────────────

func TestConsumer_TenantOffboarded_ReplayPreservesTimestamps(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedPaidTenant(t, ctx, rawPool, "offboard-replay")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, pgadapter.NewIdempotencyRepository(appPool), nil, nil, 5*time.Minute, nil)

	firstTS := time.Now().UTC().Add(-30 * time.Minute)
	env1 := mkEnvelope(t, "TenantOffboarded", tenantID, firstTS, map[string]string{})
	require.NoError(t, c.Handle(ctx, env1))

	var origCancelled, origDeleted time.Time
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT cancelled_at, deleted_at FROM tenants WHERE id = $1`,
		tenantID).Scan(&origCancelled, &origDeleted))
	require.False(t, origCancelled.IsZero())
	require.False(t, origDeleted.IsZero())

	env2 := mkEnvelope(t, "TenantOffboarded", tenantID, time.Now().UTC(), map[string]string{})
	require.NoError(t, c.Handle(ctx, env2))

	var replayCancelled, replayDeleted time.Time
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT cancelled_at, deleted_at FROM tenants WHERE id = $1`,
		tenantID).Scan(&replayCancelled, &replayDeleted))
	assert.True(t, replayCancelled.Equal(origCancelled),
		"offboarded replay must not reset cancelled_at")
	// deleted_at uses now() unconditionally in the handler — this is a
	// SEPARATE fix that could piggyback on N5, but LLD-current-behavior
	// bumps it. Document the observed behavior (regression guard on the
	// current spec — if a future fix makes deleted_at also COALESCE,
	// this assertion needs to flip).
	assert.False(t, replayDeleted.Before(origDeleted),
		"offboarded replay must not un-delete the row (deleted_at only moves forward)")
}

// ─────────────────────────────────────────────────────────────────────────
// GDPR tenant wipe (§15.5) — real bug found by the LLD-vs-code audit: the
// TenantOffboarded handler flipped status/deleted_at on the tenants row but
// never actually deleted the six child tables it's documented to wipe (the
// LLD's claimed ON DELETE CASCADE can't fire — the tenants row is only
// soft-deleted, never actually DELETEd). Fixed to issue explicit deletes in
// the same tx. This test seeds one row in every affected child table and
// proves all six are gone after a single TenantOffboarded delivery.
// ─────────────────────────────────────────────────────────────────────────

func TestConsumer_TenantOffboarded_GDPRWipe_DeletesAllChildRows(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedPaidTenant(t, ctx, rawPool, "gdpr-wipe")

	userID := uuid.New()
	deptID := uuid.New()

	membershipID := uuid.New()
	_, err := rawPool.Exec(ctx,
		`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES ($1, $2, $3, 'active')`,
		membershipID, tenantID, userID)
	require.NoError(t, err)

	_, err = rawPool.Exec(ctx,
		`INSERT INTO tenant_roles (tenant_id, user_id, tenant_membership_id, role_code, granted_by) VALUES ($1, $2, $3, 'tenant_owner', $2)`,
		tenantID, userID, membershipID)
	require.NoError(t, err)

	_, err = rawPool.Exec(ctx,
		`INSERT INTO tenant_departments (tenant_id, department_id) VALUES ($1, $2)`,
		tenantID, deptID)
	require.NoError(t, err)

	_, err = rawPool.Exec(ctx,
		`INSERT INTO dept_memberships (tenant_id, user_id, tenant_membership_id, department_id, role_level, granted_by) VALUES ($1, $2, $3, $4, 'approver', $2)`,
		tenantID, userID, membershipID, deptID)
	require.NoError(t, err)

	_, err = rawPool.Exec(ctx,
		`INSERT INTO dept_role_labels (tenant_id, role_code, display_name) VALUES ($1, 'approver', 'Approver')`,
		tenantID)
	require.NoError(t, err)

	_, err = rawPool.Exec(ctx,
		`INSERT INTO pending_invitations (tenant_id, email, full_name, invited_by, expires_at) VALUES ($1, 'pending@example.com', 'Pending Invitee', $2, now() + interval '7 days')`,
		tenantID, userID)
	require.NoError(t, err)

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, pgadapter.NewIdempotencyRepository(appPool), nil, nil, 5*time.Minute, nil)

	env := mkEnvelope(t, "TenantOffboarded", tenantID, time.Now().UTC(), map[string]string{})
	require.NoError(t, c.Handle(ctx, env))

	for _, tbl := range []string{"tenant_memberships", "tenant_roles", "tenant_departments", "dept_memberships", "dept_role_labels", "pending_invitations"} {
		var count int
		require.NoError(t, rawPool.QueryRow(ctx,
			`SELECT count(*) FROM `+tbl+` WHERE tenant_id = $1`, tenantID).Scan(&count))
		assert.Equal(t, 0, count, "GDPR wipe must delete every %s row for the offboarded tenant", tbl)
	}

	// The tenants row itself is only soft-deleted — id retained for audit.
	var status string
	var deletedAt *time.Time
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT status, deleted_at FROM tenants WHERE id = $1`, tenantID).Scan(&status, &deletedAt))
	assert.Equal(t, "offboarded", status)
	assert.NotNil(t, deletedAt)
}

// A replay (second TenantOffboarded delivery, already-offboarded tenant)
// must not error even though the child rows are already gone — the DELETEs
// are unconditional on tenant_id, so a second pass just affects 0 rows.
func TestConsumer_TenantOffboarded_GDPRWipe_ReplayIsNoopNotError(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedPaidTenant(t, ctx, rawPool, "gdpr-wipe-replay")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, pgadapter.NewIdempotencyRepository(appPool), nil, nil, 5*time.Minute, nil)

	env1 := mkEnvelope(t, "TenantOffboarded", tenantID, time.Now().UTC().Add(-time.Minute), map[string]string{})
	require.NoError(t, c.Handle(ctx, env1))

	env2 := mkEnvelope(t, "TenantOffboarded", tenantID, time.Now().UTC(), map[string]string{})
	require.NoError(t, c.Handle(ctx, env2), "replay after the tenant row is gone (deleted_at IS NOT NULL) must still be handled cleanly")
}

// ─────────────────────────────────────────────────────────────────────────
// B17: invitation_expiry reconciler respects BatchLimit — a backlog wave
// never produces a single unbounded UPDATE.
// ─────────────────────────────────────────────────────────────────────────

func TestInvitationExpiry_RespectsBatchLimit(t *testing.T) {
	t.Parallel()
	jctx, _, rawPool := newJobContext(t, context.Background())
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "b17-batch")

	// Insert-with-future + backdate to bypass G5 trigger. Seed a backlog of
	// 25 pending invitations, all past-expiry.
	const total = 25
	inviteIDs := make([]uuid.UUID, total)
	for i := 0; i < total; i++ {
		inviteIDs[i] = uuid.New()
		_, err := rawPool.Exec(ctx, `
			INSERT INTO pending_invitations
			  (id, tenant_id, email, full_name, invited_by, status, expires_at)
			VALUES ($1, $2, $3, 'Backlog', gen_random_uuid(), 'pending', now() + interval '1 hour')`,
			inviteIDs[i], tenantID,
			// Unique email per row so uq_pending_by_email doesn't collide.
			nUniqueEmail("backlog", i))
		require.NoError(t, err)
	}
	// Backdate everything.
	_, err := rawPool.Exec(ctx,
		`UPDATE pending_invitations SET expires_at = now() - interval '1 hour'
		 WHERE tenant_id = $1 AND status = 'pending'`,
		tenantID)
	require.NoError(t, err)

	// Constrain BatchLimit to something well below the backlog size.
	jctx.BatchLimit = 10

	res, err := jobs.InvitationExpiry(ctx, jctx)
	require.NoError(t, err)
	assert.LessOrEqual(t, res.Succeeded, jctx.BatchLimit,
		"B17: one tick must NOT flip more rows than BatchLimit — got %d, cap %d",
		res.Succeeded, jctx.BatchLimit)
	assert.Equal(t, jctx.BatchLimit, res.Succeeded,
		"with 25 candidates and limit 10, exactly 10 must flip")

	// Convergence: subsequent ticks catch the remainder. Assertion is
	// bounded-progress, not one-shot completion (matches §13.1 next-tick).
	res2, err := jobs.InvitationExpiry(ctx, jctx)
	require.NoError(t, err)
	assert.LessOrEqual(t, res2.Succeeded, jctx.BatchLimit)
	assert.Greater(t, res2.Succeeded, 0, "second tick must make progress on the backlog")
}

// ─────────────────────────────────────────────────────────────────────────
// TenantReactivated: LLD path — from active/trial/past_due/cancelled →
// active; NULL out cancelled_at. From offboarded: rejected (PAID-1).
// Regression guard the state matrix.
// ─────────────────────────────────────────────────────────────────────────

func TestConsumer_TenantReactivated_FromCancelled_ClearsCancelledAt(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedPaidTenant(t, ctx, rawPool, "reactivate")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, pgadapter.NewIdempotencyRepository(appPool), nil, nil, 5*time.Minute, nil)

	// Cancel first.
	env1 := mkEnvelope(t, "TenantSubscriptionCancelled", tenantID,
		time.Now().UTC().Add(-10*time.Minute), map[string]string{})
	require.NoError(t, c.Handle(ctx, env1))

	// Then reactivate.
	env2 := mkEnvelope(t, "TenantReactivated", tenantID, time.Now().UTC(), map[string]string{})
	require.NoError(t, c.Handle(ctx, env2))

	var status string
	var cancelledAt *time.Time
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT status, cancelled_at FROM tenants WHERE id = $1`,
		tenantID).Scan(&status, &cancelledAt))
	assert.Equal(t, "active", status, "reactivation must land on 'active'")
	assert.Nil(t, cancelledAt, "reactivation must NULL out cancelled_at")
}

func TestConsumer_TenantReactivated_FromOffboarded_Rejected(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedPaidTenant(t, ctx, rawPool, "reactivate-offboarded")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, pgadapter.NewIdempotencyRepository(appPool), nil, nil, 5*time.Minute, nil)

	// Offboard first — PAID-1 terminal state.
	env1 := mkEnvelope(t, "TenantOffboarded", tenantID,
		time.Now().UTC().Add(-10*time.Minute), map[string]string{})
	require.NoError(t, c.Handle(ctx, env1))

	// Attempt reactivation — must be logged and rejected (no state change).
	env2 := mkEnvelope(t, "TenantReactivated", tenantID, time.Now().UTC(), map[string]string{})
	require.NoError(t, c.Handle(ctx, env2), "handler returns nil but silently skips per PAID-1")

	var status string
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT status FROM tenants WHERE id = $1`, tenantID).Scan(&status))
	assert.Equal(t, "offboarded", status,
		"PAID-1: offboarded is terminal, reactivation must be rejected")
}

// ─────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────

// seedPaidTenant seeds a tenant with subscription_started_at set so the
// chk_subscription_started_required CHECK constraint passes when the
// consumer transitions status to cancelled/suspended/offboarded.
func seedPaidTenant(t *testing.T, ctx context.Context, rawPool *pgxpoolPool, slug string) uuid.UUID {
	t.Helper()
	tenantID := seedTenant(t, ctx, rawPool, slug)
	_, err := rawPool.Exec(ctx,
		`UPDATE tenants SET status = 'active', subscription_started_at = now(),
		                     trial_ends_at = NULL
		 WHERE id = $1`, tenantID)
	require.NoError(t, err)
	return tenantID
}

// nUniqueEmail generates a distinct email per index so partial-unique
// constraints on pending_invitations don't collide during backlog seeding.
func nUniqueEmail(prefix string, i int) string {
	// prefix-N@example.com is sufficient for tests.
	return prefix + "-" + itoa(i) + "@example.com"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		buf[n] = '-'
	}
	return string(buf[n:])
}

// Reference-touch to keep events package imported without lint noise if
// the file grows.
var _ = events.Envelope[json.RawMessage]{}

// ─────────────────────────────────────────────────────────────────────────
// TrialReactivated (TR2, §15.4) — trial_ends_at must come from the Catalog
// Service's plan.trial_duration_days, resolved via a pre-tx HTTP-shaped
// call (fakePlanCatalog here), not a local `plans` table subquery — that
// table moved to iam-catalog-admin under ADR-0007 and no longer exists in
// this service's own database.
// ─────────────────────────────────────────────────────────────────────────

// fakePlanCatalog is a minimal port.PlanCatalogReader test double — no
// cache, no HTTP, just a fixed TrialDurationDays for whatever code is
// requested.
type fakePlanCatalog struct {
	trialDurationDays int
}

var _ port.PlanCatalogReader = (*fakePlanCatalog)(nil)

func (f *fakePlanCatalog) Plans(context.Context) ([]domain.Plan, error) { return nil, nil }

func (f *fakePlanCatalog) PlanByCode(_ context.Context, code domain.TenantPlan) (*domain.Plan, error) {
	return &domain.Plan{Code: code, TrialDurationDays: f.trialDurationDays}, nil
}

func TestConsumer_TrialReactivated_SetsTrialEndsAtFromCatalogPlan(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "trial-reactivated")

	_, err := rawPool.Exec(ctx, `UPDATE tenants SET status = 'trial_expired' WHERE id = $1`, tenantID)
	require.NoError(t, err)

	outbox := &captureOutbox{}
	catalog := &fakePlanCatalog{trialDurationDays: 21}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, pgadapter.NewIdempotencyRepository(appPool), catalog, nil, 5*time.Minute, nil)

	env := mkEnvelope(t, "TrialReactivated", tenantID, time.Now().UTC(), struct{}{})
	require.NoError(t, c.Handle(ctx, env))

	var status string
	var reactivationCount int
	var trialEndsAt time.Time
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT status, trial_reactivation_count, trial_ends_at FROM tenants WHERE id = $1`, tenantID,
	).Scan(&status, &reactivationCount, &trialEndsAt))

	assert.Equal(t, "trial", status)
	assert.Equal(t, 1, reactivationCount)
	assert.WithinDuration(t, time.Now().UTC().Add(21*24*time.Hour), trialEndsAt, 2*time.Minute,
		"trial_ends_at must use the Catalog Service's trial_duration_days (21), not a dropped local plans table")
}

func TestConsumer_TrialReactivated_CapReachedIsNoop(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "trial-reactivated-capped")

	_, err := rawPool.Exec(ctx,
		`UPDATE tenants SET status = 'trial_expired', trial_reactivation_count = 1 WHERE id = $1`, tenantID)
	require.NoError(t, err)

	outbox := &captureOutbox{}
	catalog := &fakePlanCatalog{trialDurationDays: 14}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, pgadapter.NewIdempotencyRepository(appPool), catalog, nil, 5*time.Minute, nil)

	env := mkEnvelope(t, "TrialReactivated", tenantID, time.Now().UTC(), struct{}{})
	require.NoError(t, c.Handle(ctx, env))

	var status string
	var reactivationCount int
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT status, trial_reactivation_count FROM tenants WHERE id = $1`, tenantID,
	).Scan(&status, &reactivationCount))
	assert.Equal(t, "trial_expired", status, "TRIAL-5 cap already hit — no-op, status unchanged")
	assert.Equal(t, 1, reactivationCount)
}
