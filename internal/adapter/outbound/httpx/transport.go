// Package httpx supplies the single HTTP transport every outbound adapter
// in this service builds on, so W3C `traceparent` injection and client-span
// emission cannot be forgotten on a new call site. Without it an outbound
// call starts a fresh, parentless trace at the callee. Mirrors
// iam-realm-provisioner's httpx package.
package httpx

import (
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// HeaderRequestID is the correlation header forwarded on every outbound
// call.
const HeaderRequestID = gincommon.HeaderRequestID

// NewTransport wraps base — nil meaning http.DefaultTransport — with the
// OpenTelemetry round-tripper, which injects the traceparent header carried
// on the request context and records a client span per call, and forwards
// the inbound request's x-request-id (requestctx.WithRequestID) so the
// callee's logs and error bodies carry the same correlation ID. Both
// composition roots call gincommon.InitTracingWithConfig unconditionally, so
// the global propagator and tracer provider are already installed.
func NewTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(requestIDTransport{base: base})
}

// requestIDTransport sets X-Request-ID from the request context when the
// caller has not set one.
type requestIDTransport struct {
	base http.RoundTripper
}

func (t requestIDTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	id := requestctx.RequestIDFromContext(req.Context())
	if id == "" || req.Header.Get(HeaderRequestID) != "" {
		return t.base.RoundTrip(req)
	}
	out := req.Clone(req.Context()) // RoundTrippers must not mutate the caller's request
	out.Header.Set(HeaderRequestID, id)
	return t.base.RoundTrip(out)
}

// NewClient returns an http.Client with the instrumented transport, the
// given per-request timeout, and the same idle-conn settings the outbound
// adapters used before this wrapper.
func NewClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &http.Client{
		Timeout: timeout,
		Transport: NewTransport(&http.Transport{
			IdleConnTimeout:       30 * time.Second,
			MaxIdleConnsPerHost:   8,
			ResponseHeaderTimeout: timeout,
		}),
	}
}
