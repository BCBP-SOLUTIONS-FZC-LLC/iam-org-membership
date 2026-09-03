package http

import (
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/gin-gonic/gin"
)

// newErrorResponse builds a fully-populated ErrorResponse (§17). The wire
// shape mirrors platform-gincommon.ErrorResponse and the sibling
// iam-user-profile2 service — flat, with both `error`+`code` (the LLD's
// canonical field name) and `request_id`+`trace_id` for cross-service
// correlation. Callers set Status separately when invoking
// AbortWithStatusJSON. c may be nil in tests; trace_id and request_id are
// then omitted. `details` is currently only populated by 422 validation
// paths (future handlers); kept in the signature for parity with the
// sibling service.
//
//nolint:unparam // details is threaded for future 422 validation callers
func newErrorResponse(c *gin.Context, code, message string, details []ValidationError) ErrorResponse {
	er := ErrorResponse{
		Error:   code,
		Code:    code,
		Message: message,
		Details: details,
	}
	if c != nil {
		er.TraceID = gincommon.TraceIDFromContext(c)
		if rid := gincommon.RequestIDFromContext(c); rid != "" {
			er.RequestID = rid
		} else if rid := c.GetHeader(gincommon.HeaderRequestID); rid != "" {
			er.RequestID = rid
		} else if rid := c.Writer.Header().Get(gincommon.HeaderRequestIDResponse); rid != "" {
			er.RequestID = rid
		}
	}
	return er
}
