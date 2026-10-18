package http

import (
	"net/http"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/gin-gonic/gin"
)

type RoleLabelHandler struct {
	svc *service.RoleLabelService
}

func NewRoleLabelHandler(svc *service.RoleLabelService) *RoleLabelHandler {
	return &RoleLabelHandler{svc: svc}
}

// List is P-12.
//
// @Summary      P-12 — List dept-role labels (preparator/reviewer/approver)
// @Description  Returns the tenant's customized display labels for the three department role levels.
// @Tags         roles
// @Produce      json
// @Param        id   path      string                 true  "Tenant UUID"  format(uuid)
// @Success      200  {object}  RoleLabelListResponse
// @Failure      403  {object}  ProblemResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/roles [get]
func (h *RoleLabelHandler) List(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	if err := requireSameTenantMember(c, tenantID); err != nil {
		HandleError(c, err)
		return
	}
	labels, err := h.svc.List(c.Request.Context(), tenantID)
	if err != nil {
		HandleError(c, err)
		return
	}
	out := make([]RoleLabelResponse, len(labels))
	for i, l := range labels {
		out[i] = RoleLabelResponse{
			RoleCode:      string(l.RoleCode),
			DisplayName:   l.DisplayName,
			RecordVersion: l.RecordVersion,
		}
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

// Patch is P-13.
//
// @Summary      P-13 — Rename a dept-role label (display_name only)
// @Description  AUTH-2. Applies optimistic locking (CONC-4). Only display_name is mutable; role_code is immutable.
// @Tags         roles
// @Accept       json
// @Produce      json
// @Param        id         path      string                 true  "Tenant UUID"  format(uuid)
// @Param        role_code  path      string                 true  "Role level"   Enums(preparator, reviewer, approver)
// @Param        request    body      RoleLabelPatchRequest  true  "Rename payload"
// @Success      200        {object}  RoleLabelResponse
// @Failure      400        {object}  ProblemResponse
// @Failure      403        {object}  ProblemResponse
// @Failure      409        {object}  ProblemResponse  "record_version mismatch (CONC-4)"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/roles/{role_code} [patch]
func (h *RoleLabelHandler) Patch(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	roleCode := c.Param("role_code")
	allowedRoleCodes := map[string]bool{"preparator": true, "reviewer": true, "approver": true}
	if !allowedRoleCodes[roleCode] {
		// Path-param validation: return 400 (ErrValidation cause) with the
		// semantic "invalid_role" code so callers distinguish a bad path param
		// from a malformed body (VAL02/VAL03). ErrInvalidRole itself is a
		// domain-rule sentinel (422) used by the service layer.
		HandleError(c, &domain.DomainError{Code: "invalid_role", Message: "invalid role_code", Cause: domain.ErrValidation})
		return
	}
	if err := requireTenantAdmin(c, tenantID); err != nil {
		HandleError(c, err)
		return
	}
	var req RoleLabelPatchRequest
	if err := gincommon.ShouldBindJSON(c, &req); err != nil {
		HandleError(c, err)
		return
	}
	l, err := h.svc.Update(c.Request.Context(), tenantID, roleCode, req.DisplayName, req.RecordVersion)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, RoleLabelResponse{
		RoleCode:      string(l.RoleCode),
		DisplayName:   l.DisplayName,
		RecordVersion: l.RecordVersion,
	})
}
