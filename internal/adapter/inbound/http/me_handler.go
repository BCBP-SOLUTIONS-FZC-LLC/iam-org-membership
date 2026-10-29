package http

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
)

// SessionContextReader is the slice of service.SessionContextService
// MeHandler needs; an interface so the handler is unit-testable with a fake.
type SessionContextReader interface {
	Get(ctx context.Context, tenantID, userID uuid.UUID) (*service.SessionContext, error)
}

// MeHandler serves /api/v1/me/* — reads about the caller themself. The
// caller is identified only by the gateway-injected x-tenant-id/x-user-id
// (bound into requestctx by GUCBridgeMiddleware); these routes take no
// path, query or body identity, so there is no tenant or user to mismatch.
type MeHandler struct {
	svc SessionContextReader
}

// NewMeHandler builds a MeHandler.
func NewMeHandler(svc SessionContextReader) *MeHandler {
	return &MeHandler{svc: svc}
}

// SessionContext is P-35 (LLD §5.4): the signed-in user's own tenant context.
//
// @Summary      P-35 — Read my session context
// @Description  The caller's own tenant context, for the UI: the roles, departments, plan and feature flags AuthZ Enrichment injects as x-tenant-roles / x-departments / x-plan / x-feature-flags (feature_flags includes read_only when the tenant is read-only), plus mfa_freshness_seconds (the step-up window, I-14's source), the tenant locale and the tenant's workspace slug (for /t/<slug>/ routing). No parameters: the caller is identified from the gateway-injected identity headers only. 404 member_not_found when the caller has no membership in x-tenant-id (I-8's answer); 403 for a suspended member and for a trial_expired/suspended tenant (RequireActiveTenant).
// @Tags         me
// @Produce      json
// @Success      200  {object}  SessionContextResponse
// @Failure      401  {object}  ProblemResponse  "Missing/malformed identity headers"
// @Failure      403  {object}  ProblemResponse  "Suspended member, or tenant trial_expired/suspended"
// @Failure      404  {object}  ProblemResponse  "member_not_found / tenant_not_found"
// @Failure      503  {object}  ProblemResponse  "db_unavailable"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /me/session-context [get]
func (h *MeHandler) SessionContext(c *gin.Context) {
	rc, ok := requestctx.FromContext(c.Request.Context())
	if !ok {
		HandleError(c, domain.NewError(domain.ErrMissingIdentity, "missing identity"))
		return
	}
	sc, err := h.svc.Get(c.Request.Context(), rc.TenantID, rc.UserID)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, SessionContextToResponse(sc))
}

// SessionContextToResponse maps the service view to P-35's wire shape.
func SessionContextToResponse(sc *service.SessionContext) SessionContextResponse {
	roles := make([]string, len(sc.Roles))
	for i, r := range sc.Roles {
		roles[i] = string(r)
	}
	depts := make([]SessionContextDepartment, len(sc.Departments))
	for i, d := range sc.Departments {
		depts[i] = SessionContextDepartment{ID: d.DepartmentID, Level: string(d.RoleLevel)}
	}
	flags := sc.FeatureFlags
	if flags == nil {
		flags = []string{}
	}
	return SessionContextResponse{
		TenantID:            sc.TenantID,
		Roles:               roles,
		Departments:         depts,
		Plan:                string(sc.Plan),
		FeatureFlags:        flags,
		ReadOnly:            sc.ReadOnly,
		MFAFreshnessSeconds: sc.MFAFreshnessSeconds,
		Locale:              sc.Locale,
		Slug:                sc.Slug,
	}
}
