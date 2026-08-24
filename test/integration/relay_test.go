//go:build integration

// Phase 12 · EVT-16 cross-topic relay, multi-tenant routing, outbox retry.
//
//   - P12-EVT16-001: full wire path for §16 A61 relay — SNS delivers
//     TrialExpired to tenant-orgm-q, the consumer projects it AND enqueues
//     TenantStateChanged to outbox_events, the outbox runner then publishes
//     the relay on iam.membership.events, and a workflow-queue subscriber
//     receives it.
//   - P12-MULTITEN-001: two envelopes with distinct tenant_ids traverse the
//     same pipe. Each lands in the consumer, each writes its own
//     processed_events row, each drives its own tenant projection.
//   - P12-OUTBOX-RETRY-001: publisher returns an error on the first attempt;
//     outbox_events row is retried and eventually delivered — attempts count
//     climbs but the row is not moved to dead_letters until MaxAttempts.
//
// Full test-case metadata (Module · Feature · Priority · Severity) lives in
// Reference_doc/Test_metadata_P12_P16.md.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/consumer"
	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/eventbus"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// ── P12-EVT16-001 ───────────────────────────────────────────────────────────

// TestEVT16_001_TenantStateChangedRelayThroughWire — end-to-end wire
// test for §16 A61 tenant-state relay:
//
//	SNS(iam-tenant-events) → SQS(tenant-orgm-q) → Consumer.Handle
//	    → projection tx (tenants.status = trial_expired)
//	    + outbox_events INSERT (TenantStateChanged)
//	→ Outbox runner drains → SNS(iam-membership-events) → SQS(workflow-q)
//
// Assert the workflow-q ultimately observes the TenantStateChanged relay.
func TestEVT16_001_TenantStateChangedRelayThroughWire(t *testing.T) {
	e := newPhase12Env(t)

	appPool, err := pgcommon.NewPool(e.ctx, pgcommon.Config{
		DSN:           e.dsn,
		PGBouncerMode: false,
	})
	require.NoError(t, err)
	defer appPool.Close()

	tenantID := seedTenantForConsumer(t, e, "evt16-wire-relay")

	// Real outbox publisher — the consumer's EVT-16 relay enqueues into
	// outbox_events, and the outbox runner then publishes to iam-membership.
	rawCodec := eventbusadapter.Codec(eventbusadapter.NoopCodec{})
	codec, err := eventbusadapter.NewValidatingCodec(rawCodec)
	require.NoError(t, err)
	outboxPub := eventbusadapter.New("iam-org-membership-test", codec)

	memConsumer := consumer.NewMembershipEventConsumer(appPool, outboxPub, pgadapter.NewIdempotencyRepository(appPool), 5*time.Minute, nil)

	// Topics + queues.
	tenantTopic := e.createTopic(t, "iam-tenant-events")
	membershipTopic := e.createTopic(t, "iam-membership-events")

	tenantOrgmQ := e.createQueue(t, "tenant-orgm-q", "", 0)
	e.subscribeQueue(t, tenantTopic, tenantOrgmQ, "")

	workflowQ := e.createQueue(t, "membership-workflow-q", "", 0)
	e.subscribeQueue(t, membershipTopic, workflowQ,
		`{"EventType":["TenantStateChanged","MembershipRevoked"]}`)

	// Publishers for the outbox runner (only membership lane matters — the
	// relay we care about is TenantStateChanged, which routes there).
	membershipPub, err := events.NewSNSPublisher(events.SNSConfig{
		TopicARN: membershipTopic, Region: localstackRegion, EndpointURL: e.endpoint,
	})
	require.NoError(t, err)
	tenantPub, err := events.NewSNSPublisher(events.SNSConfig{
		TopicARN: tenantTopic, Region: localstackRegion, EndpointURL: e.endpoint,
	})
	require.NoError(t, err)
	rp := eventbusadapter.NewRoutingPublisher(membershipPub, tenantPub)

	stopRunner := startOutboxRunner(t, e, rp)
	defer stopRunner()

	stopCons := startConsumer(t, e, tenantOrgmQ, memConsumer.Handle)
	defer stopCons()

	// Kick off the flow by publishing TrialExpired to iam.tenant.events.
	trigger := events.Envelope[json.RawMessage]{
		ID:        uuid.NewString(),
		Type:      "TrialExpired",
		Source:    "iam.tenant.events",
		TenantID:  tenantID.String(),
		Subject:   tenantID.String(),
		Timestamp: time.Now().UTC().Add(-30 * time.Second),
		Payload:   json.RawMessage(`{}`),
	}
	body, err := json.Marshal(trigger)
	require.NoError(t, err)
	e.publishEnvelope(t, tenantTopic, trigger.Type, body, nil)

	// Wait for projection to apply.
	require.Eventually(t, func() bool {
		var status string
		_ = e.rawPool.QueryRow(e.ctx,
			`SELECT status FROM tenants WHERE id = $1`, tenantID).Scan(&status)
		return status == "trial_expired"
	}, 20*time.Second, 200*time.Millisecond,
		"P12-EVT16-001: consumer must project TrialExpired → status=trial_expired")

	// Wait for the relay to reach the workflow queue.
	relayed := e.receiveMessages(t, workflowQ, 1, 20*time.Second)
	require.Len(t, relayed, 1, "P12-EVT16-001: workflow queue must observe the TenantStateChanged relay")

	var wire events.Envelope[json.RawMessage]
	require.NoError(t, json.Unmarshal([]byte(*relayed[0].Body), &wire))
	assert.Equal(t, domain.EventTenantStateChanged, wire.Type,
		"P12-EVT16-001: relay envelope must be TenantStateChanged")
	assert.Equal(t, tenantID.String(), wire.TenantID,
		"P12-EVT16-001: relay must carry the source tenant_id")

	var payload map[string]any
	require.NoError(t, json.Unmarshal(wire.Payload, &payload))
	assert.Equal(t, "trial_expired", payload["status"],
		"P12-EVT16-001: relay payload.status must equal post-projection status")
	assert.Equal(t, "trial", payload["previous_status"],
		"P12-EVT16-001: relay payload.previous_status must be pre-projection value")
	assert.Equal(t, "TrialExpired", payload["cause"],
		"P12-EVT16-001: relay payload.cause must be the source event type")
}

// ── P12-MULTITEN-001 ────────────────────────────────────────────────────────

// TestTwoTenantsIndependentProjection — publish two events
// with distinct tenant_ids on the same queue. Both must project independently
// and each must have its own processed_events row.
func TestTwoTenantsIndependentProjection(t *testing.T) {
	e := newPhase12Env(t)

	appPool, err := pgcommon.NewPool(e.ctx, pgcommon.Config{
		DSN:           e.dsn,
		PGBouncerMode: false,
	})
	require.NoError(t, err)
	defer appPool.Close()

	tenantA := seedTenantForConsumer(t, e, "multi-tenant-a")
	tenantB := seedTenantForConsumer(t, e, "multi-tenant-b")

	outbox := &noopOutbox{}
	memConsumer := consumer.NewMembershipEventConsumer(appPool, outbox, pgadapter.NewIdempotencyRepository(appPool), 5*time.Minute, nil)

	topic := e.createTopic(t, "iam-tenant-events")
	q := e.createQueue(t, "tenant-orgm-q", "", 0)
	e.subscribeQueue(t, topic, q, "")

	stop := startConsumer(t, e, q, memConsumer.Handle)
	defer stop()

	publishTrial := func(id uuid.UUID) string {
		env := events.Envelope[json.RawMessage]{
			ID:        uuid.NewString(),
			Type:      "TrialExpired",
			Source:    "iam.tenant.events",
			TenantID:  id.String(),
			Subject:   id.String(),
			Timestamp: time.Now().UTC().Add(-30 * time.Second),
			Payload:   json.RawMessage(`{}`),
		}
		body, err := json.Marshal(env)
		require.NoError(t, err)
		e.publishEnvelope(t, topic, env.Type, body, nil)
		return env.ID
	}
	idA := publishTrial(tenantA)
	idB := publishTrial(tenantB)

	require.Eventually(t, func() bool {
		var n int
		_ = e.rawPool.QueryRow(e.ctx,
			`SELECT count(*) FROM processed_events WHERE event_id IN ($1,$2)`, idA, idB).Scan(&n)
		return n == 2
	}, 20*time.Second, 250*time.Millisecond,
		"P12-MULTITEN-001: both events must land in processed_events independently")

	var statusA, statusB string
	require.NoError(t, e.rawPool.QueryRow(e.ctx,
		`SELECT status FROM tenants WHERE id = $1`, tenantA).Scan(&statusA))
	require.NoError(t, e.rawPool.QueryRow(e.ctx,
		`SELECT status FROM tenants WHERE id = $1`, tenantB).Scan(&statusB))
	assert.Equal(t, "trial_expired", statusA, "tenant A must project independently")
	assert.Equal(t, "trial_expired", statusB, "tenant B must project independently")
}

// ── P12-OUTBOX-RETRY-001 ────────────────────────────────────────────────────

// TestOUTBOX_RETRY_001_TransientPublisherFailureRetried — a Publisher
// that fails the first N attempts must NOT poison the outbox row. The
// outbox runner increments attempts and retries; on eventual success the
// row is marked published (removed from outbox_events).
func TestOUTBOX_RETRY_001_TransientPublisherFailureRetried(t *testing.T) {
	e := newPhase12Env(t)

	membershipTopic := e.createTopic(t, "iam-membership-events")
	auditQ := e.createQueue(t, "membership-audit-q", "", 0)
	e.subscribeQueue(t, membershipTopic, auditQ, "")

	// Real SNS membership publisher — but wrapped so the first 2 calls fail.
	realMembership, err := events.NewSNSPublisher(events.SNSConfig{
		TopicARN: membershipTopic, Region: localstackRegion, EndpointURL: e.endpoint,
	})
	require.NoError(t, err)
	flaky := &flakyPublisher{inner: realMembership, failFirstN: 2}
	rp := eventbusadapter.NewRoutingPublisher(flaky, nil)

	stopRunner := startOutboxRunner(t, e, rp)
	defer stopRunner()

	tenantID := uuid.New()
	enqueueDomainEvent(t, e, &domain.DomainEvent{
		Type:       domain.EventMembershipRevoked,
		TenantID:   tenantID,
		Subject:    uuid.NewString(),
		Actor:      uuid.NewString(),
		OccurredAt: time.Now().UTC(),
		Data: domain.MembershipRevokedPayload{
			TenantID: tenantID,
			UserID:   uuid.New(),
			ActorID:  uuid.New(),
		},
	})

	msgs := e.receiveMessages(t, auditQ, 1, 30*time.Second)
	require.Len(t, msgs, 1,
		"P12-OUTBOX-RETRY-001: transient failure must eventually succeed within MaxAttempts")

	// At least 3 publish calls: 2 failures + 1 success (may be more depending
	// on runner batch scheduling — that's fine, we only care about eventual
	// success).
	assert.GreaterOrEqual(t, int(flaky.calls.Load()), 3,
		"P12-OUTBOX-RETRY-001: publisher must have been retried after transient failure")

	// The outbox_events row must be marked published_at IS NOT NULL — the
	// platform-events runner marks successful rows rather than deleting them
	// (deletion is a separate PrunePublished job, not part of the runner
	// success path).
	require.Eventually(t, func() bool {
		var pending int
		_ = e.rawPool.QueryRow(e.ctx,
			`SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&pending)
		return pending == 0
	}, 15*time.Second, 250*time.Millisecond,
		"P12-OUTBOX-RETRY-001: outbox_events row must be marked published after successful publish")

	// No dead-letter row — the failures were transient, MaxAttempts was 5.
	var dead int
	require.NoError(t, e.rawPool.QueryRow(e.ctx, `SELECT count(*) FROM outbox_dead_letters`).Scan(&dead))
	assert.Zero(t, dead,
		"P12-OUTBOX-RETRY-001: transient failure must NOT move row to dead-letter table")

	// The attempts counter must reflect the transient failures.
	var attempts int
	require.NoError(t, e.rawPool.QueryRow(e.ctx,
		`SELECT max(attempts) FROM outbox_events WHERE published_at IS NOT NULL`).Scan(&attempts))
	assert.GreaterOrEqual(t, attempts, 1,
		"P12-OUTBOX-RETRY-001: attempts column must record the retry count")
}

// flakyPublisher wraps a real Publisher and returns an error on the first N
// Publish calls, then passes through. Used by the retry test.
type flakyPublisher struct {
	inner      events.Publisher
	failFirstN int32
	calls      atomic.Int32
}

func (f *flakyPublisher) Publish(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	n := f.calls.Add(1)
	if n <= int32(f.failFirstN) {
		return errors.New("simulated transient SNS failure")
	}
	return f.inner.Publish(ctx, env)
}

func (f *flakyPublisher) PublishBatch(ctx context.Context, envs []events.Envelope[json.RawMessage]) error {
	n := f.calls.Add(int32(len(envs)))
	if n <= int32(f.failFirstN) {
		return errors.New("simulated transient SNS batch failure")
	}
	return f.inner.PublishBatch(ctx, envs)
}

// Silence any stray unused var warning if a fixture is trimmed later.
var _ outbox.Config
