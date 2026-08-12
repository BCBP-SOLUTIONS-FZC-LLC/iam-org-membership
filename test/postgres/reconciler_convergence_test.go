//go:build integration

// Reconciler-convergence integration tests. Each test seeds a specific
// out-of-band state, invokes the corresponding job body directly (no
// K8s CronJob wrapping), and asserts the DB converges to the expected
// steady state on the FIRST run — no polling, no next-tick assumptions.
//
// Jobs covered:
//   - InvitationExpiry (PI-5) — pending → expired when past expires_at
//   - DelegationExpiry (§8.7, DEL-6) — active → ended when past ends_at,
//     with UP pointer-clear happening first (mock UP tracks the call)
//   - SeatOverageReconcile (SEAT-5) — overage_since transitions in both
//     directions with outbox events
//   - ProcessedEventsPrune (PE-1) — stale rows deleted, fresh rows kept
package postgres_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/cmd/reconciler/jobs"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── shared mocks ────────────────────────────────────────────────────────

// captureEventPublisher records every event enqueued (for reconciler outbox
// assertions). Enqueue accepts a pgx.Tx but we ignore it here — the caller's
// TxRunner wraps this via port.WithEventPublisher and the tx round-trip is
// verified by other tests.
type captureEventPublisher struct {
	mu     sync.Mutex
	events []*domain.DomainEvent
}

func (p *captureEventPublisher) Enqueue(_ context.Context, _ pgx.Tx, event *domain.DomainEvent) error {
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

// captureUserProfileClient records SetAvailability calls. Configurable to
// return an error so the DEL-6 fail-open path can be exercised.
type captureUserProfileClient struct {
	mu    sync.Mutex
	calls []port.SetAvailabilityRequest
	err   error
}

func (u *captureUserProfileClient) SetAvailability(_ context.Context, req port.SetAvailabilityRequest) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, req)
	return u.err
}

func (u *captureUserProfileClient) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}

// seedDelegationMemberships inserts one active tenant_memberships row per
// user and returns their membership ids so the caller can populate the
// delegator_membership_id / delegate_membership_id composite FK columns
// on the delegations row (fk_del_delegator_membership / _delegate_membership).
func seedDelegationMemberships(t *testing.T, ctx context.Context, rawPool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, tenantID, delegatorID, delegateID uuid.UUID) (uuid.UUID, uuid.UUID) {
	t.Helper()
	var mDelegator, mDelegate uuid.UUID
	err := rawPool.QueryRow(ctx, `
		INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
		VALUES (gen_random_uuid(), $1, $2, 'active')
		RETURNING id`, tenantID, delegatorID).Scan(&mDelegator)
	require.NoError(t, err)
	err = rawPool.QueryRow(ctx, `
		INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
		VALUES (gen_random_uuid(), $1, $2, 'active')
		RETURNING id`, tenantID, delegateID).Scan(&mDelegate)
	require.NoError(t, err)
	return mDelegator, mDelegate
}

func newJobContext(t *testing.T, ctx context.Context) (*jobs.Context, *captureEventPublisher, *captureUserProfileClient) {
	t.Helper()
	appPool, rawPool := setupTestDB(t)
	// Reconcilers use SysPool (BYPASSRLS) for cross-tenant sweeps AND the
	// app pool wrapped in a TxRunner for atomic state+event emission.
	publisher := &captureEventPublisher{}
	up := &captureUserProfileClient{}
	jctx := &jobs.Context{
		Pool:                   appPool,
		SysPool:                rawPool,
		OutboxPublisher:        publisher,
		TxRunner:               postgres.NewTxRunner(appPool, publisher),
		UserProfile:            up,
		Logger:                 slog.Default(),
		BatchLimit:             100,
		ProcessedEventsTTLDays: 8,
	}
	return jctx, publisher, up
}

// ── InvitationExpiry (PI-5) ─────────────────────────────────────────────

// TestReconciler_InvitationExpiry_FlipsPastExpiresAt — a pending invitation
// whose expires_at is in the past becomes 'expired' after one job run.
// A pending invitation that is NOT yet expired stays untouched. Terminal
// invitations (revoked/accepted) are ignored.
func TestReconciler_InvitationExpiry_FlipsPastExpiresAt(t *testing.T) {
	jctx, _, _ := newJobContext(t, context.Background())
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, jctx.SysPool, "recon-invexp")

	pastID := uuid.New()
	freshID := uuid.New()
	revokedID := uuid.New()
	// G5 trigger blocks INSERT with past expires_at, so we insert future
	// then UPDATE the ones we want to appear stale. UPDATE is intentionally
	// NOT gated by the trigger (accept/revoke/expiry flows legitimately
	// transition rows after their expires_at has passed).
	_, err := jctx.SysPool.Exec(ctx, `
		INSERT INTO pending_invitations (id, tenant_id, email, full_name, invited_by, status, expires_at) VALUES
		    ($1, $4, 'past@example.com',    'Past',    gen_random_uuid(), 'pending',  now() + interval '1 hour'),
		    ($2, $4, 'fresh@example.com',   'Fresh',   gen_random_uuid(), 'pending',  now() + interval '1 hour'),
		    ($3, $4, 'revoked@example.com', 'Revoked', gen_random_uuid(), 'revoked',  now() + interval '1 hour')`,
		pastID, freshID, revokedID, tenantA)
	require.NoError(t, err)
	// Backdate the two rows that should appear stale.
	_, err = jctx.SysPool.Exec(ctx,
		`UPDATE pending_invitations SET expires_at = now() - interval '1 hour' WHERE id IN ($1, $2)`,
		pastID, revokedID)
	require.NoError(t, err)

	res, err := jobs.InvitationExpiry(ctx, jctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Succeeded, "exactly one row must transition")
	assert.Equal(t, 0, res.Failed)

	statuses := map[uuid.UUID]string{}
	rows, err := jctx.SysPool.Query(ctx, `SELECT id, status FROM pending_invitations WHERE tenant_id = $1`, tenantA)
	require.NoError(t, err)
	for rows.Next() {
		var id uuid.UUID
		var status string
		require.NoError(t, rows.Scan(&id, &status))
		statuses[id] = status
	}
	rows.Close()
	assert.Equal(t, "expired", statuses[pastID], "past-expiry pending MUST flip to 'expired'")
	assert.Equal(t, "pending", statuses[freshID], "future-expiry pending MUST stay 'pending'")
	assert.Equal(t, "revoked", statuses[revokedID], "terminal invitations MUST NOT be touched")
}

// TestReconciler_InvitationExpiry_Idempotent — re-running the job after
// convergence must be a no-op (nothing to flip).
func TestReconciler_InvitationExpiry_Idempotent(t *testing.T) {
	jctx, _, _ := newJobContext(t, context.Background())
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, jctx.SysPool, "recon-invexp-idemp")

	// Same pattern as above — insert-then-backdate to bypass the G5 trigger
	// while still ending up with a past-expiry pending row.
	inviteID := uuid.New()
	_, err := jctx.SysPool.Exec(ctx, `
		INSERT INTO pending_invitations (id, tenant_id, email, full_name, invited_by, status, expires_at)
		VALUES ($1, $2, 'x@example.com', 'X', gen_random_uuid(), 'pending', now() + interval '1 hour')`,
		inviteID, tenantA)
	require.NoError(t, err)
	_, err = jctx.SysPool.Exec(ctx,
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

// ── DelegationExpiry (§8.7, DEL-6) ──────────────────────────────────────

// TestReconciler_DelegationExpiry_EndsExpiredWithUPPointerClear —
// active delegations past ends_at get their delegator's UP pointer cleared
// (via SetAvailability with ClearDelegate=true) BEFORE the delegations row
// is flipped to 'ended'. Also emits DelegationEnded with reason='expired'.
//
// NOTE: single-tenant test — the ctx carries the target tenant's GUC so
// the RLS-enforced TxRunner in the reconciler can see the row. In
// production the reconciler currently does NOT set app.tenant_id
// per-tenant inside its sweep — see memory:reconciler-rls-wiring-bug.
func TestReconciler_DelegationExpiry_EndsExpiredWithUPPointerClear(t *testing.T) {
	jctx, pub, up := newJobContext(t, context.Background())
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, jctx.SysPool, "recon-delexp")
	ctx = withTenant(ctx, tenantA)

	delegator := uuid.New()
	delegate := uuid.New()
	delegatorMemID, delegateMemID := seedDelegationMemberships(t, ctx, jctx.SysPool, tenantA, delegator, delegate)

	// Seed the delegation with ends_at 1 hour in the past.
	delID := uuid.New()
	_, err := jctx.SysPool.Exec(ctx, `
		INSERT INTO delegations
		    (id, tenant_id, delegator_id, delegate_id,
		     delegator_membership_id, delegate_membership_id,
		     scope, status, starts_at, ends_at)
		VALUES ($1, $2, $3, $4, $5, $6,
		        'all', 'active', now() - interval '2 hours', now() - interval '1 hour')`,
		delID, tenantA, delegator, delegate, delegatorMemID, delegateMemID)
	require.NoError(t, err)

	res, err := jobs.DelegationExpiry(ctx, jctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Succeeded)

	// UP pointer-clear was called for this delegator first.
	require.Equal(t, 1, up.count(), "DEL-6: UP.SetAvailability MUST be called before local flip")
	assert.True(t, up.calls[0].ClearDelegate, "must clear delegate pointer, not toggle availability")
	assert.Equal(t, delegator, up.calls[0].UserID, "must clear the DELEGATOR's pointer")

	// Delegation row must now be 'ended'.
	var status string
	require.NoError(t, jctx.SysPool.QueryRow(ctx,
		`SELECT status FROM delegations WHERE id = $1`, delID).Scan(&status))
	assert.Equal(t, "ended", status)

	// One DelegationEnded event with reason='expired' MUST be enqueued.
	ends := pub.byType(string(domain.EventDelegationEnded))
	require.Len(t, ends, 1, "exactly one DelegationEnded event")
	payload := ends[0].Data.(domain.DelegationEndedPayload)
	assert.Equal(t, domain.EndReasonExpired, payload.EndedReason)
	assert.Equal(t, delID, payload.DelegationID)
}

// TestReconciler_DelegationExpiry_DEL6_DefersOnUPFailure — if UP is down,
// the delegation MUST remain active for the next tick to retry. Nothing
// half-completes. This is the fail-open path (DEL-6).
func TestReconciler_DelegationExpiry_DEL6_DefersOnUPFailure(t *testing.T) {
	jctx, pub, up := newJobContext(t, context.Background())
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, jctx.SysPool, "recon-delexp-fail")
	ctx = withTenant(ctx, tenantA)

	delegator := uuid.New()
	delegate := uuid.New()
	delegatorMemID, delegateMemID := seedDelegationMemberships(t, ctx, jctx.SysPool, tenantA, delegator, delegate)

	delID := uuid.New()
	_, err := jctx.SysPool.Exec(ctx, `
		INSERT INTO delegations
		    (id, tenant_id, delegator_id, delegate_id,
		     delegator_membership_id, delegate_membership_id,
		     scope, status, starts_at, ends_at)
		VALUES ($1, $2, $3, $4, $5, $6,
		        'all', 'active', now() - interval '2 hours', now() - interval '1 hour')`,
		delID, tenantA, delegator, delegate, delegatorMemID, delegateMemID)
	require.NoError(t, err)

	// Force UP failure.
	up.err = errors.New("simulated UP outage")

	res, err := jobs.DelegationExpiry(ctx, jctx)
	require.NoError(t, err, "job MUST return nil error even when per-row failures accumulate")
	assert.Equal(t, 1, res.Attempted)
	assert.Equal(t, 1, res.Failed, "DEL-6: UP failure counts as row-level failure")
	assert.Equal(t, 0, res.Succeeded)

	// Delegation row MUST remain 'active' — next tick retries.
	var status string
	require.NoError(t, jctx.SysPool.QueryRow(ctx,
		`SELECT status FROM delegations WHERE id = $1`, delID).Scan(&status))
	assert.Equal(t, "active", status, "DEL-6 fail-open: delegation must stay active for retry")

	// NO DelegationEnded event should be enqueued.
	assert.Empty(t, pub.byType(string(domain.EventDelegationEnded)),
		"DEL-6: no DelegationEnded event on UP failure")
}

// ── SeatOverageReconcile (SEAT-5) ───────────────────────────────────────

// TestReconciler_SeatOverage_StartsWhenOverCap — tenant is over-cap but
// overage_since is NULL → reconciler sets overage_since=now() and emits
// TenantSeatOverageStarted.
func TestReconciler_SeatOverage_StartsWhenOverCap(t *testing.T) {
	jctx, pub, _ := newJobContext(t, context.Background())
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, jctx.SysPool, "recon-seat-start")
	ctx = withTenant(ctx, tenantA)

	// licensed_seats=1 but 2 active members + 0 pending → over cap.
	_, err := jctx.SysPool.Exec(ctx, `UPDATE tenants SET licensed_seats = 1 WHERE id = $1`, tenantA)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		_, err := jctx.SysPool.Exec(ctx, `
			INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
			VALUES (gen_random_uuid(), $1, gen_random_uuid(), 'active')`, tenantA)
		require.NoError(t, err)
	}

	res, err := jobs.SeatOverageReconcile(ctx, jctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.Succeeded, 1)

	var overageSince *time.Time
	require.NoError(t, jctx.SysPool.QueryRow(ctx,
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
	jctx, pub, _ := newJobContext(t, context.Background())
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, jctx.SysPool, "recon-seat-resolve")
	ctx = withTenant(ctx, tenantA)

	// Pre-set overage_since to simulate a tenant that was previously flagged.
	// Now: 3 licensed seats, 1 active member, 0 pending → under cap.
	past := time.Now().Add(-48 * time.Hour)
	_, err := jctx.SysPool.Exec(ctx,
		`UPDATE tenants SET licensed_seats = 3, overage_since = $2 WHERE id = $1`, tenantA, past)
	require.NoError(t, err)
	_, err = jctx.SysPool.Exec(ctx, `
		INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
		VALUES (gen_random_uuid(), $1, gen_random_uuid(), 'active')`, tenantA)
	require.NoError(t, err)

	res, err := jobs.SeatOverageReconcile(ctx, jctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.Succeeded, 1)

	var overageSince *time.Time
	require.NoError(t, jctx.SysPool.QueryRow(ctx,
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
	jctx, pub, _ := newJobContext(t, context.Background())
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, jctx.SysPool, "recon-seat-noop")
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
	jctx, _, _ := newJobContext(t, context.Background())
	ctx := context.Background()

	// Seed 3 stale (13 days old) and 2 fresh (1 hour old).
	staleIDs := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	freshIDs := []string{uuid.NewString(), uuid.NewString()}
	for _, id := range staleIDs {
		_, err := jctx.SysPool.Exec(ctx, `
			INSERT INTO processed_events (event_id, consumer, processed_at)
			VALUES ($1, 'test-consumer', now() - interval '13 days')`, id)
		require.NoError(t, err)
	}
	for _, id := range freshIDs {
		_, err := jctx.SysPool.Exec(ctx, `
			INSERT INTO processed_events (event_id, consumer, processed_at)
			VALUES ($1, 'test-consumer', now() - interval '1 hour')`, id)
		require.NoError(t, err)
	}

	res, err := jobs.ProcessedEventsPrune(ctx, jctx)
	require.NoError(t, err)
	assert.Equal(t, 3, res.Succeeded, "exactly 3 stale rows must be deleted")

	// Fresh rows still there.
	var remaining int
	require.NoError(t, jctx.SysPool.QueryRow(ctx,
		`SELECT count(*) FROM processed_events WHERE consumer = 'test-consumer'`).Scan(&remaining))
	assert.Equal(t, 2, remaining, "PE-1: rows younger than TTL MUST be retained")
}

// TestReconciler_ProcessedEventsPrune_NoOpOnEmptyTable — a job invocation
// against a table with no stale rows returns 0 and doesn't error.
func TestReconciler_ProcessedEventsPrune_NoOpOnEmptyTable(t *testing.T) {
	jctx, _, _ := newJobContext(t, context.Background())
	ctx := context.Background()

	res, err := jobs.ProcessedEventsPrune(ctx, jctx)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Succeeded, "empty table → no deletions")
}
