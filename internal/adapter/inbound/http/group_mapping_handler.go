package http

import (
	"net/http"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/gin-gonic/gin"
)

type GroupMappingHandler struct {
	svc *service.GroupMappingService
}

func NewGroupMappingHandler(svc *service.GroupMappingService) *GroupMappingHandler {
	return &GroupMappingHandler{svc: svc}
}

// ListDeptRole is P-14. Members can read.
//
// @Summary      P-14 — List Keycloak-group → dept-role mappings
// @Description  Any active tenant member may read.
// @Tags         groups
// @Produce      json
// @Param        id   path      string                          true  "Tenant UUID"  format(uuid)
// @Success      200  {object}  GroupDeptRoleMappingsResponse
// @Failure      403  {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/group-mappings/department-roles [get]
func (h *GroupMappingHandler) ListDeptRole(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	if err := requireSameTenantMember(c, tenantID); err != nil {
		HandleError(c, err)
		return
	}
	ms, err := h.svc.ListDeptRole(c.Request.Context(), tenantID)
	if err != nil {
		HandleError(c, err)
		return
	}
	out := make([]GroupDeptRoleMappingWire, len(ms))
	for i, m := range ms {
		out[i] = GroupDeptRoleMappingWire{KeycloakGroupName: m.KeycloakGroupName, RoleCode: string(m.RoleCode)}
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

// PutDeptRole is P-15.
//
// @Summary      P-15 — Full-replacement of Keycloak-group → dept-role mappings
// @Description  AUTH-2. Full replacement — anything not in the payload is deleted.
// @Tags         groups
// @Accept       json
// @Produce      json
// @Param        id       path      string                          true  "Tenant UUID"  format(uuid)
// @Param        request  body      GroupDeptRoleMappingsRequest    true  "Desired mappings"
// @Success      200      {object}  GroupDeptRoleMappingsResponse
// @Failure      400      {object}  ErrorResponse
// @Failure      403      {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/group-mappings/department-roles [put]
func (h *GroupMappingHandler) PutDeptRole(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	if err := requireTenantAdmin(c, tenantID); err != nil {
		HandleError(c, err)
		return
	}
	var body struct {
		Mappings []GroupDeptRoleMappingWire `json:"mappings"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	desired := make([]domain.GroupDeptRoleMapping, len(body.Mappings))
	for i, m := range body.Mappings {
		desired[i] = domain.GroupDeptRoleMapping{
			TenantID:          tenantID,
			KeycloakGroupName: m.KeycloakGroupName,
			RoleCode:          domain.DeptRole(m.RoleCode),
		}
	}
	out, err := h.svc.ReplaceDeptRole(c.Request.Context(), tenantID, desired)
	if err != nil {
		HandleError(c, err)
		return
	}
	wire := make([]GroupDeptRoleMappingWire, len(out))
	for i, m := range out {
		wire[i] = GroupDeptRoleMappingWire{KeycloakGroupName: m.KeycloakGroupName, RoleCode: string(m.RoleCode)}
	}
	c.JSON(http.StatusOK, gin.H{"items": wire})
}

// ListDept is P-16.
//
// @Summary      P-16 — List Keycloak-group → department mappings
// @Description  Any active tenant member may read.
// @Tags         groups
// @Produce      json
// @Param        id   path      string                      true  "Tenant UUID"  format(uuid)
// @Success      200  {object}  GroupDeptMappingsResponse
// @Failure      403  {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/group-mappings/departments [get]
func (h *GroupMappingHandler) ListDept(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	if err := requireSameTenantMember(c, tenantID); err != nil {
		HandleError(c, err)
		return
	}
	ms, err := h.svc.ListDept(c.Request.Context(), tenantID)
	if err != nil {
		HandleError(c, err)
		return
	}
	out := make([]GroupDeptMappingWire, len(ms))
	for i, m := range ms {
		out[i] = GroupDeptMappingWire{KeycloakGroupName: m.KeycloakGroupName, DepartmentID: m.DepartmentID}
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

// PutDept is P-17.
//
// @Summary      P-17 — Full-replacement of Keycloak-group → department mappings
// @Description  AUTH-2. Full replacement — anything not in the payload is deleted.
// @Tags         groups
// @Accept       json
// @Produce      json
// @Param        id       path      string                     true  "Tenant UUID"  format(uuid)
// @Param        request  body      GroupDeptMappingsRequest   true  "Desired mappings"
// @Success      200      {object}  GroupDeptMappingsResponse
// @Failure      400      {object}  ErrorResponse
// @Failure      403      {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/group-mappings/departments [put]
func (h *GroupMappingHandler) PutDept(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	if err := requireTenantAdmin(c, tenantID); err != nil {
		HandleError(c, err)
		return
	}
	var body struct {
		Mappings []GroupDeptMappingWire `json:"mappings"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	desired := make([]domain.GroupDeptMapping, len(body.Mappings))
	for i, m := range body.Mappings {
		desired[i] = domain.GroupDeptMapping{
			TenantID:          tenantID,
			KeycloakGroupName: m.KeycloakGroupName,
			DepartmentID:      m.DepartmentID,
		}
	}
	out, err := h.svc.ReplaceDept(c.Request.Context(), tenantID, desired)
	if err != nil {
		HandleError(c, err)
		return
	}
	wire := make([]GroupDeptMappingWire, len(out))
	for i, m := range out {
		wire[i] = GroupDeptMappingWire{KeycloakGroupName: m.KeycloakGroupName, DepartmentID: m.DepartmentID}
	}
	c.JSON(http.StatusOK, gin.H{"items": wire})
}

// PutTenantRole is P-29 (new §16 A25).
//
// @Summary      P-29 — Full-replacement of Keycloak-group → tenant-role mappings (§16 A25)
// @Description  AUTH-2. GTRM-6: 'member' barred at both service and DB layer.
// @Tags         groups
// @Accept       json
// @Produce      json
// @Param        id       path      string                            true  "Tenant UUID"  format(uuid)
// @Param        request  body      GroupTenantRoleMappingsRequest    true  "Desired mappings"
// @Success      200      {object}  GroupTenantRoleMappingsResponse
// @Failure      400      {object}  ErrorResponse  "invalid_role (member barred)"
// @Failure      403      {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/group-mappings/tenant-roles [put]
func (h *GroupMappingHandler) PutTenantRole(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	if err := requireTenantAdmin(c, tenantID); err != nil {
		HandleError(c, err)
		return
	}
	var body struct {
		Mappings []GroupTenantRoleMappingWire `json:"mappings"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	desired := make([]domain.GroupTenantRoleMapping, len(body.Mappings))
	for i, m := range body.Mappings {
		desired[i] = domain.GroupTenantRoleMapping{
			TenantID:          tenantID,
			KeycloakGroupName: m.KeycloakGroupName,
			RoleCode:          domain.TenantRoleCode(m.RoleCode),
		}
	}
	out, err := h.svc.ReplaceTenantRole(c.Request.Context(), tenantID, desired)
	if err != nil {
		HandleError(c, err)
		return
	}
	wire := make([]GroupTenantRoleMappingWire, len(out))
	for i, m := range out {
		wire[i] = GroupTenantRoleMappingWire{KeycloakGroupName: m.KeycloakGroupName, RoleCode: string(m.RoleCode)}
	}
	c.JSON(http.StatusOK, gin.H{"items": wire})
}
