// traceparent_test.go covers the propagateTraceparent helper.
//
// The uncovered branch at line 42.9% is the "spanCtx is valid" path (lines
// 16-20). With only a plain context (no OTel span), SpanContext().IsValid()
// returns false and the function returns early — the happy path (valid span
// → header set) requires injecting a real OTel span.
package catalogadmin

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/trace"
)

// TestPropagateTraceparent_NoSpan_NoHeader verifies that when there is no
// OTel span in the context (spanCtx.IsValid() == false), propagateTraceparent
// is a no-op — the traceparent header is not set.
func TestPropagateTraceparent_NoSpan_NoHeader(t *testing.T) {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com", nil)
	propagateTraceparent(context.Background(), req)
	assert.Empty(t, req.Header.Get("traceparent"),
		"traceparent must not be set when there is no active span")
}

// TestPropagateTraceparent_WithValidSpan_SetsHeader verifies the valid-span
// branch: a context carrying a non-no-op span context causes propagateTraceparent
// to set the traceparent header.
func TestPropagateTraceparent_WithValidSpan_SetsHeader(t *testing.T) {
	// Build a minimal valid trace/span ID pair and inject them into the context.
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanCtx)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com", nil)
	propagateTraceparent(ctx, req)

	tp := req.Header.Get("traceparent")
	assert.NotEmpty(t, tp, "traceparent must be set when there is a valid span")
	assert.Contains(t, tp, "4bf92f3577b34da6a3ce929d0e0e4736", "header must contain the trace ID")
	assert.Contains(t, tp, "00f067aa0ba902b7", "header must contain the span ID")
	assert.True(t, len(tp) > 0 && tp[len(tp)-3:] == "-01", "sampled flag must be 01")
}
