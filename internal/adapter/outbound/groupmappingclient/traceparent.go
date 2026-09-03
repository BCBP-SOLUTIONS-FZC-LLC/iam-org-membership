package groupmappingclient

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

func propagateTraceparent(ctx context.Context, req *http.Request) {
	if req == nil {
		return
	}
	// Inject W3C traceparent via gincommon's global TextMapPropagator
	// (TraceContext, plus Baggage when enabled). Identical to
	// gincommon.PropagateHeaders' inject step, usable from outbound
	// clients that only have context.Context rather than *gin.Context.
	// Composite with TraceContext so unit tests that never bootstrap
	// gincommon still emit a header (the global propagator is a no-op
	// until InitTracingFromEnv / ObservabilityMiddlewares run).
	propagation.NewCompositeTextMapPropagator(
		otel.GetTextMapPropagator(),
		propagation.TraceContext{},
	).Inject(ctx, propagation.HeaderCarrier(req.Header))
}
