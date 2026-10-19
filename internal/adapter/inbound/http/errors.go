// Package http — error handling is fully delegated to platform-gincommon's
// RFC 9457 implementation. All error responses are written via
// gincommon.AbortWithProblem; the legacy newErrorResponse helper has been
// removed as part of the RFC 9457 adoption.
package http
