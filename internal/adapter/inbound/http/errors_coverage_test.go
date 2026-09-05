// Coverage tests for errors.go's newErrorResponse request-id fallback
// chain: ctx-stored request_id (set by gincommon's RequestIDMiddleware,
// key "request_id") → x-request-id request header → X-Request-ID response
// header → none. helpers_edges_test.go already covers the nil-context and
// "none set" cases; this file closes the three populated-source branches.
package http

import (
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/stretchr/testify/assert"
)

func TestNewErrorResponse_RequestIDFromContext(t *testing.T) {
	c, _ := buildCtx("GET", "/", ``, nil)
	// "request_id" is the literal Gin context key RequestIDMiddleware sets
	// (platform-gincommon internal/.../middleware.CtxRequestIDKey) — Gin
	// context keys are plain strings, so setting it directly here exercises
	// gincommon.RequestIDFromContext's read path without needing the real
	// middleware to run.
	c.Set("request_id", "ctx-request-id")

	er := newErrorResponse(c, "code", "msg", nil)

	assert.Equal(t, "ctx-request-id", er.RequestID)
}

func TestNewErrorResponse_RequestIDFromRequestHeader(t *testing.T) {
	c, _ := buildCtx("GET", "/", ``, nil)
	c.Request.Header.Set(gincommon.HeaderRequestID, "hdr-request-id")

	er := newErrorResponse(c, "code", "msg", nil)

	assert.Equal(t, "hdr-request-id", er.RequestID)
}

func TestNewErrorResponse_RequestIDFromResponseHeader(t *testing.T) {
	c, _ := buildCtx("GET", "/", ``, nil)
	c.Writer.Header().Set(gincommon.HeaderRequestIDResponse, "resp-request-id")

	er := newErrorResponse(c, "code", "msg", nil)

	assert.Equal(t, "resp-request-id", er.RequestID)
}
