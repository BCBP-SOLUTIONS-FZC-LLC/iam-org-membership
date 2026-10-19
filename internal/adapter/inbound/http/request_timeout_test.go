package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
)

// blockingResolver honours the request deadline, like a query cancelled by it.
type blockingResolver struct{}

func (blockingResolver) Resolve(ctx context.Context, _ string) (domain.TenantResolution, error) {
	<-ctx.Done()
	return domain.TenantResolution{}, ctx.Err()
}

func timeoutRouter(t *testing.T, timeout time.Duration) *Router {
	t.Helper()
	return NewRouter(RouterConfig{
		GinConfig:     gincommon.Config{Domain: "iam", Environment: "test", ServiceName: "iam-org-membership", RequestTimeout: timeout},
		PublicHandler: NewPublicHandler(blockingResolver{}),
	})
}

// httpStatusClasses returns the platform_http_requests_total status_class
// values recorded (non-zero) for route.
func httpStatusClasses(t *testing.T, route string) map[string]bool {
	t.Helper()
	mfs, err := gincommon.MetricsGatherer().Gather()
	require.NoError(t, err)
	out := map[string]bool{}
	for _, mf := range mfs {
		if mf.GetName() != "platform_http_requests_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["route"] == route && m.GetCounter().GetValue() > 0 {
				out[labels["status_class"]] = true
			}
		}
	}
	return out
}

// requestTimeouts returns platform_http_request_timeouts_total for route.
func requestTimeouts(t *testing.T, route string) float64 {
	t.Helper()
	mfs, err := gincommon.MetricsGatherer().Gather()
	require.NoError(t, err)
	var total float64
	for _, mf := range mfs {
		if mf.GetName() != "platform_http_request_timeouts_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "route" && lp.GetValue() == route {
					total += m.GetCounter().GetValue()
				}
			}
		}
	}
	return total
}

// A query cut short by the request deadline is answered by gincommon's
// timeout middleware (503, counted on platform_http_request_timeouts_total):
// HandleError leaves the response unwritten so the library can do both.
func TestRouter_RequestTimeout_DeadlineReturns503(t *testing.T) {
	rt := timeoutRouter(t, 50*time.Millisecond)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/public/tenants/resolve?identifier=slow", http.NoBody)
	w := httptest.NewRecorder()
	start := time.Now()
	rt.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), `"errorCode":"REQUEST_TIMEOUT"`, "gincommon's timeout body")
	assert.Less(t, time.Since(start), 5*time.Second, "the deadline must actually be enforced")
	assert.True(t, httpStatusClasses(t, "/api/v1/public/tenants/resolve")["5xx"])
	assert.Positive(t, requestTimeouts(t, "/api/v1/public/tenants/resolve"), "the library must count the timeout")
}

// HandleError on a request whose own context ended must not answer for it:
// an expired deadline is left to gincommon's timeout middleware, and a
// client that went away is recorded as 499, not a 5xx.
func TestHandleError_RequestContextEnded(t *testing.T) {
	deadlineCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	c, w := newTestContext(nil)
	c.Request = c.Request.WithContext(deadlineCtx)
	HandleError(c, domain.NewError(domain.ErrRequestTimeout, "deadline"))
	assert.False(t, c.Writer.Written(), "an expired request deadline is answered by the library")
	assert.True(t, c.IsAborted())
	assert.Equal(t, http.StatusOK, c.Writer.Status(), "status stays unset so TimeoutMiddleware writes 503")
	assert.Empty(t, w.Body.String())

	canceledCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	c, _ = newTestContext(nil)
	c.Request = c.Request.WithContext(canceledCtx)
	HandleError(c, context.Canceled)
	assert.Equal(t, clientClosedRequest, c.Writer.Status())

	// A timeout that is not the request's own (statement_timeout) is still
	// reported by HandleError.
	c, w = newTestContext(nil)
	HandleError(c, domain.NewError(domain.ErrRequestTimeout, "statement timeout"))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), `"errorCode":"REQUEST_TIMEOUT"`)
}

// The deadline middleware must sit INSIDE the observability stack: when a
// handler honours the deadline and returns without writing, gincommon's
// TimeoutMiddleware writes the 503 after c.Next(). Outside observability
// (the old r.Use(TimeoutMiddleware) first) the metrics had already recorded
// the unwritten default 200 by then.
func TestRouter_RequestTimeout_RecordedAsTheClientSeesIt(t *testing.T) {
	rt := timeoutRouter(t, 50*time.Millisecond)
	const route = "/__test/timeout-without-write"
	rt.engine.GET(route, func(c *gin.Context) { <-c.Request.Context().Done() })

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, route, http.NoBody)
	w := httptest.NewRecorder()
	rt.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	classes := httpStatusClasses(t, route)
	assert.True(t, classes["5xx"], "platform_http_requests_total must record the 503 the client got")
	assert.False(t, classes["2xx"], "a request the client saw as 503 must not be recorded as 2xx")
}

func TestNewRouter_DefaultsRequestTimeout(t *testing.T) {
	assert.Equal(t, 30*time.Second, defaultRequestTimeout)
}
