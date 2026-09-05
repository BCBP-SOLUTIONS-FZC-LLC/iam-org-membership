package realmprovisioner

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/trace"
)

func TestPropagateTraceparent_NoSpanInContextSkipsHeader(t *testing.T) {
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/", nil)
	propagateTraceparent(context.Background(), req)
	assert.Empty(t, req.Header.Get("traceparent"))
}

func TestPropagateTraceparent_SampledFlags01(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	spanID, _ := trace.SpanIDFromHex("1112131415161718")
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID,
		TraceFlags: trace.FlagsSampled, Remote: true,
	})
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/", nil)
	propagateTraceparent(trace.ContextWithSpanContext(context.Background(), sc), req)
	assert.Equal(t,
		"00-0102030405060708090a0b0c0d0e0f10-1112131415161718-01",
		req.Header.Get("traceparent"))
}

func TestPropagateTraceparent_UnsampledFlags00(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("aabbccddeeff00112233445566778899")
	spanID, _ := trace.SpanIDFromHex("aa11bb22cc33dd44")
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, Remote: true,
	})
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/", nil)
	propagateTraceparent(trace.ContextWithSpanContext(context.Background(), sc), req)
	assert.Equal(t,
		"00-aabbccddeeff00112233445566778899-aa11bb22cc33dd44-00",
		req.Header.Get("traceparent"))
}

func TestPropagateTraceparent_NilRequest_IsNoop(t *testing.T) {
	// req == nil triggers the early-return guard, verifying it never panics.
	assert.NotPanics(t, func() {
		propagateTraceparent(context.Background(), nil)
	})
}
