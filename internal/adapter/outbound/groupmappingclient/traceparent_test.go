// traceparent_test.go covers the propagateTraceparent helper.
package groupmappingclient

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/trace"
)

func TestPropagateTraceparent_NoSpan_NoHeader(t *testing.T) {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com", nil)
	propagateTraceparent(context.Background(), req)
	assert.Empty(t, req.Header.Get("traceparent"))
}

func TestPropagateTraceparent_WithValidSpan_SetsHeader(t *testing.T) {
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
	assert.NotEmpty(t, tp)
	assert.Contains(t, tp, "4bf92f3577b34da6a3ce929d0e0e4736")
	assert.Contains(t, tp, "00f067aa0ba902b7")
}
