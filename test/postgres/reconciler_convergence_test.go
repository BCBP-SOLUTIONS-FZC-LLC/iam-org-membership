//go:build integration

// Reconciler-convergence integration tests. Each test seeds a specific
// out-of-band state, invokes the corresponding job body directly (no
// K8s CronJob wrapping), and asserts the DB converges to the expected
// steady state on the FIRST run — no polling, no next-tick assumptions.
//
// Jobs covered:
//   - InvitationExpiry (PI-5) — pending → expired when past expires_at
//   - SeatOverageReconcile (SEAT-5) — overage_since transitions in both
//     directions with outbox events
//   - ProcessedEventsPrune (PE-1) — stale rows deleted, fresh rows kept
//
// DelegationExpiry / DelegationReview (§8.7, DEL-6) moved to the standalone
// iam-delegation service under ADR-0008 v2 (Option C) and are no longer
// exercised here — see iam-delegation's own reconciler job tests.
package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/cmd/reconciler/jobs"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/test/dbseed"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── shared mocks ────────────────────────────────────────────────────────

// captureEventPublisher records every event enqueued (for reconciler outbox
// assertions). TxRunner injects this via port.WithEventPublisher.
type captureEventPublisher struct {
	mu     sync.Mutex
	events []*domain.DomainEvent
}

func (p *captureEventPublisher) Enqueue(_ context.Context, event *domain.DomainEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, event)
	return nil
}

func (p *captureEventPublisher) byType(typ string) []*domain.DomainEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*domain.DomainEvent
	for _, e := range p.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// newJobContext returns a *jobs.Context wired for reconciler tests, plus
// the captured-event publisher and the raw superuser pgxpool.Pool for tests
// that need to seed/assert state directly (rawPool is a real
// *pgcommon.Pool, matching production, and has no .Exec/.Query/.QueryRow of
// its own — those go through pgcommon.RunInTx/WithConn instead).
func newJobContext(t *testing.T, ctx context.Context) (*jobs.Context, *captureEventPublisher, *dbseed.Pool) {
	t.Helper()
	appPool, rawPool, sysPool := setupTestDB(t)
	// Reconcilers use SysPool (BYPASSRLS) for cross-tenant sweeps AND the
	// app pool wrapped in a TxRunner for atomic state+event emission.
	publisher := &captureEventPublisher{}
	jctx := &jobs.Context{
		TxRunner:               postgres.NewTxRunner(appPool, publisher),
		Tenants:                postgres.NewTenantRepository(appPool),
		Invitations:            postgres.NewInvitationRepository(sysPool),
		Reconciler:             postgres.NewReconcilerStore(sysPool),
		BatchLimit:             100,
		ProcessedEventsTTLDays: 8,
	}
	return jctx, publisher, rawPool
}

// ── InvitationExpiry (PI-5) ─────────────────────────────────────────────

// TestReconciler_InvitationExpiry_FlipsPastExpiresAt — a pending invitation
// whose expires_at is in the past becomes 'expired' after one job run.
// A pending invitation that is NOT yet expired stays untouched. Terminal
// invitations (revoked/accepted) are ignored.
func TestReconciler_InvitationExpiry_FlipsPastExpiresAt(t *testing.T) {
	t.Parallel()
	jctx, _, rawPool := newJobContext(t, context.Background())
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "recon-invexp")

	pastID := uuid.New()
	freshID := uuid.New()
	revokedID := uuid.New()
	// G5 trigger blocks INSERT with past expires_at, so we insert future
	// then UPDATE the ones we want to appear stale. UPDATE is intentionally
	// NOT gated by the trigger (accept/revoke/expiry flows legitimately
	// transition rows after their expires_at has passed).
	_, err := rawPool.Exec(ctx, `
		INSERT INTO pending_invitations (id, tenant_id, email, full_name, invited_by, status, expires_at) VALUES
		    ($1, $4, 'past@example.com',    'Past',    gen_random_uuid(), 'pending',  now() + interval '1 hour'),
		    ($2, $4, 'fresh@example.com',   'Fresh',   gen_random_uuid(), 'pending',  now() + interval '1 hour'),
		    ($3, $4, 'revoked@example.com', 'Revoked', gen_random_uuid(), 'revoked',  now() + interval '1 hour')`,
		pastID, freshID, revokedID, tenantA)
	require.NoError(t, err)
	// Backdate the two rows that should appear stale.
	_, err = rawPool.Exec(ctx,
		`UPDATE pending_invitations SET expires_at = now() - interval '1 hour' WHERE id IN ($1, $2)`,
		pastID, revokedID)
	require.NoError(t, err)

	res, err := jobs.InvitationExpiry(ctx, jctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Succeeded, "exactly one row must transition")
	assert.Equal(t, 0, res.Failed)

	statuses := map[uuid.UUID]string{}
	kcPending := map[uuid.UUID]bool{}
	rows, err := rawPool.Query(ctx, `SELECT id, status, kc_cleanup_pending FROM pending_invitations WHERE tenant_id = $1`, tenantA)
	require.NoError(t, err)
	for rows.Next() {
		var id uuid.UUID
		var status string
		var pending bool
		require.NoError(t, rows.Scan(&id, &status, &pending))
		statuses[id] = status
		kcPending[id] = pending
	}
	rows.Close()
	assert.Equal(t, "expired", statuses[pastID], "past-expiry pending MUST flip to 'expired'")
	assert.Equal(t, "pending", statuses[freshID], "future-expiry pending MUST stay 'pending'")
	assert.Equal(t, "revoked", statuses[revokedID], "terminal invitations MUST NOT be touched")
	assert.True(t, kcPending[pastID], "PI-9: expiry must durably schedule KC-user cleanup, not just flip status")
	assert.False(t, kcPending[freshID], "an untouched pending row must not be marked for cleanup")
	assert.False(t, kcPending[revokedID], "expiry must not touch an already-terminal row's cleanup marker")
}

// TestReconciler_InvitationExpiry_Idempotent — re-running the job after
// convergence must be a no-op (nothing to flip).
func TestReconciler_InvitationExpiry_Idempotent(t *testing.T) {
	t.Parallel()
	jctx, _, rawPool := newJobContext(t, context.Background())
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "recon-invexp-idemp")

	// Same pattern as above — insert-then-backdate to bypass the G5 trigger
	// while still ending up with a past-expiry pending row.
	inviteID := uuid.New()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO pending_invitations (id, tenant_id, email, full_name, invited_by, status, expires_at)
		VALUES ($1, $2, 'x@example.com', 'X', gen_random_uuid(), 'pending', now() + interval '1 hour')`,
		inviteID, tenantA)
	require.NoError(t, err)
	_, err = rawPool.Exec(ctx,
		`UPDATE pending_invitations SET expires_at = now() - interval '1 hour' WHERE id = $1`,
		inviteID)
	require.NoError(t, err)

	res1, err := jobs.InvitationExpiry(ctx, jctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res1.Succeeded)

	res2, err := jobs.InvitationExpiry(ctx, jctx)
	require.NoError(t, err)
	assert.Equal(t, 0, res2.Attempted, "second run MUST find no candidates (idempotent)")
}

// ── SeatOverageReconcile (SEAT-5) ───────────────────────────────────────

// TestReconciler_SeatOverage_StartsWhenOverCap — tenant is over-cap but
// overage_since is NULL → reconciler sets overage_since=now() and emits
// TenantSeatOverageStarted.
func TestReconciler_SeatOverage_StartsWhenOverCap(t *testing.T) {
	t.Parallel()
	jctx, pub, rawPool := newJobContext(t, context.Background())
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "recon-seat-start")
	ctx = withTenant(ctx, tenantA)

	// licensed_seats=1 but 2 active members + 0 pending → over cap.
	_, err := rawPool.Exec(ctx, `UPDATE tenants SET licensed_seats = 1 WHERE id = $1`, tenantA)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		_, err := rawPool.Exec(ctx, `
			INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
			VALUES (gen_random_uuid(), $1, gen_random_uuid(), 'active')`, tenantA)
		require.NoError(t, err)
	}

	res, err := jobs.SeatOverageReconcile(ctx, jctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.Succeeded, 1)

	var overageSince *time.Time
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT overage_since FROM tenants WHERE id = $1`, tenantA).Scan(&overageSince))
	require.NotNil(t, overageSince, "SEAT-5: over-cap tenant MUST have overage_since set")

	starts := pub.byType(string(domain.EventTenantSeatOverageStarted))
	require.Len(t, starts, 1)
	payload := starts[0].Data.(domain.TenantSeatOverageStartedPayload)
	assert.Equal(t, tenantA, payload.TenantID)
	assert.Equal(t, 1, payload.LicensedSeats)
	assert.Equal(t, 2, payload.ActiveUsers)
}

// TestReconciler_SeatOverage_ResolvesWhenBackUnderCap — tenant that WAS
// over cap (overage_since set) but now active+pending <= licensed_seats
// gets overage_since cleared and TenantSeatOverageResolved emitted.
func TestReconciler_SeatOverage_ResolvesWhenBackUnderCap(t *testing.T) {
	t.Parallel()
	jctx, pub, rawPool := newJobContext(t, context.Background())
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "recon-seat-resolve")
	ctx = withTenant(ctx, tenantA)

	// Pre-set overage_since to simulate a tenant that was previously flagged.
	// Now: 3 licensed seats, 1 active member, 0 pending → under cap.
	past := time.Now().Add(-48 * time.Hour)
	_, err := rawPool.Exec(ctx,
		`UPDATE tenants SET licensed_seats = 3, overage_since = $2 WHERE id = $1`, tenantA, past)
	require.NoError(t, err)
	_, err = rawPool.Exec(ctx, `
		INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
		VALUES (gen_random_uuid(), $1, gen_random_uuid(), 'active')`, tenantA)
	require.NoError(t, err)

	res, err := jobs.SeatOverageReconcile(ctx, jctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.Succeeded, 1)

	var overageSince *time.Time
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT overage_since FROM tenants WHERE id = $1`, tenantA).Scan(&overageSince))
	assert.Nil(t, overageSince, "SEAT-5: under-cap tenant MUST have overage_since cleared")

	resolves := pub.byType(string(domain.EventTenantSeatOverageResolved))
	require.Len(t, resolves, 1)
	payload := resolves[0].Data.(domain.TenantSeatOverageResolvedPayload)
	assert.Equal(t, tenantA, payload.TenantID)
}

// TestReconciler_SeatOverage_NoOpWhenAlreadyConverged — tenant is under cap
// and overage_since is already NULL → no transition, no event emitted.
func TestReconciler_SeatOverage_NoOpWhenAlreadyConverged(t *testing.T) {
	t.Parallel()
	jctx, pub, rawPool := newJobContext(t, context.Background())
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "recon-seat-noop")
	ctx = withTenant(ctx, tenantA)
	// Fresh tenant: licensed_seats=100 (default), 0 members, 0 pending.

	res, err := jobs.SeatOverageReconcile(ctx, jctx)
	require.NoError(t, err)
	// Attempted counts candidate tenants (this test's + any others seeded
	// by prior tests in the same DB, but each fresh test gets a new
	// container so we're isolated).
	assert.GreaterOrEqual(t, res.Succeeded, 1, "no-op runs still count as succeeded")

	assert.Empty(t, pub.byType(string(domain.EventTenantSeatOverageStarted)),
		"no transition MUST NOT emit a Started event")
	assert.Empty(t, pub.byType(string(domain.EventTenantSeatOverageResolved)),
		"no transition MUST NOT emit a Resolved event")
}

// ── ProcessedEventsPrune (PE-1) ─────────────────────────────────────────

// TestReconciler_ProcessedEventsPrune_DeletesStaleRows — rows older than
// ProcessedEventsTTLDays are deleted; fresh rows are kept.
func TestReconciler_ProcessedEventsPrune_DeletesStaleRows(t *testing.T) {
	t.Parallel()
	jctx, _, rawPool := newJobContext(t, context.Background())
	ctx := context.Background()

	// Seed 3 stale (13 days old) and 2 fresh (1 hour old).
	staleIDs := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	freshIDs := []string{uuid.NewString(), uuid.NewString()}
	for _, id := range staleIDs {
		_, err := rawPool.Exec(ctx, `
			INSERT INTO processed_events (event_id, consumer, processed_at)
			VALUES ($1, 'test-consumer', now() - interval '13 days')`, id)
		require.NoError(t, err)
	}
	for _, id := range freshIDs {
		_, err := rawPool.Exec(ctx, `
			INSERT INTO processed_events (event_id, consumer, processed_at)
			VALUES ($1, 'test-consumer', now() - interval '1 hour')`, id)
		require.NoError(t, err)
	}

	res, err := jobs.ProcessedEventsPrune(ctx, jctx)
	require.NoError(t, err)
	assert.Equal(t, 3, res.Succeeded, "exactly 3 stale rows must be deleted")

	// Fresh rows still there.
	var remaining int
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT count(*) FROM processed_events WHERE consumer = 'test-consumer'`).Scan(&remaining))
	assert.Equal(t, 2, remaining, "PE-1: rows younger than TTL MUST be retained")
}

// TestReconciler_ProcessedEventsPrune_NoOpOnEmptyTable — a job invocation
// against a table with no stale rows returns 0 and doesn't error.
func TestReconciler_ProcessedEventsPrune_NoOpOnEmptyTable(t *testing.T) {
	t.Parallel()
	jctx, _, _ := newJobContext(t, context.Background())
	ctx := context.Background()

	res, err := jobs.ProcessedEventsPrune(ctx, jctx)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Succeeded, "empty table → no deletions")
}

// TestReconciler_ProcessedEventsPrune_BatchLimitCapsOneTick — a backlog
// larger than BatchLimit deletes exactly BatchLimit rows in one call,
// leaving the rest for the next tick. Real gap the LLD-vs-code audit
// found: this delete used to be a single unbounded statement.
func TestReconciler_ProcessedEventsPrune_BatchLimitCapsOneTick(t *testing.T) {
	t.Parallel()
	jctx, _, rawPool := newJobContext(t, context.Background())
	jctx.BatchLimit = 3
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_, err := rawPool.Exec(ctx, `
			INSERT INTO processed_events (event_id, consumer, processed_at)
			VALUES ($1, 'test-consumer-batch', now() - interval '13 days')`, uuid.NewString())
		require.NoError(t, err)
	}

	res, err := jobs.ProcessedEventsPrune(ctx, jctx)
	require.NoError(t, err)
	assert.Equal(t, 3, res.Succeeded, "BatchLimit=3 must cap this tick at exactly 3 deletes")

	var remaining int
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT count(*) FROM processed_events WHERE consumer = 'test-consumer-batch'`).Scan(&remaining))
	assert.Equal(t, 2, remaining, "the other 2 stale rows must survive for the next tick")
}

// ── OutboxPrune ──────────────────────────────────────────────────────────

// TestReconciler_OutboxPrune_DeletesPublishedPastRetention — a published
// row past OutboxRetentionDays is deleted; an unpublished row and a
// recently-published row both survive.
func TestReconciler_OutboxPrune_DeletesPublishedPastRetention(t *testing.T) {
	t.Parallel()
	jctx, _, rawPool := newJobContext(t, context.Background())
	jctx.OutboxRetentionDays = 8
	ctx := context.Background()

	stale := uuid.New()
	fresh := uuid.New()
	unpublished := uuid.New()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO outbox_events (id, event_type, payload, published_at)
		VALUES ($1, 'TestEvent', '{}'::jsonb, now() - interval '10 days')`, stale)
	require.NoError(t, err)
	_, err = rawPool.Exec(ctx, `
		INSERT INTO outbox_events (id, event_type, payload, published_at)
		VALUES ($1, 'TestEvent', '{}'::jsonb, now() - interval '1 hour')`, fresh)
	require.NoError(t, err)
	_, err = rawPool.Exec(ctx, `
		INSERT INTO outbox_events (id, event_type, payload, published_at)
		VALUES ($1, 'TestEvent', '{}'::jsonb, NULL)`, unpublished)
	require.NoError(t, err)

	res, err := jobs.OutboxPrune(ctx, jctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Succeeded, "exactly the stale published row must be deleted")

	var remaining []uuid.UUID
	rows, err := rawPool.Query(ctx, `SELECT id FROM outbox_events WHERE id IN ($1, $2, $3)`, stale, fresh, unpublished)
	require.NoError(t, err)
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		remaining = append(remaining, id)
	}
	rows.Close()
	assert.ElementsMatch(t, []uuid.UUID{fresh, unpublished}, remaining,
		"a fresh-published row and an unpublished row must both survive")
}

// TestReconciler_OutboxPrune_BatchLimitCapsOneTick — same batching
// guarantee as ProcessedEventsPrune, for outbox_events.
func TestReconciler_OutboxPrune_BatchLimitCapsOneTick(t *testing.T) {
	t.Parallel()
	jctx, _, rawPool := newJobContext(t, context.Background())
	jctx.BatchLimit = 2
	jctx.OutboxRetentionDays = 8
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		_, err := rawPool.Exec(ctx, `
			INSERT INTO outbox_events (id, event_type, payload, published_at)
			VALUES ($1, 'TestEvent', '{}'::jsonb, now() - interval '10 days')`, uuid.New())
		require.NoError(t, err)
	}

	res, err := jobs.OutboxPrune(ctx, jctx)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Succeeded, "BatchLimit=2 must cap this tick at exactly 2 deletes")
}
