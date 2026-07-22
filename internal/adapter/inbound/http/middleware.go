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

		// "iam-system" is the reserved system principal used by internal
		// callers (Realm Provisioner, Event Consumer). It is intentionally
		// not a UUID; the typed RequestContext stores uuid.Nil and handlers
		// check HasRole("iam-system"). Any other non-UUID user ID is a
		// misconfigured gateway → 401.
		var userID uuid.UUID
		if platformRc.UserID == "iam-system" {
			userID = uuid.Nil
		} else {
			id, err := uuid.Parse(platformRc.UserID)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
					"code":    "missing_identity_headers",
					"message": "x-user-id header is not a valid UUID",
				})
				return
			}
			userID = id
		}
		tenantID, err := uuid.Parse(platformRc.TenantID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    "missing_identity_headers",
				"message": "x-tenant-id header is not a valid UUID",
			})
			return
		}

		rc := &requestctx.RequestContext{
			UserID:    userID,
			TenantID:  tenantID,
			Roles:     platformRc.Roles,
			ClientIP:  platformRc.ClientIP,
			UserAgent: c.Request.Header.Get("User-Agent"),
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
			c.AbortWithStatusJSON(http.StatusUnsupportedMediaType, gin.H{
				"code":    "unsupported_media_type",
				"message": "Content-Type must be application/json",
			})
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
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"code":    "insufficient_role",
				"message": "internal route requires iam-system role",
			})
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
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"code":    "insufficient_role",
				"message": "operator route requires platform_operator role",
			})
			return
		}
		c.Next()
	}
}

// RegisterValidators wires custom Gin validators (slug regex, keycloak
// group name, BCP-47 locale, mfa_freshness range). Phase 0 leaves the set
// empty — Phase 2 populates it alongside the first DTOs. Called from
// main.go before router construction.
func RegisterValidators() {
	// Phase 2 lands validators via github.com/go-playground/validator/v10.
}

// HandleError writes a JSON error response derived from err. Recognises
// *domain.DomainError and maps its Code to an HTTP status per §17.
func HandleError(c *gin.Context, err error) {
	var de *domain.DomainError
	if errors.As(err, &de) {
		status := domainErrorStatus(de)
		body := gin.H{"code": de.Code, "message": de.Message}
		for k, v := range de.Details {
			body[k] = v
		}
		c.AbortWithStatusJSON(status, body)
		return
	}
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
		"code":    "internal_error",
		"message": "an unexpected error occurred",
	})
}

// domainErrorStatus maps a DomainError to its HTTP status per LLD §17.
func domainErrorStatus(de *domain.DomainError) int {
	switch {
	case errors.Is(de.Cause, domain.ErrValidation):
		return http.StatusBadRequest
	case errors.Is(de.Cause, domain.ErrMissingIdentity):
		return http.StatusUnauthorized
	case errors.Is(de.Cause, domain.ErrInsufficientRole):
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
		errors.Is(de.Cause, domain.ErrTenantOffboarded):
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
