// Package http implements the inbound HTTP surface: Gin handlers, DTOs,
// and the middleware chain that binds gateway-injected identity into the
// request context and the DB pool's RLS GUC.
package http

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"strings"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/problem"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

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
// requestctx.RequestContext. Returns a non-nil *problem.Error (and empty
// rc) if either identity header is malformed — the caller writes the 401.
//
// Reserved principal strings are mapped to fixed UUIDs instead of being
// parsed as ordinary UUIDs (RLS-5/IAPI-2):
//   - "iam-system"        → uuid.Nil   (IamSystemSentinelUUID)
//   - "commercial-system" → CommercialSystemUUID (CD-19)
func parseBridgedIdentity(in bridgedIdentity) (*requestctx.RequestContext, *problem.Error) {
	var userID uuid.UUID
	switch in.UserIDStr {
	case requestctx.IamSystemPrincipal:
		userID = uuid.Nil
	case requestctx.CommercialSystemPrincipal:
		userID = requestctx.CommercialSystemUUID
	default:
		id, err := uuid.Parse(in.UserIDStr)
		if err != nil {
			return nil, problem.NewUnauthorizedError("x-user-id header is not a valid UUID")
		}
		userID = id
	}
	tenantID, err := uuid.Parse(in.TenantIDStr)
	if err != nil {
		return nil, problem.NewUnauthorizedError("x-tenant-id header is not a valid UUID")
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
		rc, errProb := parseBridgedIdentity(bridgedIdentity{
			UserIDStr:   platformRc.UserID,
			TenantIDStr: platformRc.TenantID,
			Roles:       platformRc.Roles,
			ClientIP:    platformRc.ClientIP,
			UserAgent:   c.Request.Header.Get("User-Agent"),
		})
		if errProb != nil {
			gincommon.AbortWithProblem(c, errProb)
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
			gincommon.AbortWithProblem(c, problem.NewError(problem.TypeUnsupportedMediaType, "Content-Type must be application/json"))
			return
		}
		c.Next()
	}
}

// RequireSystemRole gates /api/v1/internal/* to the reserved iam-system
// principal (RLS-5, IAPI-2, AUTH-5). NetworkPolicy is the primary defence;
// this middleware is defense-in-depth.
//
// Deprecated: prefer RequireInternalCaller for the internal group, which also
// accepts commercial-system on the three CD-19 routes. RequireSystemRole is
// kept for any caller that explicitly wants iam-system-only access.
func RequireSystemRole() gin.HandlerFunc {
	return func(c *gin.Context) {
		rc, ok := requestctx.FromContext(c.Request.Context())
		if !ok || !rc.HasRole(requestctx.IamSystemPrincipal) {
			gincommon.AbortWithProblem(c, problem.NewForbiddenError("internal route requires iam-system role"))
			return
		}
		c.Next()
	}
}

// commercialSystemAllowedRoutes is the explicit per-route allow-list for the
// commercial-system principal (CD-19). Key: "METHOD /full/gin/route/template".
// Gin's c.FullPath() returns the registered route pattern with :param
// placeholders, so the key format is stable regardless of actual path values.
// Any commercial-system caller that hits a route NOT in this map receives 403.
var commercialSystemAllowedRoutes = map[string]bool{
	"GET /api/v1/internal/tenants/:id/seat-usage":        true, // I-11 — Billing pre-check
	"GET /api/v1/internal/tenants/:id/plan-context":      true, // I-17 — Entitlements re-read
	"GET /api/v1/internal/tenants/:id/plan-change-check": true, // I-18 — Billing downgrade guard
}

// RequireInternalCaller gates /api/v1/internal/* routes. It accepts:
//   - iam-system principal on every internal route (existing AUTH-5 behaviour)
//   - commercial-system principal on the three CD-19 routes only
//     (I-11, I-17, I-18 — see commercialSystemAllowedRoutes)
//
// Any other caller, or a commercial-system caller on a non-allowed route,
// receives 403 insufficient_role. NetworkPolicy + Istio AuthorizationPolicy
// are the primary defence; this middleware is application-layer defence-in-depth.
func RequireInternalCaller() gin.HandlerFunc {
	return func(c *gin.Context) {
		rc, ok := requestctx.FromContext(c.Request.Context())
		if !ok {
			gincommon.AbortWithProblem(c, problem.NewForbiddenError("internal route requires iam-system role"))
			return
		}
		if rc.IsSystem() {
			// iam-system: unrestricted access to all internal routes.
			c.Next()
			return
		}
		if rc.IsCommercialSystem() {
			// commercial-system: allowed only on the explicit per-route list.
			key := c.Request.Method + " " + c.FullPath()
			if commercialSystemAllowedRoutes[key] {
				c.Next()
				return
			}
			gincommon.AbortWithProblem(c, problem.NewForbiddenError("commercial-system is not authorised on this internal route"))
			return
		}
		gincommon.AbortWithProblem(c, problem.NewForbiddenError("internal route requires iam-system role"))
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
				gincommon.AbortWithProblem(c, problem.NewForbiddenError("caller is not an active member of this tenant"))
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
			gincommon.AbortWithProblem(c, problem.NewForbiddenError(msg))
			return
		}
		c.Next()
	}
}

// RequireOperatorRole gates /api/v1/operator/* (AUTH-6). Every operator
// route re-checks this before any DB access — the header is hardened by
// gateway hygiene (AUTH-7).
func RequireOperatorRole() gin.HandlerFunc {
	return func(c *gin.Context) {
		rc, ok := requestctx.FromContext(c.Request.Context())
		if !ok || !rc.IsOperator() {
			gincommon.AbortWithProblem(c, problem.NewForbiddenError("operator route requires platform_operator role"))
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

// HandleError writes an RFC 9457 problem response derived from err.
// Recognises *problem.Error (passed through), *domain.DomainError (mapped
// to the appropriate platform problem type per §17), and raw pgconn/context
// errors (classified to 503/409). All error responses are written as
// application/problem+json via gincommon.AbortWithProblem.
//
// When a *domain.DomainError carries a non-empty Details map, the response
// is written via abortWithDetailedProblem, which merges those fields as
// RFC 9457 §3.1 extension members (e.g. active_workflows, licensed_seats).
func HandleError(c *gin.Context, err error) {
	if leaveToRequestContext(c, err) {
		return
	}
	var de *domain.DomainError
	if errors.As(err, &de) && len(de.Details) > 0 {
		abortWithDetailedProblem(c, domainErrorToProblem(de), de.Details)
		return
	}
	gincommon.AbortWithProblem(c, toDomainProblem(err))
}

// abortWithDetailedProblem writes an RFC 9457 problem response extended with
// domain-level fields from details (RFC 9457 §3.1 extension members). Used
// when a DomainError carries structured context that API clients depend on
// (e.g. active_workflows, licensed_seats, record_version, retry_after_seconds).
func abortWithDetailedProblem(c *gin.Context, pe *problem.Error, details map[string]any) {
	if c.Writer.Written() {
		c.Abort()
		return
	}
	traceID := problem.ResolveTraceID(
		c.GetHeader("traceparent"),
		c.GetHeader("x-request-id"),
		"")
	p := problem.BuilderFor(pe).
		Instance(c.Request.URL.Path).
		TraceID(traceID).
		Build()

	// Marshal standard problem to a map, then overlay extension members.
	var base map[string]any
	raw, _ := json.Marshal(p)
	_ = json.Unmarshal(raw, &base)
	maps.Copy(base, details)
	body, _ := json.Marshal(base)
	c.Abort()
	c.Data(p.Status, problem.ContentType, body)
}

// toDomainProblem converts any service error to the correct *problem.Error
// for RFC 9457 rendering. *problem.Error passes through; *domain.DomainError
// is mapped via domainErrorToProblem; pg errors are classified by SQLSTATE;
// anything else is returned as-is for gincommon's FromError fallback (which
// maps DeadlineExceeded→503, MaxBytesError→413, else→500).
func toDomainProblem(err error) error {
	var pe *problem.Error
	if errors.As(err, &pe) {
		return pe
	}

	var de *domain.DomainError
	if errors.As(err, &de) {
		return domainErrorToProblem(de)
	}

	// Leaked pgconn.PgError not wrapped by the service layer — classify by
	// SQLSTATE family so inbound HTTP never imports pgconn directly.
	if pgcommon.IsConnectionException(err) || pgcommon.IsInsufficientResources(err) ||
		pgcommon.IsOperatorIntervention(err) || pgcommon.IsSystemError(err) || pgcommon.IsPoolClosed(err) {
		return problem.NewServiceUnavailableError("database unavailable")
	}
	if pgcommon.IsQueryCanceled(err) || pgcommon.IsLockNotAvailable(err) {
		return problem.NewServiceUnavailableError("database statement or lock timeout")
	}
	if pgcommon.IsDeadlock(err) || pgcommon.IsSerializationFailure(err) {
		return problem.NewConflictError("concurrent update conflict — retry the request")
	}

	// context.Canceled: client disconnected or a non-request-context deadline
	// (e.g. a statement timeout) surfaced as Canceled. Map to 503 so it's
	// never misread as a server fault. DeadlineExceeded is already handled by
	// gincommon.FromError; Canceled is not, so we classify it here.
	if errors.Is(err, context.Canceled) {
		return problem.NewServiceUnavailableError("request canceled")
	}

	// Unclassified: pass to gincommon (DeadlineExceeded→503, else→500 no-detail).
	return err
}

// domainErrorToProblem maps a *domain.DomainError to the corresponding
// platform problem type per LLD §17. The domain message becomes the RFC 9457
// detail field; status is determined by the error cause. The domain error
// code (e.g. "tenant_not_found") is surfaced as an uppercase errorCode
// override (e.g. "TENANT_NOT_FOUND") so clients can branch on specific
// business errors without parsing human-readable detail strings.
func domainErrorToProblem(de *domain.DomainError) *problem.Error {
	msg := de.Message
	// Convert the domain sentinel's lowercase_snake code (e.g. "tenant_not_found")
	// to the RFC 9457 / platform-standard UPPER_SNAKE errorCode.
	code := strings.ToUpper(de.Code)
	switch {
	case errors.Is(de.Cause, domain.ErrValidation),
		errors.Is(de.Cause, domain.ErrNoMutableField),
		errors.Is(de.Cause, domain.ErrInvalidTargetPlan):
		return problem.NewBadRequestError(msg).WithCode(code)
	case errors.Is(de.Cause, domain.ErrMissingIdentity):
		return problem.NewUnauthorizedError(msg).WithCode(code)
	case errors.Is(de.Cause, domain.ErrInsufficientRole),
		errors.Is(de.Cause, domain.ErrCannotRemoveOwner),
		errors.Is(de.Cause, domain.ErrTenantTrialExpired),
		errors.Is(de.Cause, domain.ErrTenantSuspended),
		errors.Is(de.Cause, domain.ErrTenantReadOnly),
		errors.Is(de.Cause, domain.ErrServiceAccountNotGrantable):
		return problem.NewForbiddenError(msg).WithCode(code)
	case errors.Is(de.Cause, domain.ErrTenantNotFound),
		errors.Is(de.Cause, domain.ErrMemberNotFound),
		errors.Is(de.Cause, domain.ErrDepartmentNotFound),
		errors.Is(de.Cause, domain.ErrInvitationNotFound),
		errors.Is(de.Cause, domain.ErrPlanNotFound):
		return problem.NewNotFoundError(msg).WithCode(code)
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
		return problem.NewConflictError(msg).WithCode(code)
	case errors.Is(de.Cause, domain.ErrReinviteTooSoon),
		errors.Is(de.Cause, domain.ErrInviteRateLimited):
		return problem.NewRateLimitError(msg).WithCode(code)
	case errors.Is(de.Cause, domain.ErrDBUnavailable),
		errors.Is(de.Cause, domain.ErrCacheUnavailable),
		errors.Is(de.Cause, domain.ErrWorkflowServiceUnavailable),
		errors.Is(de.Cause, domain.ErrRealmProvisionerUnavailable),
		errors.Is(de.Cause, domain.ErrRequestTimeout),
		errors.Is(de.Cause, domain.ErrCatalogUnavailable),
		errors.Is(de.Cause, domain.ErrGroupMappingUnavailable),
		errors.Is(de.Cause, domain.ErrDependencyUnavailable):
		return problem.NewServiceUnavailableError(msg).WithCode(code)
	default:
		// Remaining domain codes are 422 business-rule violations (§17).
		return problem.NewBusinessRuleError(msg).WithCode(code)
	}
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
