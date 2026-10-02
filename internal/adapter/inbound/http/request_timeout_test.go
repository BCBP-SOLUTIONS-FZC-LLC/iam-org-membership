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
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
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
	assert.Contains(t, w.Body.String(), `"error":"request timeout"`, "gincommon's timeout body")
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
	assert.Contains(t, w.Body.String(), `"code":"request_timeout"`)
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

// NormalizeAuthErrors forwards only what the handler produced: nothing
// written stays unwritten (so an outer middleware can still answer), and a
// status set without a body still reaches the client.
func TestNormalizeAuthErrors_ForwardsOnlyWhatTheHandlerWrote(t *testing.T) {
	newEngine := func(h gin.HandlerFunc) (*gin.Engine, *bool) {
		r := gin.New()
		written := new(bool)
		r.Use(func(c *gin.Context) {
			c.Next()
			*written = c.Writer.Written()
		})
		r.Use(NormalizeAuthErrors())
		r.GET("/", h)
		return r, written
	}

	r, written := newEngine(func(*gin.Context) {})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody))
	assert.False(t, *written, "an empty handler must leave the response unwritten for the outer middlewares")

	r, _ = newEngine(func(c *gin.Context) { c.Status(http.StatusNoContent) })
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody))
	assert.Equal(t, http.StatusNoContent, w.Code)

	r, _ = newEngine(func(c *gin.Context) { c.JSON(http.StatusCreated, gin.H{"ok": true}) })
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody))
	assert.Equal(t, http.StatusCreated, w.Code)
	assert.JSONEq(t, `{"ok":true}`, w.Body.String())
}

// Only a 401 is held back and normalised; an ambiguous identity header gets
// its own code.
func TestNormalizeAuthErrors_OnlyRewrites401(t *testing.T) {
	r := gin.New()
	r.Use(NormalizeAuthErrors())
	r.GET("/missing", func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or invalid authentication headers", "status": 401})
	})
	r.GET("/ambiguous", func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "ambiguous x-user-id header", "status": 401})
	})
	r.GET("/forbidden", func(c *gin.Context) { c.JSON(http.StatusForbidden, gin.H{"error": "nope"}) })

	for path, want := range map[string]string{"/missing": "missing_identity_headers", "/ambiguous": "ambiguous_identity_headers"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, http.NoBody))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Contains(t, w.Body.String(), `"code":"`+want+`"`, path)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/forbidden", http.NoBody))
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.JSONEq(t, `{"error":"nope"}`, w.Body.String(), "non-401 bodies pass through untouched")
}

// ContextMiddleware's library 400 gets a code too; a handler's own 400
// (which already has one) is untouched.
func TestNormalizeAuthErrors_LibraryBadRequestGetsCode(t *testing.T) {
	r := gin.New()
	r.Use(NormalizeAuthErrors())
	r.GET("/lib", func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid request context", "status": 400})
	})
	r.GET("/own", func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "validation_error", "code": "validation_error"})
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/lib", http.NoBody))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), `"code":"invalid_request_context"`)

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/own", http.NoBody))
	assert.JSONEq(t, `{"error":"validation_error","code":"validation_error"}`, w.Body.String())
}

// A handler panic unwinds past NormalizeAuthErrors; the outer recovery must
// still write to the real response writer, not into the discarded buffer.
func TestNormalizeAuthErrors_RestoresWriterOnPanic(t *testing.T) {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		defer func() {
			if recover() != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal"})
			}
		}()
		c.Next()
	})
	r.Use(NormalizeAuthErrors())
	r.GET("/", func(c *gin.Context) {
		c.Status(http.StatusUnauthorized) // buffered, then the handler panics
		panic("boom")
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "internal")
}
