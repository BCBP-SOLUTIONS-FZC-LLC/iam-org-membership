package realmprovisioner

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel/trace"
)

func propagateTraceparent(ctx context.Context, req *http.Request) {
	spanCtx := trace.SpanFromContext(ctx).SpanContext()
	if !spanCtx.IsValid() {
		return
	}
	flags := "00"
	if spanCtx.IsSampled() {
		flags = "01"
	}
	req.Header.Set("traceparent",
		"00-"+spanCtx.TraceID().String()+"-"+spanCtx.SpanID().String()+"-"+flags)
}
