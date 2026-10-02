package sqspipeline

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	consumeradapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/consumer"
	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events/mock"
)

const flociQueueURL = "http://floci:4566/000000000000/tenant-orgm-q"

func handlerReturning(err error) events.Handler {
	return func(context.Context, events.Envelope[json.RawMessage]) error { return err }
}

func testEnvelope() events.Envelope[json.RawMessage] {
	return events.Envelope[json.RawMessage]{
		ID: "evt-1", Type: "TenantSuspended", Source: "iam-realm-provisioner",
		TenantID: uuid.NewString(), SchemaID: uuid.NewString(),
		Timestamp: time.Now().Add(time.Hour).UTC(), Payload: json.RawMessage(`{"reason":"billing"}`),
	}
}

// ── requireDLQ — startup preflight (gap: a missing RedrivePolicy must stop
// startup, a transient failure must not disable DLQ routing) ─────────────

func TestRequireDLQ_Resolved(t *testing.T) {
	require.NoError(t, requireDLQ(context.Background(), &mock.DLQPublisher{}, flociQueueURL, &fakeLogger{}))
}

func TestRequireDLQ_NoRedrivePolicy_IsFatal(t *testing.T) {
	for _, cfgErr := range []error{events.ErrDLQNotConfigured, events.ErrDLQInvalidRedrivePolicy} {
		dlq := &mock.DLQPublisher{}
		dlq.SetError(cfgErr)
		err := requireDLQ(context.Background(), dlq, flociQueueURL, &fakeLogger{})
		require.Error(t, err)
		assert.ErrorIs(t, err, cfgErr)
	}
}

func TestRequireDLQ_TransientFailure_WarnsAndContinues(t *testing.T) {
	dlq := &mock.DLQPublisher{}
	dlq.SetError(errors.New("throttled"))
	log := &fakeLogger{}
	require.NoError(t, requireDLQ(context.Background(), dlq, flociQueueURL, log),
		"a transient failure must not stop startup; the publisher resolves again on the first forward")
	assert.Equal(t, int32(1), log.warnCalls)
}

// ── routeRejectsToDLQ outside a consumer dispatch ────────────────────────

func TestRouteRejects_NoSourceMessage_KeepsRetrySemantics(t *testing.T) {
	dlq := &mock.DLQPublisher{}
	h := routeRejectsToDLQ(handlerReturning(consumeradapter.ErrPoisonPill), dlq, &fakeLogger{})
	assert.ErrorIs(t, h(context.Background(), testEnvelope()), consumeradapter.ErrPoisonPill)
	assert.Empty(t, dlq.Sent())
}

func TestRouteRejects_OtherOutcomes_PassThroughUntouched(t *testing.T) {
	transient := errors.New("db down")
	for name, in := range map[string]error{"success": nil, "transient": transient} {
		t.Run(name, func(t *testing.T) {
			dlq := &mock.DLQPublisher{}
			h := routeRejectsToDLQ(handlerReturning(in), dlq, &fakeLogger{})
			assert.Equal(t, in, h(context.Background(), testEnvelope()))
			assert.Empty(t, dlq.Sent())
		})
	}
}

// ── Real platform-events consumer over a fake SQS transport ──────────────

// oneShotSQS delivers one message on the first ReceiveMessage, then idles.
// It implements the platform-events SQS client interface directly (raw SDK
// types are legitimate in test files — they simulate external actors).
type oneShotSQS struct {
	mu      sync.Mutex
	msg     *sqstypes.Message
	deleted chan struct{}
}

func (s *oneShotSQS) ReceiveMessage(ctx context.Context, _ *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	s.mu.Lock()
	msg := s.msg
	s.msg = nil
	s.mu.Unlock()
	if msg != nil {
		return &sqs.ReceiveMessageOutput{Messages: []sqstypes.Message{*msg}}, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(20 * time.Millisecond):
		return &sqs.ReceiveMessageOutput{}, nil
	}
}

func (s *oneShotSQS) DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	close(s.deleted)
	return &sqs.DeleteMessageOutput{}, nil
}

func (s *oneShotSQS) ChangeMessageVisibility(context.Context, *sqs.ChangeMessageVisibilityInput, ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	return &sqs.ChangeMessageVisibilityOutput{}, nil
}

func newOneShot(body string, attrs map[string]string) *oneShotSQS {
	msg := &sqstypes.Message{
		MessageId: aws.String("m-1"), ReceiptHandle: aws.String("rh-1"), Body: aws.String(body),
		Attributes: map[string]string{"ApproximateReceiveCount": "1"},
	}
	if len(attrs) > 0 {
		msg.MessageAttributes = map[string]sqstypes.MessageAttributeValue{}
		for k, v := range attrs {
			msg.MessageAttributes[k] = sqstypes.MessageAttributeValue{DataType: aws.String("String"), StringValue: aws.String(v)}
		}
	}
	return &oneShotSQS{msg: msg, deleted: make(chan struct{})}
}

func testSQSEnv() eventcfg.SQSConfigEnv {
	return sqsEnvForQueue(eventcfg.SQSConfigEnv{QueueURL: flociQueueURL, Region: "ap-south-1", Concurrency: 1, MaxMessages: 1, WaitSeconds: 1, VisibilityTimeout: 30 * time.Second}, flociQueueURL, 1)
}

// runConsumer starts a consumer and returns whether the message was deleted
// within wait.
func runConsumer(t *testing.T, client *oneShotSQS, handler events.Handler, dlq events.DLQPublisher, wait time.Duration) bool {
	t.Helper()
	cons, err := buildSQSConsumer(testSQSEnv(), client, handler, dlq, &fakeLogger{})
	require.NoError(t, err)
	go func() { _ = cons.Start(t.Context()) }()
	defer func() { _ = cons.Stop() }()
	select {
	case <-client.deleted:
		return true
	case <-time.After(wait):
		return false
	}
}

// glueFramedBody is an upstream envelope whose data is Glue-framed and whose
// dataschema is set, as Realm Provisioner publishes it.
func glueFramedBody(t *testing.T, at time.Time, payload string) string {
	t.Helper()
	versionID := uuid.New()
	framed := append([]byte{0x03, 0x00}, versionID[:]...)
	framed = append(framed, payload...)
	data, err := json.Marshal(framed) // []byte marshals as a base64 JSON string (WrapCodecPayload's format)
	require.NoError(t, err)
	body, err := json.Marshal(map[string]any{
		"id": "evt-1", "type": "TenantSuspended", "source": "iam-realm-provisioner",
		"tenant_id": uuid.NewString(), "dataschema": versionID.String(),
		"time": at, "data": json.RawMessage(data),
	})
	require.NoError(t, err)
	return string(body)
}

// TestBuildSQSConsumer_DecodesGlueEncodedUpstreamPayload proves the
// consumer carries a codec: an upstream envelope with a dataschema and a
// base64 Glue-framed payload reaches the handler as plain JSON and is acked.
// Without WithConsumerCodec platform-events fails decode and never deletes.
func TestBuildSQSConsumer_DecodesGlueEncodedUpstreamPayload(t *testing.T) {
	plain := `{"reason":"billing_lapse"}`
	client := newOneShot(glueFramedBody(t, time.Now().UTC(), plain), nil)
	got := make(chan json.RawMessage, 1)
	handler := func(_ context.Context, env events.Envelope[json.RawMessage]) error {
		got <- env.Payload
		return nil
	}
	require.True(t, runConsumer(t, client, handler, &mock.DLQPublisher{}, 5*time.Second), "message was not acked")
	select {
	case payload := <-got:
		assert.JSONEq(t, plain, string(payload))
	default:
		t.Fatal("handler never received the message — Glue decode likely failed")
	}
}

// A body that is not a valid envelope used to be deleted and only logged —
// a lost lifecycle (or offboarding) event. With WithDLQForwarding it lands
// in the DLQ byte-for-byte, and is deleted only after the forward.
func TestConsumer_MalformedBody_ForwardedToDLQNotDropped(t *testing.T) {
	const body = `this is not an event envelope`
	client := newOneShot(body, map[string]string{"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"})
	dlq := &mock.DLQPublisher{}
	handlerCalled := false
	h := func(context.Context, events.Envelope[json.RawMessage]) error { handlerCalled = true; return nil }

	require.True(t, runConsumer(t, client, h, dlq, 5*time.Second), "forwarded message must then be deleted")
	assert.False(t, handlerCalled)
	sent := dlq.Sent()
	require.Len(t, sent, 1)
	assert.Equal(t, body, string(sent[0].Body))
	assert.Equal(t, flociQueueURL, sent[0].SourceQueueURL)
	assert.Equal(t, "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", sent[0].Attrs["traceparent"])
}

// A permanent reject is forwarded as the ORIGINAL transport message — Glue
// framing, dataschema and message attributes intact — not a re-serialised,
// already-decoded envelope, so it can be redriven unchanged.
func TestConsumer_PoisonPill_ForwardsOriginalMessage(t *testing.T) {
	body := glueFramedBody(t, time.Now().Add(time.Hour).UTC(), `{"tenant_id":"t","realm":"r","suspended_at":"2026-09-23T00:00:00Z","source":"operator"}`)
	attrs := map[string]string{"EventType": "TenantSuspended", "TenantID": "tenant-123", "traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}
	client := newOneShot(body, attrs)
	dlq := &mock.DLQPublisher{}

	h := routeRejectsToDLQ(handlerReturning(consumeradapter.ErrPoisonPill), dlq, &fakeLogger{})
	require.True(t, runConsumer(t, client, h, dlq, 5*time.Second), "a forwarded reject must be acked")
	sent := dlq.Sent()
	require.Len(t, sent, 1)
	assert.Equal(t, body, string(sent[0].Body), "the original body, Glue framing included")
	assert.Equal(t, poisonPillReason, sent[0].Reason)
	for k, v := range attrs {
		assert.Equal(t, v, sent[0].Attrs[k], "message attribute %s must survive the forward", k)
	}
}

// Through the full inbound pipeline: a schema violation is forwarded with
// DLQReason=schema_violation and never reaches the handler.
func TestConsumer_SchemaViolation_ForwardedThroughInboundPipeline(t *testing.T) {
	body := glueFramedBody(t, time.Now().UTC(), `{"source":"hacker"}`)
	client := newOneShot(body, nil)
	dlq := &mock.DLQPublisher{}
	inner := &recordingHandler{}

	h := inboundHandler("tenant-orgm-q", inner.handle, realValidator(t), dlq, &fakeLogger{})
	require.True(t, runConsumer(t, client, h, dlq, 5*time.Second))
	assert.Zero(t, inner.calls)
	sent := dlq.Sent()
	require.Len(t, sent, 1)
	assert.Equal(t, schemaViolationReason, sent[0].Reason)
	assert.Equal(t, body, string(sent[0].Body))
}

// A failed forward leaves the message on the queue: it is neither acked nor
// lost, so retry and SQS redrive remain the backstop.
func TestConsumer_DLQSendFails_MessageStaysOnQueue(t *testing.T) {
	body := glueFramedBody(t, time.Now().Add(time.Hour).UTC(), `{"reason":"x"}`)
	client := newOneShot(body, nil)
	dlq := &mock.DLQPublisher{}
	log := &fakeLogger{}
	h := routeRejectsToDLQ(func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		dlq.SetError(errors.New("sqs unavailable")) // after Start's DLQ check, before the forward
		return consumeradapter.ErrPoisonPill
	}, dlq, log)

	assert.False(t, runConsumer(t, client, h, dlq, 500*time.Millisecond), "a message whose forward failed must not be deleted")
	assert.Empty(t, dlq.Sent())
	assert.GreaterOrEqual(t, log.warnCalls, int32(1))
}

// An unset SQS_MAX_RECEIVE_COUNT becomes 4, strictly below the queues'
// RedrivePolicy maxReceiveCount of 5, so the library's own forward fires
// before SQS moves the message.
func TestSQSEnvForQueue_DefaultsMaxReceiveBelowRedrive(t *testing.T) {
	assert.Equal(t, dlqForwardReceiveCount, sqsEnvForQueue(eventcfg.SQSConfigEnv{}, flociQueueURL, 0).MaxReceiveCount)
	assert.Equal(t, 3, sqsEnvForQueue(eventcfg.SQSConfigEnv{MaxReceiveCount: 3}, flociQueueURL, 0).MaxReceiveCount)
}

// Unset drain/handler timeouts get bounded defaults: the drain must fit the
// shutdown budget, the handler timeout must stay below the visibility timeout.
func TestSQSEnvForQueue_DefaultsDrainAndHandlerTimeouts(t *testing.T) {
	env := sqsEnvForQueue(eventcfg.SQSConfigEnv{}, flociQueueURL, 0)
	assert.Equal(t, sqsDrainTimeout, env.DrainTimeout)
	assert.Equal(t, sqsHandlerTimeout, env.HandlerTimeout)
	assert.Equal(t, sqsQueueDepthInterval, env.QueueDepthInterval, "queue/DLQ depth sampling is on by default")
	assert.Less(t, sqsHandlerTimeout, 30*time.Second, "handler timeout must stay below the SQS visibility timeout")

	env = sqsEnvForQueue(eventcfg.SQSConfigEnv{DrainTimeout: time.Second, HandlerTimeout: 2 * time.Second}, flociQueueURL, 0)
	assert.Equal(t, time.Second, env.DrainTimeout)
	assert.Equal(t, 2*time.Second, env.HandlerTimeout)
}
