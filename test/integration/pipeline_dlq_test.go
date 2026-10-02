//go:build integration

// The production inbound pipeline (internal/adapter/inbound/sqspipeline)
// against a real SQS (floci): consumer construction with this service's
// defaults, platform-events' DLQ forwarding, and the permanent-reject
// shortcut. TestMaxReceiveCountRoutesToDLQ (dlq_idemp_test.go) covers SQS's
// own redrive; these cover what the service actually runs.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/consumer"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/sqspipeline"
	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// pipelineQueue creates a source queue whose RedrivePolicy targets a fresh
// DLQ at maxReceiveCount 5 (the platform's setting), subscribed to a topic.
func pipelineQueue(t *testing.T, e *phase12Env, name string) (topic, queue, dlq string) {
	t.Helper()
	topic = e.createTopic(t, name+"-topic")
	dlq = e.createQueue(t, name+"-dlq", "", 0)
	queue = e.createQueue(t, name, e.queueArn(t, dlq), 5)
	_, err := e.sqsCli.SetQueueAttributes(e.ctx, &sqs.SetQueueAttributesInput{
		QueueUrl:   &queue,
		Attributes: map[string]string{"VisibilityTimeout": "2"},
	})
	require.NoError(t, err)
	e.subscribeQueue(t, topic, queue, "")
	return topic, queue, dlq
}

// startPipeline runs h behind the production pipeline (Handler + NewConsumer
// with EnvForQueue's defaults and a real DLQ publisher) on queue.
func startPipeline(t *testing.T, e *phase12Env, queue string, h events.Handler) func() {
	t.Helper()
	dlq, err := events.NewSQSDLQPublisherWithClient(events.DLQConfig{ConsumerName: "iam-org-membership-it"}, e.sqsCli)
	require.NoError(t, err)
	require.NoError(t, sqspipeline.RequireDLQ(e.ctx, dlq, queue, nil), "a queue with a RedrivePolicy passes the startup check")

	env := sqspipeline.EnvForQueue(eventcfg.SQSConfigEnv{
		Region: flociRegion, EndpointURL: e.endpoint,
		WaitSeconds: 1, MaxMessages: 1, VisibilityTimeout: 2 * time.Second,
	}, queue, 1)
	passAll := sqspipeline.ValidatorFunc(func(string, []byte) error { return nil })
	cons, err := sqspipeline.NewConsumer(env, e.sqsCli, sqspipeline.Handler("it-queue", h, passAll, dlq, nil), dlq, nil)
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

func publishTestEnvelope(t *testing.T, e *phase12Env, topic string) {
	t.Helper()
	env := events.Envelope[json.RawMessage]{
		ID: uuid.NewString(), Type: "TenantSuspended", Source: "iam-realm-provisioner",
		TenantID: uuid.NewString(), Timestamp: time.Now().UTC(), Payload: json.RawMessage(`{}`),
	}
	body, err := json.Marshal(env)
	require.NoError(t, err)
	e.publishEnvelope(t, topic, env.Type, body, nil)
}

func dlqReason(t *testing.T, e *phase12Env, dlq string) string {
	t.Helper()
	msgs := e.receiveMessages(t, dlq, 1, 30*time.Second)
	require.Len(t, msgs, 1, "exactly one message must reach the DLQ")
	attr, ok := msgs[0].MessageAttributes["DLQReason"]
	require.True(t, ok, "a forwarded message carries DLQReason")
	return *attr.StringValue
}

// EVT-15: a permanent reject is forwarded on its FIRST delivery — not
// retried until the receive count runs out.
func TestPipeline_PermanentReject_ForwardedOnFirstDelivery(t *testing.T) {
	e := newPhase12Env(t)
	topic, queue, dlq := pipelineQueue(t, e, "pipeline-reject")

	var attempts atomic.Int32
	stop := startPipeline(t, e, queue, func(context.Context, events.Envelope[json.RawMessage]) error {
		attempts.Add(1)
		return consumer.ErrPoisonPill
	})
	defer stop()

	publishTestEnvelope(t, e, topic)
	assert.Equal(t, "future_time_clamp", dlqReason(t, e, dlq))
	assert.Equal(t, int32(1), attempts.Load(), "a poison pill must not be redelivered")
}

// A transient failure is retried and then forwarded by platform-events at
// SQS_MAX_RECEIVE_COUNT (4), before SQS's own redrive at 5 would move it
// without the library's attributes.
func TestPipeline_TransientFailure_ForwardedAtReceiveCountFour(t *testing.T) {
	e := newPhase12Env(t)
	topic, queue, dlq := pipelineQueue(t, e, "pipeline-retry")

	var attempts atomic.Int32
	stop := startPipeline(t, e, queue, func(context.Context, events.Envelope[json.RawMessage]) error {
		attempts.Add(1)
		return errors.New("simulated transient failure")
	})
	defer stop()

	publishTestEnvelope(t, e, topic)
	reason := dlqReason(t, e, dlq)
	assert.NotEmpty(t, reason)
	assert.Equal(t, int32(4), attempts.Load(), "forwarded after the 4th failed receive, before the RedrivePolicy's 5")
}
