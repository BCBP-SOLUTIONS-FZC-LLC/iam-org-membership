// Package http implements the inbound HTTP surface: Gin handlers, DTOs,
// and the middleware chain that binds gateway-injected identity into the
// request context and the DB pool's RLS GUC.
package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
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
// Applies ONLY to public routes (/api/v1/tenants/*, /api/v1/delegations/*).
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
			c.Next() // real DB error — let downstream surface it
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
// same /api/v1/tenants/* and /api/v1/delegations/* route groups.
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
		if m.Status == domain.MembershipSuspended {
			er := newErrorResponse(c, "insufficient_role",
				"suspended members cannot access tenant APIs", nil)
			er.Status = http.StatusForbidden
			c.AbortWithStatusJSON(http.StatusForbidden, er)
			return
		}
		c.Next()
	}
}

// NormalizeAuthErrors intercepts 401 responses from platform-gincommon's auth
// middleware and rewrites them to match our standard error envelope (LLD §17,
// G-13). The library writes {"error":"missing or invalid...","status":401}
// without a "code" field; this middleware adds code=missing_identity_headers
// so all 401s have a consistent shape.
func NormalizeAuthErrors() gin.HandlerFunc {
	return func(c *gin.Context) {
		buf := &bufferedWriter{ResponseWriter: c.Writer, buf: &bytes.Buffer{}}
		c.Writer = buf
		c.Next()
		if buf.status == http.StatusUnauthorized {
			var raw map[string]any
			if err := json.Unmarshal(buf.buf.Bytes(), &raw); err == nil {
				if _, hasCode := raw["code"]; !hasCode {
					raw["code"] = "missing_identity_headers"
					raw["error"] = "missing_identity_headers"
					rewritten, _ := json.Marshal(raw)
					buf.ResponseWriter.Header().Set("Content-Type", "application/json; charset=utf-8")
					buf.ResponseWriter.WriteHeader(http.StatusUnauthorized)
					_, _ = buf.ResponseWriter.Write(rewritten)
					return
				}
			}
		}
		if buf.status != 0 {
			buf.ResponseWriter.WriteHeader(buf.status)
		}
		_, _ = buf.ResponseWriter.Write(buf.buf.Bytes())
	}
}

type bufferedWriter struct {
	gin.ResponseWriter
	buf    *bytes.Buffer
	status int
}

func (w *bufferedWriter) WriteHeader(code int) { w.status = code }
func (w *bufferedWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.buf.Write(b)
}
func (w *bufferedWriter) Status() int {
	if w.status == 0 {
		return w.ResponseWriter.Status()
	}
	return w.status
}
func (w *bufferedWriter) Written() bool { return w.buf.Len() > 0 || w.status != 0 }
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

// RegisterValidators wires custom Gin validators (slug regex, keycloak
// group name, BCP-47 locale, mfa_freshness range). Phase 0 leaves the set
// empty — Phase 2 populates it alongside the first DTOs. Called from
// main.go before router construction. Safe to call multiple times.
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
	// layer. SQLSTATE class 08 (connection exception) and 53 (insufficient
	// resources) are genuine DB-availability failures → 503 db_unavailable
	// per LLD §17 (line 2544). All other classes (constraint violations,
	// syntax errors, etc.) are surfaced as a plain 500 — those should
	// have been translated to DomainErrors by the repository layer before
	// reaching here.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if isDBUnavailableSQLState(pgErr.Code) {
			er := newErrorResponse(c, domain.ErrDBUnavailable.Error(), "database unavailable", nil)
			er.Status = http.StatusServiceUnavailable
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, er)
			return
		}
	}
	log.Printf("[DEBUG] unhandled 500 error type=%T value=%v", err, err)
	er := newErrorResponse(c, "internal_error", "an unexpected error occurred", nil)
	er.Status = http.StatusInternalServerError
	c.AbortWithStatusJSON(http.StatusInternalServerError, er)
}

// isDBUnavailableSQLState returns true for SQLSTATE classes that indicate
// a connectivity or resource-exhaustion failure rather than a logic error.
// See https://www.postgresql.org/docs/current/errcodes-appendix.html.
//
//	Class 08 — connection_exception (connection lost, server gone)
//	Class 53 — insufficient_resources (too many connections, out of memory)
//	Class 57 — operator_intervention (admin forced disconnect)
//	Class 58 — system_error (I/O or undefined error at the OS level)
func isDBUnavailableSQLState(code string) bool {
	if len(code) < 2 {
		return false
	}
	switch strings.ToUpper(code[:2]) {
	case "08", "53", "57", "58":
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
		errors.Is(de.Cause, domain.ErrTenantReadOnly):
		return http.StatusForbidden
	case errors.Is(de.Cause, domain.ErrTenantNotFound),
		errors.Is(de.Cause, domain.ErrMemberNotFound),
		errors.Is(de.Cause, domain.ErrDepartmentNotFound),
		errors.Is(de.Cause, domain.ErrDelegationNotFound),
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
		errors.Is(de.Cause, domain.ErrRoleAlreadyGranted),
		errors.Is(de.Cause, domain.ErrACLAlreadyExists):
		return http.StatusConflict
	case errors.Is(de.Cause, domain.ErrReinviteTooSoon),
		errors.Is(de.Cause, domain.ErrInviteRateLimited):
		return http.StatusTooManyRequests
	case errors.Is(de.Cause, domain.ErrDBUnavailable),
		errors.Is(de.Cause, domain.ErrCacheUnavailable),
		errors.Is(de.Cause, domain.ErrUserProfileUnavailable),
		errors.Is(de.Cause, domain.ErrWorkflowServiceUnavailable),
		errors.Is(de.Cause, domain.ErrRealmProvisionerUnavailable),
		errors.Is(de.Cause, domain.ErrCatalogServiceUnavailable),
		errors.Is(de.Cause, domain.ErrDependencyUnavailable):
		return http.StatusServiceUnavailable
	default:
		// Remaining domain codes are 422 domain-rule violations.
		return http.StatusUnprocessableEntity
	}
}
