package http

import (
	"net/http"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/problem"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// TenantMismatchType is the RFC 9457 type returned in strict mode when the
// path tenant and x-tenant-id disagree ({base}/iam/tenant-mismatch).
var TenantMismatchType = problem.MustType("iam", "tenant-mismatch",
	"Tenant mismatch", http.StatusBadRequest, "TENANT_MISMATCH")

// PathTenantConfig configures BindPathTenant.
type PathTenantConfig struct {
	// Strict rejects a request whose x-tenant-id differs from the path
	// tenant with 400 TenantMismatchType instead of overriding the header
	// (STRICT_TENANT_PATH_VALIDATION / Helm iam.strictTenantPathValidation).
	Strict bool
	// Logger receives the mismatch warning. Optional.
	Logger gincommon.Logger
}

// BindPathTenant makes the :id path segment the tenant for every
// /api/v1/internal/tenants/:id/* route. Repositories filter on the path
// tenant while RLS filters on app.tenant_id, which GUCBridgeMiddleware took
// from x-tenant-id; when the two disagreed a read found no rows (false 404 /
// active:false), a cache hit and a miss disagreed, and a write failed the
// RLS WITH CHECK and counted as an RLS violation. This rebinds app.tenant_id
// and the RequestContext tenant to the path tenant; the caller's user, roles,
// client IP and user agent are left as they were.
//
// A mismatch is logged and counted (iam_org_membership_tenant_header_path_
// mismatch_total). In strict mode it is rejected before the handler runs.
//
// Register it only on the /tenants/:id group: on other internal routes :id
// is not a tenant (I-8's /users/:id is a user ID).
func BindPathTenant(cfg PathTenantConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		pathTenant, err := parsePathTenant(c.Param("id"))
		if err != nil {
			HandleError(c, err)
			c.Abort()
			return
		}
		ctx := c.Request.Context()
		rc, ok := requestctx.FromContext(ctx)
		if ok && rc.TenantID != pathTenant {
			action := metrics.TenantMismatchOverridden
			if cfg.Strict {
				action = metrics.TenantMismatchRejected
			}
			metrics.IncTenantHeaderPathMismatch(c.FullPath(), c.Request.Method, action)
			if cfg.Logger != nil {
				cfg.Logger.Warn("tenant_header_path_mismatch", map[string]interface{}{
					"route":            c.FullPath(),
					"method":           c.Request.Method,
					"request_id":       gincommon.RequestIDFromContext(c),
					"caller_user_id":   rc.UserID.String(),
					"caller_roles":     rc.Roles,
					"path_tenant_id":   pathTenant.String(),
					"header_tenant_id": rc.TenantID.String(),
					"action":           action,
				})
			}
			if cfg.Strict {
				gincommon.AbortWithProblem(c, problem.NewError(TenantMismatchType,
					"x-tenant-id does not match the tenant in the request path"))
				return
			}
		}
		ctx = requestctx.WithTenant(ctx, pathTenant)
		if ok {
			bound := *rc
			bound.TenantID = pathTenant
			ctx = requestctx.WithContext(ctx, &bound)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// parsePathTenant validates the :id segment as a UUID — the same rule and
// 400 VALIDATION_ERROR/invalid_uuid the handlers' parseTenantIDParam applies.
func parsePathTenant(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, domain.NewError(domain.ErrValidation, "tenant id in path is not a valid UUID").
			WithDetails(map[string]any{"code": "invalid_uuid"})
	}
	return id, nil
}
