//go:build integration

// EVT-14/15/16 integration tests via direct MembershipEventConsumer invocation.
//
//   - EVT-14 recency guard — events with event.time <= last_event_at skip the
//     projection but STILL record processed_events (dedup) (§16 A33).
//   - EVT-15 future-time clamp — events with event.time > now() + skew return
//     ErrPoisonPill and do NOT record processed_events (poison-pill guard,
//     §16 A40).
//   - EVT-16 tenant-state relay — when a projected event actually changes
//     tenants.status or tenants.plan, a TenantStateChanged event is enqueued
//     on the same tx as the projection (§16 A61).
//
// The consumer is exercised directly (no SQS runner) with a mock
// OutboxEnqueuer that captures relayed events for assertion.
package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/consumer"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureOutbox is a mock OutboxEnqueuer that records every relayed event.
// The tx argument is ignored — for these tests we only verify that a relay
// was attempted, and any real INSERT into event_outbox happens via
// platform-events at runtime (outside our migration set).
type captureOutbox struct {
	mu     sync.Mutex
	events []*domain.DomainEvent
}

func (o *captureOutbox) EnqueueInTx(_ context.Context, _ pgx.Tx, event *domain.DomainEvent) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, event)
	return nil
}

func (o *captureOutbox) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.events)
}

func (o *captureOutbox) last() *domain.DomainEvent {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.events) == 0 {
		return nil
	}
	return o.events[len(o.events)-1]
}

// mkEnvelope builds a minimally-populated Envelope[json.RawMessage] whose
// payload is the given typed value marshaled to JSON.
func mkEnvelope(t *testing.T, eventType string, tenantID uuid.UUID, ts time.Time, payload any) events.Envelope[json.RawMessage] {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return events.Envelope[json.RawMessage]{
		ID:        uuid.NewString(),
		Type:      eventType,
		Source:    "iam.tenant.events",
		TenantID:  tenantID.String(),
		Subject:   tenantID.String(),
		Timestamp: ts,
		Payload:   raw,
	}
}

// ── EVT-15: future-time clamp ────────────────────────────────────────────

// TestConsumerEVT15_FutureTimeClamp — event.time > now() + skew → returns
// ErrPoisonPill, NO processed_events row is written (the DLQ path skips
// dedup so the poison message can be re-delivered from DLQ investigation).
func TestConsumerEVT15_FutureTimeClamp(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "evt15-clamp")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, 5*time.Minute, slog.Default())

	future := time.Now().UTC().Add(10 * time.Minute) // > 5min skew
	env := mkEnvelope(t, "TrialExpired", tenantA, future, map[string]string{})

	err := c.Handle(ctx, env)
	require.Error(t, err)
	assert.True(t, errors.Is(err, consumer.ErrPoisonPill),
		"EVT-15: future-time event must return ErrPoisonPill (got %v)", err)

	// tenants.status must NOT have flipped — projection is bypassed entirely.
	var status string
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT status FROM tenants WHERE id = $1`, tenantA).Scan(&status))
	assert.Equal(t, "trial", status, "EVT-15: status must not change on future-time reject")

	// processed_events must NOT be recorded (DLQ path skips dedup).
	var seen int
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT count(*) FROM processed_events WHERE event_id = $1`, env.ID).Scan(&seen))
	assert.Equal(t, 0, seen, "EVT-15: processed_events must NOT be inserted for poison-pill")

	assert.Equal(t, 0, outbox.count(), "EVT-15: no relay when event rejected")
}

// TestConsumerEVT15_WithinSkewApplied — event.time = now() + skew - 1s
// (just inside the window) → projection runs normally.
func TestConsumerEVT15_WithinSkewApplied(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "evt15-boundary")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, 5*time.Minute, slog.Default())

	// 4m59s in the future — inside the 5-min skew window.
	ts := time.Now().UTC().Add(5*time.Minute - time.Second)
	env := mkEnvelope(t, "TrialExpired", tenantA, ts, map[string]string{})

	require.NoError(t, c.Handle(ctx, env), "EVT-15: within skew must not trip clamp")

	var status string
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT status FROM tenants WHERE id = $1`, tenantA).Scan(&status))
	assert.Equal(t, "trial_expired", status, "within-skew projection must apply")
}

// ── EVT-14: recency guard ────────────────────────────────────────────────

// TestConsumerEVT14_FirstEventApplied — last_event_at IS NULL initially →
// first event's projection applies and last_event_at is stamped.
func TestConsumerEVT14_FirstEventApplied(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "evt14-first")

	// Fresh tenant: last_event_at IS NULL.
	var pre *time.Time
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT last_event_at FROM tenants WHERE id = $1`, tenantA).Scan(&pre))
	require.Nil(t, pre, "precondition: last_event_at must start NULL")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, 5*time.Minute, slog.Default())

	ts := time.Now().UTC().Add(-1 * time.Minute)
	env := mkEnvelope(t, "TrialExpired", tenantA, ts, map[string]string{})
	require.NoError(t, c.Handle(ctx, env))

	var status string
	var post *time.Time
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT status, last_event_at FROM tenants WHERE id = $1`, tenantA).Scan(&status, &post))
	assert.Equal(t, "trial_expired", status)
	require.NotNil(t, post, "last_event_at must be stamped after first event")
	assert.WithinDuration(t, ts, *post, time.Millisecond)
}

// TestConsumerEVT14_StaleEventSkipped — event.time <= tenants.last_event_at
// → projection is BYPASSED (status stays put) but processed_events IS
// inserted so SQS never redelivers it.
func TestConsumerEVT14_StaleEventSkipped(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "evt14-stale")

	// Prime last_event_at only — keep status='trial'. The stale event we
	// send below would flip status to 'trial_expired' if the guard misfires,
	// so trial→trial_expired divergence is the observable proof.
	high := time.Now().UTC().Add(-30 * time.Second)
	_, err := rawPool.Exec(ctx,
		`UPDATE tenants SET last_event_at = $2 WHERE id = $1`, tenantA, high)
	require.NoError(t, err)

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, 5*time.Minute, slog.Default())

	// Stale event: timestamp BEFORE last_event_at.
	staleTS := high.Add(-1 * time.Minute)
	env := mkEnvelope(t, "TrialExpired", tenantA, staleTS, map[string]string{})
	require.NoError(t, c.Handle(ctx, env), "stale event must silently succeed (record + skip)")

	// Status must remain 'trial' — projection skipped.
	var status string
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT status FROM tenants WHERE id = $1`, tenantA).Scan(&status))
	assert.Equal(t, "trial", status, "EVT-14: stale event MUST NOT flip status")

	// processed_events MUST hold this event_id so redelivery is a no-op.
	var seen int
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT count(*) FROM processed_events WHERE event_id = $1`, env.ID).Scan(&seen))
	assert.Equal(t, 1, seen, "EVT-14: stale event MUST still record processed_events (dedup)")

	// last_event_at must NOT regress.
	var post *time.Time
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT last_event_at FROM tenants WHERE id = $1`, tenantA).Scan(&post))
	require.NotNil(t, post)
	assert.True(t, !post.Before(high), "EVT-14: last_event_at MUST NOT regress on stale event")

	// No EVT-16 relay on a skipped projection.
	assert.Equal(t, 0, outbox.count(), "EVT-14: no relay when projection is skipped")
}

// TestConsumerEVT14_EqualTimeSkipped — edge case: event.time == last_event_at.
// The guard is `!env.Timestamp.After(*lastEventAt)`, which treats equal as
// stale (defensive against clock-tie duplicate emissions).
func TestConsumerEVT14_EqualTimeSkipped(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "evt14-equal")

	// Postgres stores timestamptz at microsecond precision; round the Go
	// time.Time to match so the equality assertion is stable.
	high := time.Now().UTC().Add(-30 * time.Second).Round(time.Microsecond)
	_, err := rawPool.Exec(ctx,
		`UPDATE tenants SET last_event_at = $2 WHERE id = $1`, tenantA, high)
	require.NoError(t, err)

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, 5*time.Minute, slog.Default())

	env := mkEnvelope(t, "TrialExpired", tenantA, high, map[string]string{})
	require.NoError(t, c.Handle(ctx, env))

	var status string
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT status FROM tenants WHERE id = $1`, tenantA).Scan(&status))
	assert.Equal(t, "trial", status, "EVT-14: equal-time event MUST NOT project (defensive tie-break)")
}

// ── EVT-16: tenant-state relay ───────────────────────────────────────────

// TestConsumerEVT16_StatusChangeEnqueuesRelay — a TenantSuspended that flips
// status trial→suspended must enqueue a TenantStateChanged relay.
func TestConsumerEVT16_StatusChangeEnqueuesRelay(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "evt16-status")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, 5*time.Minute, slog.Default())

	ts := time.Now().UTC().Add(-10 * time.Second)
	env := mkEnvelope(t, "TrialExpired", tenantA, ts, map[string]string{})
	require.NoError(t, c.Handle(ctx, env))

	require.Equal(t, 1, outbox.count(), "EVT-16: state change must enqueue exactly one relay")
	relay := outbox.last()
	require.NotNil(t, relay)
	assert.Equal(t, domain.EventTenantStateChanged, relay.Type)
	payload, ok := relay.Data.(domain.TenantStateChangedPayload)
	require.True(t, ok, "relay payload must be TenantStateChangedPayload")
	assert.Equal(t, domain.StatusTrialExpired, payload.Status)
	assert.Equal(t, domain.StatusTrial, payload.PreviousStatus, "EVT-16: PreviousStatus must be the pre-projection value")
	assert.Equal(t, "TrialExpired", payload.Cause, "EVT-16: Cause must carry the source event type")
}

// TestConsumerEVT16_NoRelayOnNoStateChange — TenantSeatsChanged does NOT
// touch status or plan → no relay enqueued.
func TestConsumerEVT16_NoRelayOnNoStateChange(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "evt16-noop")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, 5*time.Minute, slog.Default())

	ts := time.Now().UTC().Add(-10 * time.Second)
	env := mkEnvelope(t, "TenantSeatsChanged", tenantA, ts, map[string]int{"licensed_seats": 50})
	require.NoError(t, c.Handle(ctx, env))

	var seats int
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT licensed_seats FROM tenants WHERE id = $1`, tenantA).Scan(&seats))
	assert.Equal(t, 50, seats, "SeatsChanged must apply")
	assert.Equal(t, 0, outbox.count(), "EVT-16: seats change without status/plan change must NOT relay")
}

// TestConsumerEVT16_PlanChangeEnqueuesRelay — TenantPlanChanged that flips
// plan starter→enterprise must enqueue a TenantStateChanged with previous_plan.
func TestConsumerEVT16_PlanChangeEnqueuesRelay(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "evt16-plan")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, 5*time.Minute, slog.Default())

	ts := time.Now().UTC().Add(-5 * time.Second)
	env := mkEnvelope(t, "TenantPlanChanged", tenantA, ts, map[string]string{"plan": "enterprise"})
	require.NoError(t, c.Handle(ctx, env))

	var plan string
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT plan FROM tenants WHERE id = $1`, tenantA).Scan(&plan))
	assert.Equal(t, "enterprise", plan)

	require.Equal(t, 1, outbox.count(), "EVT-16: plan change must enqueue exactly one relay")
	relay := outbox.last()
	payload := relay.Data.(domain.TenantStateChangedPayload)
	assert.Equal(t, domain.TenantPlan("enterprise"), payload.Plan)
	assert.Equal(t, domain.TenantPlan("starter"), payload.PreviousPlan, "EVT-16: PreviousPlan must be pre-projection value")
}

// ── Idempotency: (event_id, consumer) short-circuit ──────────────────────

// TestConsumerIdempotency — same event delivered twice is a no-op the
// second time. Projection runs once; status doesn't get double-applied.
func TestConsumerIdempotency(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "idemp-dup")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, 5*time.Minute, slog.Default())

	ts := time.Now().UTC().Add(-10 * time.Second)
	env := mkEnvelope(t, "TrialExpired", tenantA, ts, map[string]string{})

	require.NoError(t, c.Handle(ctx, env), "first delivery must succeed")
	require.NoError(t, c.Handle(ctx, env), "second delivery must also succeed (idempotent)")

	var seen int
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT count(*) FROM processed_events WHERE event_id = $1`, env.ID).Scan(&seen))
	assert.Equal(t, 1, seen, "processed_events must hold exactly one row for duplicate deliveries")

	// Only ONE relay enqueued — the second delivery short-circuits before
	// the projection tx and therefore before the outbox enqueue.
	assert.Equal(t, 1, outbox.count(), "duplicate delivery must NOT re-enqueue relay")
}

// TestConsumerUnknownEventSilentAck — an event type this consumer doesn't
// know about is silently ack'd + recorded in processed_events (forward-compat).
func TestConsumerUnknownEventSilentAck(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "unknown-ack")

	outbox := &captureOutbox{}
	c := consumer.NewMembershipEventConsumer(appPool, outbox, 5*time.Minute, slog.Default())

	ts := time.Now().UTC().Add(-1 * time.Second)
	env := mkEnvelope(t, "FutureEventTypeThatWeShouldHandleSomeday", tenantA, ts, map[string]any{"anything": "goes"})

	require.NoError(t, c.Handle(ctx, env), "unknown event must NOT error")

	// processed_events recorded → prevents SQS redelivery.
	var seen int
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT count(*) FROM processed_events WHERE event_id = $1`, env.ID).Scan(&seen))
	assert.Equal(t, 1, seen, "unknown event MUST record processed_events (forward-compat, no DLQ storm)")

	// Tenant state must not have changed.
	var status string
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT status FROM tenants WHERE id = $1`, tenantA).Scan(&status))
	assert.Equal(t, "trial", status)

	assert.Equal(t, 0, outbox.count(), "unknown event MUST NOT enqueue relay")
}

