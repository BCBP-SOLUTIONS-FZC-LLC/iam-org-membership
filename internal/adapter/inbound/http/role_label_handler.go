package http

import (
	"net/http"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
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
// @Failure      403  {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
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
// @Failure      400        {object}  ErrorResponse
// @Failure      403        {object}  ErrorResponse
// @Failure      409        {object}  ErrorResponse  "record_version mismatch (CONC-4)"
// @Security     UserID
// @Security     TenantID
// @Router       /tenants/{id}/roles/{role_code} [patch]
func (h *RoleLabelHandler) Patch(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	roleCode := c.Param("role_code")
	if err := requireTenantAdmin(c, tenantID); err != nil {
		HandleError(c, err)
		return
	}
	var req RoleLabelPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
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
