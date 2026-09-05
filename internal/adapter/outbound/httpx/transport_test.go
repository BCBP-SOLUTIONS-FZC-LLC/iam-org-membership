package httpx_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/httpx"
)

func activeSpanContext(t *testing.T) (context.Context, string) {
	t.Helper()
	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	return trace.ContextWithSpanContext(context.Background(), sc), traceID.String()
}

func TestNewClient_InjectsTraceparent(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	ctx, traceID := activeSpanContext(t)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)

	resp, err := httpx.NewClient(5 * time.Second).Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.NotEmpty(t, got, "outbound call carried no traceparent — the trace breaks at this hop")
	assert.Contains(t, got, traceID, "the injected traceparent must carry the caller's trace id")
}

// TestNewClient_NonPositiveTimeoutDefaultsTo3s covers NewClient's <=0
// branch for both the zero value and a negative duration.
func TestNewClient_NonPositiveTimeoutDefaultsTo3s(t *testing.T) {
	for _, tc := range []time.Duration{0, -1 * time.Second} {
		c := httpx.NewClient(tc)
		assert.Equal(t, 3*time.Second, c.Timeout, "timeout %v must default to 3s", tc)
	}
}

func TestNewClient_WithoutAnActiveSpanStillSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)

	resp, err := httpx.NewClient(5 * time.Second).Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}
