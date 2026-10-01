package main

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
)

const (
	flociQueueURL = "http://floci:4566/000000000000/tenant-orgm-q"
	flociDLQURL   = "http://floci:4566/000000000000/tenant-orgm-q-dlq"
	flociDLQARN   = "arn:aws:sqs:ap-south-1:000000000000:tenant-orgm-q-dlq"
)

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

// ── routeRejectsToDLQ ──────────────────────────────────────────────────

func TestRoutePoisonPills_PoisonPill_SentToDLQAndAcked(t *testing.T) {
	transport := &fakeDLQTransport{dlqURL: flociDLQURL}
	h := routeRejectsToDLQ(handlerReturning(consumeradapter.ErrPoisonPill), transport, flociDLQURL, nil)

	env := testEnvelope()
	require.NoError(t, h(context.Background(), env), "a DLQ'd poison pill must ack the source message")

	require.Len(t, transport.sent, 1)
	msg := transport.sent[0]
	assert.Equal(t, flociDLQURL, msg.DLQUrl)
	assert.Equal(t, env.Type, msg.EventType)
	assert.Equal(t, poisonPillReason, msg.Reason)

	var got events.Envelope[json.RawMessage]
	require.NoError(t, json.Unmarshal([]byte(msg.Body), &got))
	assert.Equal(t, env.ID, got.ID)
	assert.JSONEq(t, string(env.Payload), string(got.Payload), "DLQ body carries the already-decoded plain payload")
	assert.Empty(t, got.SchemaID, "dataschema must be cleared so a redrive isn't Glue-decoded twice")
}

func TestRoutePoisonPills_WrappedPoisonPill_StillRouted(t *testing.T) {
	transport := &fakeDLQTransport{dlqURL: flociDLQURL}
	wrapped := errors.Join(errors.New("context"), consumeradapter.ErrPoisonPill)
	h := routeRejectsToDLQ(handlerReturning(wrapped), transport, flociDLQURL, nil)
	require.NoError(t, h(context.Background(), testEnvelope()))
	assert.Len(t, transport.sent, 1)
}

func TestRoutePoisonPills_SendFails_ReturnsOriginalErrorForRedrive(t *testing.T) {
	transport := &fakeDLQTransport{dlqURL: flociDLQURL, sendErr: errors.New("throttled")}
	log := &fakeLogger{}
	h := routeRejectsToDLQ(handlerReturning(consumeradapter.ErrPoisonPill), transport, flociDLQURL, log)

	err := h(context.Background(), testEnvelope())
	assert.ErrorIs(t, err, consumeradapter.ErrPoisonPill, "send failure must leave the message visible")
	assert.Equal(t, int32(1), log.warnCalls)
}

func TestRoutePoisonPills_SendFails_NilLogger_NoPanic(t *testing.T) {
	transport := &fakeDLQTransport{dlqURL: flociDLQURL, sendErr: errors.New("throttled")}
	h := routeRejectsToDLQ(handlerReturning(consumeradapter.ErrPoisonPill), transport, flociDLQURL, nil)
	assert.ErrorIs(t, h(context.Background(), testEnvelope()), consumeradapter.ErrPoisonPill)
}

func TestRoutePoisonPills_OtherOutcomes_PassThroughUntouched(t *testing.T) {
	transient := errors.New("db down")
	for name, want := range map[string]error{"success": nil, "transient error": transient} {
		t.Run(name, func(t *testing.T) {
			transport := &fakeDLQTransport{dlqURL: flociDLQURL}
			h := routeRejectsToDLQ(handlerReturning(want), transport, flociDLQURL, nil)
			assert.Equal(t, want, h(context.Background(), testEnvelope()))
			assert.Empty(t, transport.sent, "only ErrPoisonPill is sent to the DLQ")
		})
	}
}

// ── redriveURL / siblingQueueURL ───────────────────────────────────────────
// redriveURL is the pure-logic helper; sqsDLQAdapter.RedriveTargetURL
// (dlq_adapter.go) wraps it with the actual SQS GetQueueAttributes call.

func TestRedriveURL_FromRedrivePolicy(t *testing.T) {
	policy := `{"deadLetterTargetArn":"` + flociDLQARN + `","maxReceiveCount":"5"}`
	got, err := redriveURL(flociQueueURL, policy)
	require.NoError(t, err)
	assert.Equal(t, flociDLQURL, got)
}

func TestRedriveURL_Errors(t *testing.T) {
	cases := map[string]string{
		"no redrive":       "",
		"bad policy json":  "{",
		"non-sqs dlq arn":  `{"deadLetterTargetArn":"arn:aws:sns:ap-south-1:000000000000:topic"}`,
		"empty target arn": `{}`,
	}
	for name, policy := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := redriveURL(flociQueueURL, policy)
			assert.Error(t, err)
		})
	}
}

func TestSiblingQueueURL_RealAWSEndpoint(t *testing.T) {
	got, err := siblingQueueURL(
		"https://sqs.ap-south-1.amazonaws.com/123456789012/billing-orgm-q",
		"arn:aws:sqs:ap-south-1:123456789012:billing-orgm-q-dlq",
	)
	require.NoError(t, err)
	assert.Equal(t, "https://sqs.ap-south-1.amazonaws.com/123456789012/billing-orgm-q-dlq", got)
}

func TestSiblingQueueURL_Errors(t *testing.T) {
	_, err := siblingQueueURL("http://floci:4566/", flociDLQARN)
	assert.Error(t, err, "URL without /<account>/<name> path")
	_, err = siblingQueueURL("://bad", flociDLQARN)
	assert.Error(t, err, "unparseable URL")
}

// ── withDLQRouting ──────────────────────────────────────────────────────

func TestWithPoisonPillDLQ_Resolved_WrapsHandler(t *testing.T) {
	transport := &fakeDLQTransport{dlqURL: flociDLQURL}
	h := withDLQRouting(context.Background(), transport, flociQueueURL, handlerReturning(consumeradapter.ErrPoisonPill), &fakeLogger{})
	require.NoError(t, h(context.Background(), testEnvelope()))
	assert.Len(t, transport.sent, 1)
}

func TestWithPoisonPillDLQ_Unresolvable_FallsBackToRedrive(t *testing.T) {
	transport := &fakeDLQTransport{getErr: errors.New("access denied")}
	log := &fakeLogger{}
	h := withDLQRouting(context.Background(), transport, flociQueueURL, handlerReturning(consumeradapter.ErrPoisonPill), log)
	assert.ErrorIs(t, h(context.Background(), testEnvelope()), consumeradapter.ErrPoisonPill)
	assert.Empty(t, transport.sent)
	assert.Equal(t, int32(1), log.warnCalls)
}

// ── buildSQSConsumer — consumer-side Glue decode ───────────────────────────

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

// TestBuildSQSConsumer_DecodesGlueEncodedUpstreamPayload proves the
// consumer carries a codec: an upstream envelope with a dataschema and a
// base64 Glue-framed payload reaches the handler as plain JSON and is acked.
// Without WithConsumerCodec platform-events fails decode and never deletes.
func TestBuildSQSConsumer_DecodesGlueEncodedUpstreamPayload(t *testing.T) {
	versionID := uuid.New()
	plain := []byte(`{"reason":"billing_lapse"}`)
	framed := append([]byte{0x03, 0x00}, versionID[:]...)
	framed = append(framed, plain...)
	data, err := json.Marshal(framed) // []byte marshals as a base64 JSON string (WrapCodecPayload's format)
	require.NoError(t, err)

	body, err := json.Marshal(map[string]any{
		"id": "evt-1", "type": "TenantSuspended", "source": "iam-realm-provisioner",
		"tenant_id": uuid.NewString(), "dataschema": versionID.String(),
		"time": time.Now().UTC(), "data": json.RawMessage(data),
	})
	require.NoError(t, err)

	client := &oneShotSQS{
		msg:     &sqstypes.Message{MessageId: aws.String("m-1"), ReceiptHandle: aws.String("rh-1"), Body: aws.String(string(body))},
		deleted: make(chan struct{}),
	}
	got := make(chan json.RawMessage, 1)
	handler := func(_ context.Context, env events.Envelope[json.RawMessage]) error {
		got <- env.Payload
		return nil
	}

	env := eventcfg.SQSConfigEnv{QueueURL: flociQueueURL, Region: "ap-south-1", Concurrency: 1, MaxMessages: 1, WaitSeconds: 1, VisibilityTimeout: 30 * time.Second}
	cons, err := buildSQSConsumer(env, client, handler, nil)
	require.NoError(t, err)

	go func() { _ = cons.Start(t.Context()) }()
	defer func() { _ = cons.Stop() }()

	select {
	case payload := <-got:
		assert.JSONEq(t, string(plain), string(payload))
	case <-time.After(5 * time.Second):
		t.Fatal("handler never received the message — Glue decode likely failed")
	}
	select {
	case <-client.deleted:
	case <-time.After(5 * time.Second):
		t.Fatal("message was not acked")
	}
}
