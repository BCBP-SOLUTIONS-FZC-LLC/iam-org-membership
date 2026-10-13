// Package http implements the inbound HTTP surface: Gin handlers, DTOs,
// and the middleware chain that binds gateway-injected identity into the
// request context and the DB pool's RLS GUC.
package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// errorLogger is the shared gincommon-backed Logger, set once by NewRouter
// from RouterConfig.GinConfig.Logger. HandleError has ~150 call sites across
// every handler file, so threading a logger through as an explicit
// parameter would touch all of them; a package-level var set at router
// construction time reaches the one bypass site (the unhandled-500 branch
// below) without that churn. nil-safe — tests that build handlers without
// going through NewRouter simply get no logging here, same as before.
var errorLogger port.Logger

// bridgedIdentity is the primitive-typed view of the gateway-injected
// identity used by the bridge helper. Keeping this decoupled from the
// gincommon.RequestContext type lets the parsing/validation logic be
// unit-tested without depending on gincommon's internal-package type.
type bridgedIdentity struct {
	UserIDStr   string
	TenantIDStr string
	Roles       []string
	ClientIP    string
	UserAgent   string
}

// parseBridgedIdentity turns primitive gateway header values into a typed
// requestctx.RequestContext. Returns a non-nil ErrorResponse (and empty
// rc) if either identity header is malformed — the caller writes the 401.
// "iam-system" user id is honored specially per RLS-5/IAPI-2.
func parseBridgedIdentity(in bridgedIdentity) (*requestctx.RequestContext, *ErrorResponse) {
	var userID uuid.UUID
	if in.UserIDStr == "iam-system" {
		userID = uuid.Nil
	} else {
		id, err := uuid.Parse(in.UserIDStr)
		if err != nil {
			return nil, &ErrorResponse{
				Error: "missing_identity_headers", Code: "missing_identity_headers",
				Message: "x-user-id header is not a valid UUID",
				Status:  http.StatusUnauthorized,
			}
		}
		userID = id
	}
	tenantID, err := uuid.Parse(in.TenantIDStr)
	if err != nil {
		return nil, &ErrorResponse{
			Error: "missing_identity_headers", Code: "missing_identity_headers",
			Message: "x-tenant-id header is not a valid UUID",
			Status:  http.StatusUnauthorized,
		}
	}
	return &requestctx.RequestContext{
		UserID: userID, TenantID: tenantID,
		Roles: in.Roles, ClientIP: in.ClientIP, UserAgent: in.UserAgent,
	}, nil
}

// GUCBridgeMiddleware runs after gincommon.ProtectedMiddlewares. It parses
// the gateway-injected identity into typed uuid.UUID values, stores a
// requestctx.RequestContext for handlers, and writes pgcommon.GUCSet so
// every checked-out connection binds `SET LOCAL app.tenant_id` (and
// user_id / tenant_roles) inside the transaction (RLS-6). A session-scoped
// SET would leak across pooled backends and defeat RLS.
func GUCBridgeMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		platformRc, ok := gincommon.RequestContext(c)
		if !ok {
			c.Next()
			return
		}
		rc, errResp := parseBridgedIdentity(bridgedIdentity{
			UserIDStr:   platformRc.UserID,
			TenantIDStr: platformRc.TenantID,
			Roles:       platformRc.Roles,
			ClientIP:    platformRc.ClientIP,
			UserAgent:   c.Request.Header.Get("User-Agent"),
		})
		if errResp != nil {
			// Enrich with trace/request IDs then abort.
			er := newErrorResponse(c, errResp.Code, errResp.Message, nil)
			er.Status = errResp.Status
			c.AbortWithStatusJSON(errResp.Status, er)
			return
		}
		ctx := requestctx.WithContext(c.Request.Context(), rc)
		// x-request-id rides on the plain context so outbound clients
		// (httpx transport) forward it to the services this request calls.
		ctx = requestctx.WithRequestID(ctx, gincommon.RequestIDFromContext(c))

		// Bridge into pgcommon so the pool checkout hook emits `SET LOCAL
		// app.tenant_id` on every transaction (writes and reads). Both IDs
		// are already parsed into uuid.UUID above, so WithGUCSet (not
		// WithValidatedGUCSet) is the right choice — re-validation would
		// duplicate work with no additional safety.
		g, _ := pgcommon.GUCSetFromContext(ctx)
		g.UserID = platformRc.UserID
		g.TenantID = platformRc.TenantID
		g.TenantRoles = platformRc.Roles
		ctx = pgcommon.WithGUCSet(ctx, g)

		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// RequireJSONContentType rejects POST/PUT/PATCH requests without
// application/json to guard against form-encoded posts hitting mutation
// endpoints. Status is 415 (protocol-level) — distinct from the 422
// domain-rule `invalid_content_type` code emitted by body validators.
func RequireJSONContentType() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
		default:
			c.Next()
			return
		}
		if c.Request.ContentLength == 0 {
			c.Next()
			return
		}
		if c.ContentType() != "application/json" {
			er := newErrorResponse(c, "unsupported_media_type", "Content-Type must be application/json", nil)
			er.Status = http.StatusUnsupportedMediaType
			c.AbortWithStatusJSON(http.StatusUnsupportedMediaType, er)
			return
		}
		c.Next()
	}
}

// RequireSystemRole gates /api/v1/internal/* to the reserved iam-system
// principal (RLS-5, IAPI-2, AUTH-5). NetworkPolicy is the primary defence;
// this middleware is defense-in-depth.
func RequireSystemRole() gin.HandlerFunc {
	return func(c *gin.Context) {
		rc, ok := requestctx.FromContext(c.Request.Context())
		if !ok || !rc.HasRole("iam-system") {
			er := newErrorResponse(c, "insufficient_role", "internal route requires iam-system role", nil)
			er.Status = http.StatusForbidden
			c.AbortWithStatusJSON(http.StatusForbidden, er)
			return
		}
		c.Next()
	}
}

// RequireActiveTenant is the TRIAL-4 / §16 A53 defense-in-depth gate on the
// public tenant-facing API. The workflow docs
// (trial-subscription-end-to-end-workflow §106, trial-expiry-cleanup-workflow
// §26) are unambiguous: a `trial_expired` tenant has "no session, no read,
// no export, no API access" — enforcement is nominally at Keycloak (RP sets
// enabled=false on every user when TrialExpired lands), but this middleware
// makes the guarantee independent of that upstream: if a JWT slips through
// (long TTL, RP session-revoke failure, misconfigured realm), the service
// still refuses.
//
// Applies ONLY to public routes (/api/v1/tenants/*).
// Skipped for iam-system (internal/consumer/reconciler paths that need to
// mutate a trial_expired tenant to restore it) and platform_operator (O-7
// reassign-owner, O-4 feature-flags, etc — operators must retain access to
// recover a tenant in any lifecycle state).
//
// Status → verdict:
//   - trial_expired   → 403 tenant_trial_expired   (TRIAL-4, all methods)
//   - suspended       → 403 tenant_suspended       (all methods)
//   - offboarded      → 404 tenant_not_found       (row is soft-deleted anyway)
//   - cancelled       → 403 tenant_read_only       (writes only; reads pass, §16 A53)
//   - trial/active/past_due → proceed
func RequireActiveTenant(tenants port.TenantRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc, ok := requestctx.FromContext(c.Request.Context())
		if !ok {
			c.Next() // no identity — let GUCBridge / auth middleware surface the 401
			return
		}
		// Bypass system + operator principals. Both are internal control
		// paths that must reach a trial_expired tenant to restore it.
		if rc.HasRole("iam-system") || rc.IsOperator() {
			c.Next()
			return
		}
		t, err := tenants.FindByID(c.Request.Context(), rc.TenantID)
		if err != nil {
			if errors.Is(err, domain.ErrTenantNotFound) {
				HandleError(c, domain.NewError(domain.ErrTenantNotFound, "tenant not found"))
				return
			}
			// SECURITY-7: fail closed on unexpected DB error — a transient
			// connection error must not let a cancelled/suspended tenant through.
			HandleError(c, domain.NewError(domain.ErrDBUnavailable, "database unavailable — please retry"))
			return
		}
		method := c.Request.Method
		isWrite := method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions

		switch t.Status {
		case domain.StatusTrialExpired:
			HandleError(c, domain.NewError(domain.ErrTenantTrialExpired,
				"tenant trial has expired — no API access; reactivate via the emailed link (TRIAL-4)"))
			return
		case domain.StatusSuspended:
			HandleError(c, domain.NewError(domain.ErrTenantSuspended, "tenant is suspended"))
			return
		case domain.StatusOffboarded:
			HandleError(c, domain.NewError(domain.ErrTenantNotFound, "tenant not found"))
			return
		case domain.StatusCancelled:
			if isWrite {
				HandleError(c, domain.NewError(domain.ErrTenantReadOnly,
					"cancelled tenant is read-only (§16 A53)"))
				return
			}
		case domain.StatusTrial, domain.StatusActive, domain.StatusPastDue:
			// active lifecycle states — allow through
		}
		c.Next()
	}
}

// RequireActiveMembership blocks callers whose tenant_membership status is
// suspended. The gateway header only carries the caller's roles — it does not
// carry the DB-level membership status — so a suspended member can still send
// a matching x-tenant-id and reach the handler. This middleware closes that
// gap by doing a direct membership lookup for public routes.
//
// Bypass: iam-system and platform_operator callers are never membership-checked
// (they have no tenant_memberships row to look up and must retain access to
// recover tenants in any state). Applied after RequireActiveTenant on the
// same /api/v1/tenants/* route group.
func RequireActiveMembership(memberships port.MembershipRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc, ok := requestctx.FromContext(c.Request.Context())
		if !ok {
			c.Next()
			return
		}
		if rc.HasRole("iam-system") || rc.IsOperator() {
			c.Next()
			return
		}
		m, err := memberships.FindByUserID(c.Request.Context(), rc.TenantID, rc.UserID)
		if err != nil {
			if errors.Is(err, domain.ErrMemberNotFound) {
				// No active membership row (left/never joined) → not an active member
				er := newErrorResponse(c, "insufficient_role",
					"caller is not an active member of this tenant", nil)
				er.Status = http.StatusForbidden
				c.AbortWithStatusJSON(http.StatusForbidden, er)
				return
			}
			// Real DB error — let the handler surface it naturally
			c.Next()
			return
		}
		if m.Status != domain.MembershipActive {
			// AUTH-1: only active members access tenant APIs.
			// Left members retain a non-deleted row but are no longer active.
			msg := "member is not an active member of this tenant"
			if m.Status == domain.MembershipSuspended {
				msg = "suspended members cannot access tenant APIs"
			}
			er := newErrorResponse(c, "insufficient_role", msg, nil)
			er.Status = http.StatusForbidden
			c.AbortWithStatusJSON(http.StatusForbidden, er)
			return
		}
		c.Next()
	}
}

// NormalizeAuthErrors rewrites the error bodies platform-gincommon's own
// middlewares write inside it — RequireAuth's 401 and ContextMiddleware's
// 400 — to this service's envelope (LLD §17, G-13). The library writes
// {"error":"...","status":N} without a "code"; this adds one:
//   - 401: ambiguous_identity_headers when an identity header was sent more
//     than once, missing_identity_headers otherwise;
//   - 400: invalid_request_context.
//
// A body that already has a "code" (every HandleError response) is left
// alone. Two library responses are written outside this middleware and keep
// gincommon's shape: the request-deadline 503 (TimeoutMiddleware, body
// {"error":"request timeout"}) and the panic-recovery 500.
//
// Only a 400 or 401 is buffered. Any other status passes straight through to
// the real writer, and a handler that writes nothing leaves the response
// unwritten, so gincommon's TimeoutMiddleware can still answer with its 503.
func NormalizeAuthErrors() gin.HandlerFunc {
	return func(c *gin.Context) {
		buf := &bufferedWriter{ResponseWriter: c.Writer, buf: &bytes.Buffer{}}
		c.Writer = buf
		// Hand the real writer back to the outer middlewares (observability,
		// TimeoutMiddleware, panic recovery) however the chain exits — a
		// panicking handler unwinds past the code after c.Next().
		defer func() { c.Writer = buf.ResponseWriter }()
		c.Next()
		if buf.status == 0 {
			return // passed through (or nothing written)
		}
		c.Writer = buf.ResponseWriter
		body := buf.buf.Bytes()
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err == nil {
			if _, hasCode := raw["code"]; !hasCode {
				code := libraryErrorCode(buf.status, raw)
				raw["code"] = code
				raw["error"] = code
				body, _ = json.Marshal(raw)
				buf.ResponseWriter.Header().Set("Content-Type", "application/json; charset=utf-8")
			}
		}
		buf.ResponseWriter.WriteHeader(buf.status)
		if len(body) > 0 {
			_, _ = buf.ResponseWriter.Write(body)
		}
	}
}

// libraryErrorCode is the code for a platform-gincommon error body.
func libraryErrorCode(status int, raw map[string]any) string {
	if status == http.StatusBadRequest {
		return "invalid_request_context"
	}
	return authErrorCode(raw)
}

// authErrorCode classifies a platform-gincommon 401 body by its message.
func authErrorCode(raw map[string]any) string {
	if msg, _ := raw["error"].(string); strings.HasPrefix(msg, "ambiguous ") {
		return "ambiguous_identity_headers"
	}
	return "missing_identity_headers"
}

// bufferedWriter holds back a 400 or 401 so NormalizeAuthErrors can rewrite
// it; every other status is written through to the embedded writer as it
// comes.
type bufferedWriter struct {
	gin.ResponseWriter
	buf         *bytes.Buffer
	status      int  // buffered status (400 or 401 only)
	passthrough bool // another status was set: write straight through
}

// buffered reports whether NormalizeAuthErrors may need to rewrite code.
func buffered(code int) bool {
	return code == http.StatusUnauthorized || code == http.StatusBadRequest
}

func (w *bufferedWriter) WriteHeader(code int) {
	if w.passthrough {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if buffered(code) {
		w.status = code
		return
	}
	w.passthrough = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *bufferedWriter) Write(b []byte) (int, error) {
	if w.passthrough {
		return w.ResponseWriter.Write(b)
	}
	if w.status == 0 {
		// Write without a status: an implicit 200, never a 401.
		w.passthrough = true
		return w.ResponseWriter.Write(b)
	}
	return w.buf.Write(b)
}

func (w *bufferedWriter) Status() int {
	if w.passthrough || w.status == 0 {
		return w.ResponseWriter.Status()
	}
	return w.status
}

func (w *bufferedWriter) Written() bool {
	if w.passthrough {
		return w.ResponseWriter.Written()
	}
	return w.buf.Len() > 0 || w.status != 0
}

func (w *bufferedWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

// RequireOperatorRole gates /api/v1/operator/* (AUTH-6). Every operator
// route re-checks this before any DB access — the header is hardened by
// gateway hygiene (AUTH-7).
func RequireOperatorRole() gin.HandlerFunc {
	return func(c *gin.Context) {
		rc, ok := requestctx.FromContext(c.Request.Context())
		if !ok || !rc.IsOperator() {
			er := newErrorResponse(c, "insufficient_role", "operator route requires platform_operator role", nil)
			er.Status = http.StatusForbidden
			c.AbortWithStatusJSON(http.StatusForbidden, er)
			return
		}
		c.Next()
	}
}

// validatorsRegistered records whether RegisterValidators has been called.
// Idempotency guard so re-invocation (e.g. tests calling into main's setup
// twice) is a safe no-op.
var validatorsRegistered bool

// RegisterValidators is called from main.go before router construction.
// All field constraints (slug regex, BCP-47 locale, mfa_freshness range)
// are enforced in the service layer (provisioning_service.go isValidSlug /
// isValidLocale, tenant_service.go Patch, etc.) rather than at the Gin
// binding layer — service-layer validation is the single source of truth
// and runs regardless of the HTTP entry point. This hook exists as an
// extension point if framework-level binding tags are added in future.
// Safe to call multiple times (idempotent guard below).
func RegisterValidators() {
	validatorsRegistered = true
}

// HandleError writes a JSON error response derived from err. Recognises
// *domain.DomainError and maps its Code to an HTTP status per §17. The
// response body follows the flat ErrorResponse shape (§17, matches
// platform-gincommon.ErrorResponse and the sibling iam-user-profile2
// service) — populates `request_id`/`trace_id` for cross-service
// correlation.
func HandleError(c *gin.Context, err error) {
	if leaveToRequestContext(c, err) {
		return
	}
	var de *domain.DomainError
	if errors.As(err, &de) {
		status := domainErrorStatus(de)
		body := newErrorResponse(c, de.Code, de.Message, nil)
		body.Status = status
		// Merge domain-error details into the flat envelope so 409/422
		// contract fields (record_version, active_workflows, workflow_ids,
		// allowed_actions, licensed_seats, ...) surface at the top level
		// as the DTO declares (dto.go:245).
		mergedBody := errorResponseWithDetails(body, de.Details)
		c.AbortWithStatusJSON(status, mergedBody)
		return
	}
	// Raw pgconn.PgError that was not caught and translated by the service
	// layer. wrapConnErr maps these classes to ErrDBUnavailable, so the
	// DomainError branch above is the production path. This fallback still
	// classifies a leaked PgError (tests, future missed wrap) into 503
	// db_unavailable per LLD §17, using pgcommon's SQLSTATE helpers so
	// inbound HTTP never imports pgconn.
	if pgcommon.IsConnectionException(err) || pgcommon.IsInsufficientResources(err) ||
		pgcommon.IsOperatorIntervention(err) || pgcommon.IsSystemError(err) || pgcommon.IsPoolClosed(err) {
		er := newErrorResponse(c, domain.ErrDBUnavailable.Error(), "database unavailable", nil)
		er.Status = http.StatusServiceUnavailable
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, er)
		return
	}
	// A leaked statement/lock timeout (57014/55P03) or an exhausted
	// deadlock/serialization retry (40P01/40001): the same codes wrapConnErr
	// gives them, never a 500.
	if pgcommon.IsQueryCanceled(err) || pgcommon.IsLockNotAvailable(err) {
		er := newErrorResponse(c, domain.ErrRequestTimeout.Error(), "database statement or lock timeout", nil)
		er.Status = http.StatusServiceUnavailable
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, er)
		return
	}
	if pgcommon.IsDeadlock(err) || pgcommon.IsSerializationFailure(err) {
		er := newErrorResponse(c, domain.ErrConflict.Error(), "concurrent update conflict — retry the request", nil)
		er.Status = http.StatusConflict
		c.AbortWithStatusJSON(http.StatusConflict, errorResponseWithDetails(er, map[string]any{"code": "concurrent_update"}))
		return
	}
	// A request deadline that expired outside a wrapped repository call (or
	// a client that went away) is a timeout, not a server fault.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		er := newErrorResponse(c, domain.ErrRequestTimeout.Error(), "request deadline exceeded", nil)
		er.Status = http.StatusServiceUnavailable
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, er)
		return
	}
	if errorLogger != nil {
		errorLogger.Error("unhandled 500 error", map[string]any{"error_type": fmt.Sprintf("%T", err), "error": err.Error()})
	}
	er := newErrorResponse(c, "internal_error", "an unexpected error occurred", nil)
	er.Status = http.StatusInternalServerError
	c.AbortWithStatusJSON(http.StatusInternalServerError, er)
}

// clientClosedRequest is the de-facto status (nginx's 499) recorded when the
// client went away before a response; nothing reads the body.
const clientClosedRequest = 499

// leaveToRequestContext handles errors caused by the request's own context
// ending, which this handler must not report as its own failure:
//
//   - The request deadline expired (gincommon's Config.RequestTimeout): abort
//     without writing, so gincommon's timeout middleware writes its 503 and
//     counts it on platform_http_request_timeouts_total. Writing a 503 here
//     would mark the response started and the library would skip both.
//   - The client disconnected: record 499 instead of a misleading 5xx.
//
// It only applies when the request context itself is done; a deadline that
// belongs to something else (e.g. a statement_timeout) is still reported by
// HandleError as request_timeout.
func leaveToRequestContext(c *gin.Context, err error) bool {
	if c.Request == nil {
		return false
	}
	switch ctxErr := c.Request.Context().Err(); {
	case ctxErr == nil:
		return false
	case errors.Is(ctxErr, context.DeadlineExceeded) && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, domain.ErrRequestTimeout)):
		c.Abort()
		return true
	case errors.Is(ctxErr, context.Canceled) && (errors.Is(err, context.Canceled) || errors.Is(err, domain.ErrRequestTimeout)):
		c.AbortWithStatus(clientClosedRequest)
		return true
	}
	return false
}

// errorResponseWithDetails renders the flat ErrorResponse envelope with
// any DomainError.Details merged as top-level fields on a marshalable map.
// Using a map preserves the DTO's flat shape while allowing arbitrary
// per-code extras (record_version, active_workflows, ...) without
// enumerating every field on the struct.
func errorResponseWithDetails(er ErrorResponse, details map[string]any) map[string]any {
	out := map[string]any{
		"error":   er.Error,
		"code":    er.Code,
		"status":  er.Status,
		"message": er.Message,
	}
	if er.TraceID != "" {
		out["trace_id"] = er.TraceID
	}
	if er.RequestID != "" {
		out["request_id"] = er.RequestID
	}
	for k, v := range details {
		out[k] = v
	}
	return out
}

// domainErrorStatus maps a DomainError to its HTTP status per LLD §17.
func domainErrorStatus(de *domain.DomainError) int {
	switch {
	case errors.Is(de.Cause, domain.ErrValidation),
		errors.Is(de.Cause, domain.ErrNoMutableField):
		return http.StatusBadRequest
	case errors.Is(de.Cause, domain.ErrMissingIdentity):
		return http.StatusUnauthorized
	case errors.Is(de.Cause, domain.ErrInsufficientRole),
		errors.Is(de.Cause, domain.ErrCannotRemoveOwner),
		errors.Is(de.Cause, domain.ErrTenantTrialExpired),
		errors.Is(de.Cause, domain.ErrTenantSuspended),
		errors.Is(de.Cause, domain.ErrTenantReadOnly),
		errors.Is(de.Cause, domain.ErrServiceAccountNotGrantable):
		return http.StatusForbidden
	case errors.Is(de.Cause, domain.ErrTenantNotFound),
		errors.Is(de.Cause, domain.ErrMemberNotFound),
		errors.Is(de.Cause, domain.ErrDepartmentNotFound),
		errors.Is(de.Cause, domain.ErrInvitationNotFound),
		errors.Is(de.Cause, domain.ErrPlanNotFound):
		return http.StatusNotFound
	case errors.Is(de.Cause, domain.ErrOptimisticLockConflict),
		errors.Is(de.Cause, domain.ErrConflict),
		errors.Is(de.Cause, domain.ErrSlugAlreadyTaken),
		errors.Is(de.Cause, domain.ErrMemberAlreadyExists),
		errors.Is(de.Cause, domain.ErrDeptMembershipAlreadyExists),
		errors.Is(de.Cause, domain.ErrWorkflowResolutionRequired),
		errors.Is(de.Cause, domain.ErrSeatLimitReached),
		errors.Is(de.Cause, domain.ErrInvitationAlreadyExists),
		errors.Is(de.Cause, domain.ErrTenantOffboarded),
		errors.Is(de.Cause, domain.ErrDepartmentAlreadyActivated),
		errors.Is(de.Cause, domain.ErrRoleAlreadyGranted):
		return http.StatusConflict
	case errors.Is(de.Cause, domain.ErrReinviteTooSoon),
		errors.Is(de.Cause, domain.ErrInviteRateLimited):
		return http.StatusTooManyRequests
	case errors.Is(de.Cause, domain.ErrDBUnavailable),
		errors.Is(de.Cause, domain.ErrCacheUnavailable),
		errors.Is(de.Cause, domain.ErrWorkflowServiceUnavailable),
		errors.Is(de.Cause, domain.ErrRealmProvisionerUnavailable),
		errors.Is(de.Cause, domain.ErrRequestTimeout),
		errors.Is(de.Cause, domain.ErrCatalogUnavailable),
		errors.Is(de.Cause, domain.ErrGroupMappingUnavailable),
		errors.Is(de.Cause, domain.ErrDependencyUnavailable):
		return http.StatusServiceUnavailable
	default:
		// Remaining domain codes are 422 domain-rule violations.
		return http.StatusUnprocessableEntity
	}
}
