package http

import (
	"net/http"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/gin-gonic/gin"
)

type DeptMembershipHandler struct {
	svc *service.DeptMembershipService
}

func NewDeptMembershipHandler(svc *service.DeptMembershipService) *DeptMembershipHandler {
	return &DeptMembershipHandler{svc: svc}
}

// List is P-9 — dept members by level.
//
// @Summary      P-9 — List department members by level
// @Description  Returns preparator/reviewer/approver assignments for the given department.
// @Tags         departments
// @Produce      json
// @Param        id       path      string                  true  "Tenant UUID"    format(uuid)
// @Param        dept_id  path      string                  true  "Department UUID" format(uuid)
// @Success      200      {object}  DeptMemberListResponse
// @Failure      403      {object}  ErrorResponse
// @Failure      404      {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/departments/{dept_id}/members [get]
func (h *DeptMembershipHandler) List(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	deptID, err := parseUUIDParam(c, "dept_id")
	if err != nil {
		HandleError(c, err)
		return
	}
	if err := requireSameTenantMember(c, tenantID); err != nil {
		HandleError(c, err)
		return
	}
	dms, err := h.svc.ListByDepartment(c.Request.Context(), tenantID, deptID)
	if err != nil {
		HandleError(c, err)
		return
	}
	out := make([]gin.H, len(dms))
	for i, dm := range dms {
		out[i] = gin.H{"user_id": dm.UserID, "level": string(dm.RoleLevel)}
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

// Assign is P-10 — PUT user into a dept at a level.
//
// @Summary      P-10 — Assign user to department at level
// @Description  AUTH-2. Fresh grant → DepartmentMembershipGranted; level change → DepartmentMembershipLevelChanged with previous_level.
// @Tags         departments
// @Accept       json
// @Produce      json
// @Param        id       path      string                    true  "Tenant UUID"    format(uuid)
// @Param        dept_id  path      string                    true  "Department UUID" format(uuid)
// @Param        user_id  path      string                    true  "User UUID"       format(uuid)
// @Param        request  body      DeptMembershipPutRequest  true  "Level payload"
// @Success      200      {object}  DeptMembershipAssignResponse
// @Failure      400      {object}  ErrorResponse
// @Failure      403      {object}  ErrorResponse
// @Failure      422      {object}  ErrorResponse  "department_not_active_for_tenant"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/departments/{dept_id}/members/{user_id} [put]
func (h *DeptMembershipHandler) Assign(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	deptID, err := parseUUIDParam(c, "dept_id")
	if err != nil {
		HandleError(c, err)
		return
	}
	userID, err := parseUUIDParam(c, "user_id")
	if err != nil {
		HandleError(c, err)
		return
	}
	if err := requireTenantAdmin(c, tenantID); err != nil {
		HandleError(c, err)
		return
	}
	var req DeptMembershipPutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	rc, _ := requestctx.FromContext(c.Request.Context())
	dm, err := h.svc.Assign(c.Request.Context(), tenantID, userID, deptID, domain.DeptRole(req.Level), rc.UserID)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"user_id":        dm.UserID,
		"department_id":  dm.DepartmentID,
		"level":          string(dm.RoleLevel),
		"record_version": dm.RecordVersion,
	})
}

// Remove is P-11 — DELETE user from a dept.
//
// @Summary      P-11 — Remove user from department
// @Description  AUTH-2. §8.8.4 delegate-impact gated (WFI-11) — returns 409 workflow_resolution_required when the user is an active delegate for open workflows scoped to this department.
// @Tags         departments
// @Produce      json
// @Param        id       path      string  true  "Tenant UUID"    format(uuid)
// @Param        dept_id  path      string  true  "Department UUID" format(uuid)
// @Param        user_id  path      string  true  "User UUID"       format(uuid)
// @Success      200      {object}  DeptMembershipRemoveResponse
// @Failure      403      {object}  ErrorResponse
// @Failure      404      {object}  ErrorResponse
// @Failure      409      {object}  ErrorResponse  "workflow_resolution_required (§8.8.4)"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/departments/{dept_id}/members/{user_id} [delete]
func (h *DeptMembershipHandler) Remove(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	deptID, err := parseUUIDParam(c, "dept_id")
	if err != nil {
		HandleError(c, err)
		return
	}
	userID, err := parseUUIDParam(c, "user_id")
	if err != nil {
		HandleError(c, err)
		return
	}
	if err := requireTenantAdmin(c, tenantID); err != nil {
		HandleError(c, err)
		return
	}
	rc, _ := requestctx.FromContext(c.Request.Context())
	dm, err := h.svc.Remove(c.Request.Context(), tenantID, userID, deptID, rc.UserID)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user_id": dm.UserID, "department_id": dm.DepartmentID, "removed": true})
}
