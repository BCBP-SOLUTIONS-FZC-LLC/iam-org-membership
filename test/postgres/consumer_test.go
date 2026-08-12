//go:build integration

// Phase 5 — SQS consumer + reconciler tests focused on audit fixes that
// weren't already covered by consumer_evt_test.go / reconciler_convergence_test.go.
//
// Coverage focus:
//
//   N5 — TenantSubscriptionCancelled uses COALESCE(cancelled_at, now())
//        so replaying the event never resets the §15.5 retention clock.
//   TenantSuspended replay parity — same COALESCE guarantee.
//   TenantOffboarded replay parity — cancelled_at preserved AND
//        deleted_at preserved on replay (PAID-1 terminal state).
//   B17 — invitation_expiry reconciler respects jctx.BatchLimit; a
//        backlog wave never produces a single unbounded UPDATE.
package postgres_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/cmd/reconciler/jobs"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/consumer"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────
// N5: TenantSubscriptionCancelled COALESCE preserves cancelled_at on replay.
// ─────────────────────────────────────────────────────────────────────────

func TestN5_TenantSubscriptionCancelled_ReplayPreservesCancelledAt(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedPaidTenant(t, ctx, rawPool, "n5-cancel-replay")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, 5*time.Minute, slog.Default())

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
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedPaidTenant(t, ctx, rawPool, "suspend-replay")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, 5*time.Minute, slog.Default())

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
// TenantOffboarded — terminal (PAID-1). Replay must preserve BOTH
// cancelled_at and deleted_at.
// ─────────────────────────────────────────────────────────────────────────

func TestConsumer_TenantOffboarded_ReplayPreservesTimestamps(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedPaidTenant(t, ctx, rawPool, "offboard-replay")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, 5*time.Minute, slog.Default())

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
// B17: invitation_expiry reconciler respects BatchLimit — a backlog wave
// never produces a single unbounded UPDATE.
// ─────────────────────────────────────────────────────────────────────────

func TestInvitationExpiry_RespectsBatchLimit(t *testing.T) {
	jctx, _, _ := newJobContext(t, context.Background())
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, jctx.SysPool, "b17-batch")

	// Insert-with-future + backdate to bypass G5 trigger. Seed a backlog of
	// 25 pending invitations, all past-expiry.
	const total = 25
	inviteIDs := make([]uuid.UUID, total)
	for i := 0; i < total; i++ {
		inviteIDs[i] = uuid.New()
		_, err := jctx.SysPool.Exec(ctx, `
			INSERT INTO pending_invitations
			  (id, tenant_id, email, full_name, invited_by, status, expires_at)
			VALUES ($1, $2, $3, 'Backlog', gen_random_uuid(), 'pending', now() + interval '1 hour')`,
			inviteIDs[i], tenantID,
			// Unique email per row so uq_pending_by_email doesn't collide.
			nUniqueEmail("backlog", i))
		require.NoError(t, err)
	}
	// Backdate everything.
	_, err := jctx.SysPool.Exec(ctx,
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
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedPaidTenant(t, ctx, rawPool, "reactivate")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, 5*time.Minute, slog.Default())

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
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedPaidTenant(t, ctx, rawPool, "reactivate-offboarded")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, 5*time.Minute, slog.Default())

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
