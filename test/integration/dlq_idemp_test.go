//go:build integration

// Phase 12 · DLQ + idempotency tests.
//
//   - P12-DLQ-001: RedrivePolicy(maxReceiveCount=5) — after 5 handler
//     failures the 6th delivery must be routed to the DLQ.
//   - P12-IDEMP-001: same envelope delivered twice via SQS lands in
//     processed_events exactly once (IDEMP-4); the second Handle call is
//     a no-op that does NOT re-project state.
//   - P12-IDEMP-002: the processed_events row is keyed by (event_id,
//     consumer) — a second CONSUMER seeing the same envelope IS free to
//     project independently (PE-1 semantics).
//
// Full test-case metadata (Module · Feature · Priority · Severity) lives in
// Reference_doc/Test_metadata_P12_P16.md.
package integration_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/consumer"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// noopOutbox implements port.EventPublisher with a mutex-guarded slice —
// enough for wire-integration assertions where projection is the observable.
type noopOutbox struct {
	mu     sync.Mutex
	events []*domain.DomainEvent
}

func (o *noopOutbox) Enqueue(_ context.Context, e *domain.DomainEvent) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, e)
	return nil
}

func (o *noopOutbox) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.events)
}

// startConsumer wires a real events.SQSConsumer against the given queue,
// dispatching to the handler. Returns a stop func.
func startConsumer(t *testing.T, e *phase12Env, queueURL string, handler events.Handler, opts ...events.ConsumerOption) func() {
	t.Helper()
	cons, err := events.NewSQSConsumerWithClient(
		events.SQSConfig{
			QueueURL:    queueURL,
			Region:      flociRegion,
			EndpointURL: e.endpoint,
			WaitSeconds: 1,
			MaxMessages: 5,
		},
		e.sqsCli,
		handler,
		opts...,
	)
	require.NoError(t, err)
	runCtx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cons.Start(runCtx)
	}()
	return func() {
		cancel()
		<-done
		_ = cons.Stop()
	}
}

// seedTenantForConsumer inserts a minimal tenants row so EVT-14/relay logic
// has something to update. Bypasses RLS via the superuser pool.
func seedTenantForConsumer(t *testing.T, e *phase12Env, slug string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := e.rawPool.Exec(e.ctx, `
		INSERT INTO tenants (id, slug, name, plan, status, trial_ends_at)
		VALUES ($1, $2, $3, 'starter', 'trial', now() + interval '30 days')`,
		id, slug, slug)
	require.NoError(t, err)
	return id
}

// ── P12-DLQ-001 ─────────────────────────────────────────────────────────────

// TestMaxReceiveCountRoutesToDLQ — publish one envelope on a
// queue whose RedrivePolicy limits redelivery to 5. A handler that always
// returns an error must see it 5 times; on the 6th receive attempt SQS moves
// it to the DLQ. Assert by draining the DLQ and confirming the envelope ID.
func TestMaxReceiveCountRoutesToDLQ(t *testing.T) {
	e := newPhase12Env(t)

	topic := e.createTopic(t, "iam-membership-events")
	dlq := e.createQueue(t, "phase12-dlq", "", 0)
	dlqArn := e.queueArn(t, dlq)
	main := e.createQueue(t, "phase12-main", dlqArn, 5)
	e.subscribeQueue(t, topic, main, "")

	// Set a short VisibilityTimeout on the main queue so failed messages
	// become visible again quickly. Default is 30s which would stretch the
	// test to ~150s for 5 retries.
	_, err := e.sqsCli.SetQueueAttributes(e.ctx, &sqs.SetQueueAttributesInput{
		QueueUrl:   &main,
		Attributes: map[string]string{"VisibilityTimeout": "3"},
	})
	require.NoError(t, err)

	var attempts atomic.Int32
	handler := func(_ context.Context, _ events.Envelope[json.RawMessage]) error {
		attempts.Add(1)
		return assertErr("simulated handler failure")
	}
	stop := startConsumer(t, e, main, handler,
		events.WithVisibilityTimeout(3*time.Second),
	)
	defer stop()

	// Publish a proper envelope (with Type set) so the DLQ payload parses
	// cleanly — the assertion downstream checks wire.Type.
	envelope := events.Envelope[json.RawMessage]{
		ID:        uuid.NewString(),
		Type:      domain.EventMembershipRevoked,
		Source:    "iam.membership.events",
		TenantID:  uuid.NewString(),
		Subject:   uuid.NewString(),
		Timestamp: time.Now().UTC(),
		Payload:   json.RawMessage(`{"delegator":"` + uuid.NewString() + `"}`),
	}
	body, err := json.Marshal(envelope)
	require.NoError(t, err)
	e.publishEnvelope(t, topic, envelope.Type, body, nil)

	// Wait until at least 5 attempts landed (redelivery driven by VisibilityTimeout).
	require.Eventually(t, func() bool { return attempts.Load() >= 5 },
		45*time.Second, 250*time.Millisecond,
		"P12-DLQ-001: handler must be invoked at least 5 times (got %d)", attempts.Load())

	// Give SQS a moment to move the message from main → DLQ after the 5th failure.
	dlqMsgs := e.receiveMessages(t, dlq, 1, 30*time.Second)
	require.Len(t, dlqMsgs, 1, "P12-DLQ-001: message must land in DLQ after maxReceiveCount=5")

	var wire events.Envelope[json.RawMessage]
	require.NoError(t, json.Unmarshal([]byte(*dlqMsgs[0].Body), &wire))
	assert.Equal(t, domain.EventMembershipRevoked, wire.Type,
		"P12-DLQ-001: DLQ payload must be the failed envelope")
}

// ── P12-IDEMP-001 ───────────────────────────────────────────────────────────

// TestProcessedEventsDedupOnRedelivery — send the SAME
// envelope twice through the pipe (simulating an SQS at-least-once redeliver
// after visibility timeout). The consumer's processed_events insert must
// keep the second delivery from re-projecting state.
func TestProcessedEventsDedupOnRedelivery(t *testing.T) {
	e := newPhase12Env(t)

	// Build a real pgcommon.Pool so the consumer's tx dance works.
	appPool, err := pgcommon.NewPool(e.ctx, pgcommon.Config{
		DSN:           e.dsn,
		PGBouncerMode: false,
	})
	require.NoError(t, err)
	defer appPool.Close()

	tenantID := seedTenantForConsumer(t, e, "idemp-dup-wire")

	outbox := &noopOutbox{}
	memConsumer := consumer.NewMembershipEventConsumer(pgadapter.NewTxRunner(appPool, outbox), pgadapter.NewTenantRepository(appPool), pgadapter.NewIdempotencyRepository(appPool), nil, nil, 5*time.Minute, nil)

	topic := e.createTopic(t, "iam-tenant-events")
	q := e.createQueue(t, "tenant-orgm-q", "", 0)
	e.subscribeQueue(t, topic, q, "")

	stop := startConsumer(t, e, q, memConsumer.Handle)
	defer stop()

	// Build one envelope, publish it TWICE by re-sending the same ID.
	envelope := events.Envelope[json.RawMessage]{
		ID:        uuid.NewString(),
		Type:      "TrialExpired",
		Source:    "iam.tenant.events",
		TenantID:  tenantID.String(),
		Subject:   tenantID.String(),
		Timestamp: time.Now().UTC().Add(-30 * time.Second),
		Payload:   json.RawMessage(`{}`),
	}
	body, err := json.Marshal(envelope)
	require.NoError(t, err)

	e.publishEnvelope(t, topic, envelope.Type, body, nil)
	// Second publish — same envelope ID, different SQS message ID (SNS
	// re-fans-out fresh). This is what an SQS visibility-timeout redelivery
	// looks like from the consumer's perspective.
	e.publishEnvelope(t, topic, envelope.Type, body, nil)

	require.Eventually(t, func() bool {
		var n int
		if err := e.rawPool.QueryRow(e.ctx,
			`SELECT count(*) FROM processed_events WHERE event_id = $1`, envelope.ID).Scan(&n); err != nil {
			return false
		}
		return n == 1
	}, 15*time.Second, 200*time.Millisecond,
		"P12-IDEMP-001: processed_events must dedup to exactly one row for repeated deliveries")

	// The projection ran once — status flipped from trial → trial_expired
	// and stays there regardless of how many times the consumer sees the event.
	var status string
	require.NoError(t, e.rawPool.QueryRow(e.ctx,
		`SELECT status FROM tenants WHERE id = $1`, tenantID).Scan(&status))
	assert.Equal(t, "trial_expired", status,
		"P12-IDEMP-001: projection ran once — status change is durable")
}

// ── P12-IDEMP-002 ───────────────────────────────────────────────────────────

// TestSecondConsumerReceivesIndependently — processed_events is
// keyed by (event_id, consumer_name). If a HYPOTHETICAL second consumer with
// a different identity sees the same event, its INSERT succeeds because it's
// a distinct composite key. This isolates dedup per consumer per PE-1.
//
// We don't have two consumer identities in this service — but we exercise
// the primary key semantics by inserting a manual (event_id, other_consumer)
// row and verifying it coexists with the org-membership row.
func TestSecondConsumerReceivesIndependently(t *testing.T) {
	e := newPhase12Env(t)

	appPool, err := pgcommon.NewPool(e.ctx, pgcommon.Config{
		DSN:           e.dsn,
		PGBouncerMode: false,
	})
	require.NoError(t, err)
	defer appPool.Close()

	tenantID := seedTenantForConsumer(t, e, "idemp-multi-consumer")

	outbox := &noopOutbox{}
	memConsumer := consumer.NewMembershipEventConsumer(pgadapter.NewTxRunner(appPool, outbox), pgadapter.NewTenantRepository(appPool), pgadapter.NewIdempotencyRepository(appPool), nil, nil, 5*time.Minute, nil)

	topic := e.createTopic(t, "iam-tenant-events")
	q := e.createQueue(t, "tenant-orgm-q", "", 0)
	e.subscribeQueue(t, topic, q, "")

	stop := startConsumer(t, e, q, memConsumer.Handle)
	defer stop()

	envelope := events.Envelope[json.RawMessage]{
		ID:        uuid.NewString(),
		Type:      "TrialExpired",
		Source:    "iam.tenant.events",
		TenantID:  tenantID.String(),
		Subject:   tenantID.String(),
		Timestamp: time.Now().UTC().Add(-30 * time.Second),
		Payload:   json.RawMessage(`{}`),
	}
	body, err := json.Marshal(envelope)
	require.NoError(t, err)
	e.publishEnvelope(t, topic, envelope.Type, body, nil)

	require.Eventually(t, func() bool {
		var n int
		_ = e.rawPool.QueryRow(e.ctx,
			`SELECT count(*) FROM processed_events WHERE event_id = $1 AND consumer = 'iam-org-membership'`,
			envelope.ID).Scan(&n)
		return n == 1
	}, 15*time.Second, 200*time.Millisecond)

	// Second consumer identity — INSERT succeeds because composite PK differs.
	_, err = e.rawPool.Exec(e.ctx,
		`INSERT INTO processed_events (event_id, consumer, processed_at) VALUES ($1, 'notification', now())`,
		envelope.ID)
	require.NoError(t, err, "P12-IDEMP-002: second consumer identity must INSERT without conflict")

	var totalRows int
	require.NoError(t, e.rawPool.QueryRow(e.ctx,
		`SELECT count(*) FROM processed_events WHERE event_id = $1`, envelope.ID).Scan(&totalRows))
	assert.Equal(t, 2, totalRows,
		"P12-IDEMP-002: same event_id must coexist under two consumer identities (PE-1)")
}

// stringErr is a tiny errors.New replacement kept local so test code doesn't
// need to import the errors package just for one throwaway sentinel.
type stringErr string

func (e stringErr) Error() string { return string(e) }
func assertErr(msg string) error  { return stringErr(msg) }
