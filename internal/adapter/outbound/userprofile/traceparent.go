package userprofile

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel/trace"
)

// propagateTraceparent sets the W3C traceparent header from the OTel span
// context in ctx, so the UP-side spans link to the O&M-side originating
// span. If there's no active span (e.g. reconciler cron), the header is
// omitted.
//
// The full gincommon.PropagateHeaders helper covers custom correlation
// IDs and tenant context too; we replicate the traceparent piece inline
// so this outbound package doesn't take a gincommon dependency.
func propagateTraceparent(ctx context.Context, req *http.Request) {
	spanCtx := trace.SpanFromContext(ctx).SpanContext()
	if !spanCtx.IsValid() {
		return
	}
	// W3C traceparent: version=00, trace-id (32 hex), parent-id (16 hex),
	// trace-flags (2 hex).
	flags := "00"
	if spanCtx.IsSampled() {
		flags = "01"
	}
	req.Header.Set("traceparent",
		"00-"+spanCtx.TraceID().String()+"-"+spanCtx.SpanID().String()+"-"+flags)
}
