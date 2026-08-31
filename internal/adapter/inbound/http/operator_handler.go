package http

import (
	"net/http"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	pgcommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type OperatorHandler struct {
	svc *service.OperatorService
}

func NewOperatorHandler(svc *service.OperatorService) *OperatorHandler {
	return &OperatorHandler{svc: svc}
}

// ── O-4 PATCH /operator/tenants/:id/feature-flags ─────────────────────

// SetFeatureFlags is O-4 — full-replacement feature-flag override (allow-list, scalars only).
//
// @Summary      O-4 — Full-replacement feature-flag override (allow-list, scalars only)
// @Description  PLAN-6(d): non-scalar values → 400 invalid_feature_value. Unknown keys → 400 unknown_feature_flag.
// @Tags         operator
// @Accept       json
// @Produce      json
// @Param        id       path      string                         true  "Tenant UUID"  format(uuid)
// @Param        request  body      OperatorFeatureFlagsRequest    true  "Feature-flags map"
// @Success      200      {object}  TenantResponse
// @Failure      400      {object}  ErrorResponse  "unknown_feature_flag | invalid_feature_value"
// @Failure      403      {object}  ErrorResponse
// @Failure      404      {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /operator/tenants/{id}/feature-flags [patch]
func (h *OperatorHandler) SetFeatureFlags(c *gin.Context) {
	if err := requireOperator(c); err != nil {
		HandleError(c, err)
		return
	}
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	var req OperatorFeatureFlagsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	// RLS-6: override GUC with the PATH tenant so operator cross-tenant
	// mutations are scoped to the target tenant, not the header tenant.
	ctx := c.Request.Context()
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.TenantID = tenantID.String()
	ctx = pgcommon.WithGUCSet(ctx, g)
	t, err := h.svc.SetFeatureFlags(ctx, tenantID, req.FeatureFlags, req.RecordVersion)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, TenantToResponse(t))
}

// ── O-7 POST /operator/tenants/:id/reassign-owner ─────────────────────

// ReassignOwner is O-7 — recover ownerless tenant.
//
// @Summary      O-7 — Recover ownerless tenant (grants tenant_owner, clears ownerless_since)
// @Description  Only path to resolve TM-12 escalation. 422 invalid_owner_candidate if the new owner is not an active member. 409 tenant_offboarded if the tenant is no longer eligible.
// @Tags         operator
// @Accept       json
// @Produce      json
// @Param        id       path      string                          true  "Tenant UUID"  format(uuid)
// @Param        request  body      OperatorReassignOwnerRequest    true  "New owner payload"
// @Success      200      {object}  OperatorReassignOwnerResponse
// @Failure      400      {object}  ErrorResponse
// @Failure      403      {object}  ErrorResponse
// @Failure      409      {object}  ErrorResponse  "tenant_offboarded"
// @Failure      422      {object}  ErrorResponse  "invalid_owner_candidate"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /operator/tenants/{id}/reassign-owner [post]
func (h *OperatorHandler) ReassignOwner(c *gin.Context) {
	if err := requireOperator(c); err != nil {
		HandleError(c, err)
		return
	}
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	var req OperatorReassignOwnerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	newOwnerID := req.EffectiveUserID()
	if newOwnerID == uuid.Nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "user_id is required"))
		return
	}
	// RLS-6: override GUC with the PATH tenant so operator cross-tenant
	// mutations are scoped to the target tenant, not the header tenant.
	ctx := c.Request.Context()
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.TenantID = tenantID.String()
	ctx = pgcommon.WithGUCSet(ctx, g)
	rc, _ := requestctx.FromContext(ctx)
	tr, err := h.svc.ReassignOwner(ctx, tenantID, newOwnerID, rc.UserID)
	if err != nil {
		HandleError(c, err)
		return
	}
	// LLD §5.4 O-7: step 4 unconditionally clears ownerless_since in the
	// same tx as the grant, so it is always nil by this point — echoed
	// back rather than re-read, since ReassignOwner already guarantees it.
	roleCodes, err := h.svc.ActiveRoleCodes(ctx, tenantID, tr.UserID)
	if err != nil {
		HandleError(c, err)
		return
	}
	roles := make([]string, len(roleCodes))
	for i, code := range roleCodes {
		roles[i] = string(code)
	}
	c.JSON(http.StatusOK, OperatorReassignOwnerResponse{
		TenantID:       tr.TenantID,
		UserID:         tr.UserID,
		Roles:          roles,
		OwnerlessSince: nil,
	})
}

// requireOperator is the AUTH-6 gate for /operator/* routes. Also
// re-checked here in addition to the RequireOperatorRole middleware
// (defense-in-depth per AUTH-7).
func requireOperator(c *gin.Context) error {
	rc, ok := requestctx.FromContext(c.Request.Context())
	if !ok {
		return domain.NewError(domain.ErrMissingIdentity, "missing identity")
	}
	if !rc.IsOperator() {
		return domain.NewError(domain.ErrInsufficientRole, "platform_operator required")
	}
	return nil
}
