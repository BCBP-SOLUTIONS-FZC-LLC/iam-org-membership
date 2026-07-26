// Package http implements the inbound HTTP surface: Gin handlers, DTOs,
// and the middleware chain that binds gateway-injected identity into the
// request context and the DB pool's RLS GUC.
package http

import (
	"errors"
	"net/http"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
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
	er := newErrorResponse(c, "internal_error", "an unexpected error occurred", nil)
	er.Status = http.StatusInternalServerError
	c.AbortWithStatusJSON(http.StatusInternalServerError, er)
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
		errors.Is(de.Cause, domain.ErrCannotRemoveOwner):
		return http.StatusForbidden
	case errors.Is(de.Cause, domain.ErrTenantNotFound),
		errors.Is(de.Cause, domain.ErrMemberNotFound),
		errors.Is(de.Cause, domain.ErrDepartmentNotFound),
		errors.Is(de.Cause, domain.ErrDelegationNotFound),
		errors.Is(de.Cause, domain.ErrInvitationNotFound):
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
		errors.Is(de.Cause, domain.ErrUserProfileUnavailable),
		errors.Is(de.Cause, domain.ErrWorkflowServiceUnavailable),
		errors.Is(de.Cause, domain.ErrRealmProvisionerUnavailable),
		errors.Is(de.Cause, domain.ErrDependencyUnavailable):
		return http.StatusServiceUnavailable
	default:
		// Remaining domain codes are 422 domain-rule violations.
		return http.StatusUnprocessableEntity
	}
}
