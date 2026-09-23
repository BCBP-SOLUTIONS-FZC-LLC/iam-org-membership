package main

import (
	"context"
	"encoding/json"

	"github.com/aws/aws-sdk-go-v2/service/glue"

	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// buildTopicPublisher returns an SNS publisher for topicARN carrying codec
// (applied transiently at publish time via events.WithCodec — never touches
// outbox_events), or a noop publisher when the ARN is empty (dev/test).
//
// Region and EndpointURL come from eventcfg.LoadSNS (AWS_REGION /
// AWS_ENDPOINT_URL). TopicARN is the per-topic override because this
// service publishes to two SNS topics and LoadSNS only reads SNS_TOPIC_ARN.
func buildTopicPublisher(topicARN string, codec events.Codec, log port.Logger) (events.Publisher, error) {
	if topicARN == "" {
		return eventbusadapter.NoopPublisher{}, nil
	}
	snsEnv := eventcfg.LoadSNS()
	snsEnv.TopicARN = topicARN
	return events.NewSNSPublisher(eventcfg.SNSConfigFromEnv(snsEnv, log), events.WithCodec(codec))
}

// sqsEnvForQueue copies the shared LoadSQS() values (region, endpoint,
// long-poll, visibility, max receive count) onto one inbound queue, then
// overlays that queue's URL and concurrency. LoadSQS only reads SQS_QUEUE_URL
// / SQS_CONCURRENCY; this service has three queues with distinct env names.
func sqsEnvForQueue(base eventcfg.SQSConfigEnv, queueURL string, concurrency int) eventcfg.SQSConfigEnv {
	env := base
	env.QueueURL = queueURL
	if concurrency > 0 {
		env.Concurrency = concurrency
	}
	return env
}

// buildSQSConsumer constructs a platform-events SQS consumer from a
// LoadSQS-derived env (after sqsEnvForQueue) plus SQSConsumerOptions
// (concurrency, visibility timeout, optional max-receive-count) and the
// consumer-side GlueDecoder. Without a consumer codec, any upstream
// envelope carrying a dataschema (i.e. Glue-encoded by its producer) fails
// decode on every delivery and ends up in the DLQ.
func buildSQSConsumer(env eventcfg.SQSConfigEnv, client events.SQSClientLike, handler events.Handler, log port.Logger) (events.Consumer, error) {
	return events.NewSQSConsumerWithClient(
		eventcfg.SQSConfigFromEnv(env, log),
		client,
		handler,
		append(eventcfg.SQSConsumerOptions(env), events.WithConsumerCodec(eventbusadapter.GlueDecoder{}))...,
	)
}

// buildTopicCodec returns a GlueCodec whose schema version UUIDs are
// resolved once, by definition, from registryName — fixed for the life of
// the process, so no refresher — or a NoopCodec (plain JSON) when
// registryName is empty (dev/test without a Glue registry configured).
func buildTopicCodec(ctx context.Context, glueClient *glue.Client, registryName string, schemaNames []string) (events.Codec, error) {
	if registryName == "" {
		// events.NoopCodec (platform-events' own identity Codec), not this
		// package's local eventbus.Codec/NoopCodec — those are a different,
		// Encode-only interface used at outbox-enqueue time for schema
		// validation, not the Encode+Decode events.Codec WithCodec expects.
		return events.NoopCodec{}, nil
	}
	return eventbusadapter.NewGlueCodec(ctx, glueClient, registryName, schemaNames)
}

// instrumentedHandler wraps an SQS handler with the Tier-1
// platform_messages_{received,processed,failed}_total counters, labelled by
// queue. Wired here rather than inside MembershipEventConsumer.Handle
// because the queue identity is only known at the subscription call site —
// both tenant-orgm-q and billing-orgm-q share the same Handle method.
func instrumentedHandler(queue string, h events.Handler) events.Handler {
	return func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		metrics.IncMessagesReceived(queue)
		if err := h(ctx, env); err != nil {
			metrics.IncMessagesFailed(queue)
			return err
		}
		metrics.IncMessagesProcessed(queue)
		return nil
	}
}
