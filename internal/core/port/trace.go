package port

import "context"

// SpanContext holds the OTel trace and span IDs of the active span as plain
// hex strings. Outbound adapters that need to stamp correlation IDs on
// outbox envelopes (publisher.go) use this type instead of importing the
// OTel trace API themselves — the concrete extraction is installed by the
// composition root (cmd/) after gincommon.InitTracingWithConfig.
type SpanContext struct {
	TraceID string // 32-hex trace ID; empty when no active span
	SpanID  string // 16-hex span ID; empty when no active span
}

// spanContextExtractorFn is installed once at startup by the composition
// root via SetSpanContextExtractor. nil is safe: SpanContextFromContext
// returns a zero SpanContext.
var spanContextExtractorFn func(context.Context) SpanContext

// SetSpanContextExtractor installs the OTel span-context extractor. Call
// it once from the composition root (cmd/server, cmd/reconciler) after
// gincommon.InitTracingWithConfig has installed the global tracer provider.
// Not safe for concurrent use after the first event-publisher call.
func SetSpanContextExtractor(fn func(context.Context) SpanContext) {
	spanContextExtractorFn = fn
}

// SpanContextFromContext returns the active OTel span context for ctx as
// plain strings. Returns a zero SpanContext when no extractor is installed
// or when there is no active span.
func SpanContextFromContext(ctx context.Context) SpanContext {
	if spanContextExtractorFn == nil {
		return SpanContext{}
	}
	return spanContextExtractorFn(ctx)
}

// ConsumerSpanFunc wraps one SQS message handler invocation in a trace
// span. name is the span name (e.g. "consumer.TenantCreated"), queue is
// the messaging.destination.name attribute, and link carries the publisher's
// trace/span IDs to link back to the originating request. The returned
// context carries the new child span; calling end(err) records the error (if
// non-nil) and closes the span.
//
// The concrete implementation is installed by cmd/server after
// gincommon.InitTracingWithConfig, keeping the OTel trace API out of
// internal adapters (OBS-7/OBS-9).
type ConsumerSpanFunc func(ctx context.Context, name, queue string, link SpanContext) (context.Context, func(error))

// consumerSpanFn is the installed implementation; nil when unset (tests,
// reconciler binary).
var consumerSpanFn ConsumerSpanFunc

// SetConsumerSpanFunc installs the OTel span wrapper for SQS consumer
// handlers. Call it once from the composition root (cmd/server) after
// gincommon.InitTracingWithConfig.
func SetConsumerSpanFunc(fn ConsumerSpanFunc) {
	consumerSpanFn = fn
}

// WrapConsumerSpan starts a trace span for one SQS handler invocation and
// returns the enriched context plus an end function. When no
// ConsumerSpanFunc is installed (tests, reconciler), ctx is returned
// unchanged and end is a no-op.
func WrapConsumerSpan(ctx context.Context, name, queue string, link SpanContext) (context.Context, func(error)) {
	if consumerSpanFn == nil {
		return ctx, func(error) {}
	}
	return consumerSpanFn(ctx, name, queue, link)
}
