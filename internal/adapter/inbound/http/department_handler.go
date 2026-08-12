package http

import (
	"net/http"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// DepartmentHandler wires P-3 (list), P-24 (activate), P-25 (toggle) to
// DepartmentService.
type DepartmentHandler struct {
	svc *service.DepartmentService
}

func NewDepartmentHandler(svc *service.DepartmentService) *DepartmentHandler {
	return &DepartmentHandler{svc: svc}
}

// List is P-3: any active member.
//
// @Summary      P-3 — List active departments
// @Description  Returns the tenant_departments rows joined with the global departments catalog. Any active tenant member may read.
// @Tags         departments
// @Produce      json
// @Param        id   path      string                  true  "Tenant UUID"  format(uuid)
// @Success      200  {object}  DepartmentListResponse
// @Failure      403  {object}  ErrorResponse
// @Failure      404  {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/departments [get]
func (h *DepartmentHandler) List(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	rc, ok := requestctx.FromContext(c.Request.Context())
	if !ok {
		HandleError(c, domain.NewError(domain.ErrMissingIdentity, "missing identity"))
		return
	}
	if rc.TenantID != tenantID {
		HandleError(c, domain.NewError(domain.ErrInsufficientRole, "cannot read another tenant"))
		return
	}

	views, err := h.svc.ListForTenant(c.Request.Context(), tenantID)
	if err != nil {
		HandleError(c, err)
		return
	}
	out := make([]DepartmentResponse, 0, len(views))
	for _, v := range views {
		out = append(out, DepartmentResponse{
			DepartmentID:  v.DepartmentID,
			Code:          v.Code,
			Name:          v.Name,
			IsSystem:      v.IsSystem,
			IsActive:      v.IsActive,
			RecordVersion: v.RecordVersion,
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

// Activate is P-24: tenant_admin/tenant_owner activates a catalog dept.
// URL: POST /tenants/:id/departments with body { department_id }.
//
// @Summary      P-24 — Activate a catalog department for the tenant
// @Description  AUTH-2 tenant_admin/owner. Returns 409 department_already_activated if already present (TD-7). Returns 422 department_retired if catalog entry is globally inactive (D-5/TD-1).
// @Tags         departments
// @Accept       json
// @Produce      json
// @Param        id       path      string                             true  "Tenant UUID"  format(uuid)
// @Param        request  body      object{department_id=string}       true  "Catalog department UUID"
// @Success      201      {object}  TenantDepartmentActivatedResponse
// @Failure      400      {object}  ErrorResponse
// @Failure      403      {object}  ErrorResponse  "AUTH-1..8 gate failed"
// @Failure      404      {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/departments [post]
func (h *DepartmentHandler) Activate(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	rc, ok := requestctx.FromContext(c.Request.Context())
	if !ok {
		HandleError(c, domain.NewError(domain.ErrMissingIdentity, "missing identity"))
		return
	}
	if rc.TenantID != tenantID {
		HandleError(c, domain.NewError(domain.ErrInsufficientRole, "cannot mutate another tenant"))
		return
	}
	if !rc.IsAdmin() {
		HandleError(c, domain.NewError(domain.ErrInsufficientRole, "tenant_admin or tenant_owner required"))
		return
	}
	var body struct {
		DepartmentID uuid.UUID `json:"department_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.DepartmentID == uuid.Nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "department_id is required").
			WithDetails(map[string]any{"code": "invalid_uuid"}))
		return
	}
	td, wasCreated, err := h.svc.Activate(c.Request.Context(), tenantID, body.DepartmentID)
	if err != nil {
		HandleError(c, err)
		return
	}
	// P-24 idempotent: 201 on fresh create, 200 on replay (LLD §5.4).
	status := http.StatusOK
	if wasCreated {
		status = http.StatusCreated
	}
	c.JSON(status, gin.H{
		"department_id":  td.DepartmentID,
		"is_active":      td.IsActive,
		"record_version": td.RecordVersion,
	})
}

// Patch is P-25: toggle is_active on a tenant_departments row.
// URL: PATCH /tenants/:id/departments/:dept_id
//
// @Summary      P-25 — Toggle is_active (deactivate/reactivate)
// @Description  AUTH-2. System-department deactivation is blocked by chk_system_department_active (422 system_department_cannot_be_retired). Applies optimistic locking (CONC-4).
// @Tags         departments
// @Accept       json
// @Produce      json
// @Param        id       path      string                        true  "Tenant UUID"    format(uuid)
// @Param        dept_id  path      string                        true  "Department UUID" format(uuid)
// @Param        request  body      TenantDepartmentPatchRequest  true  "Toggle payload"
// @Success      200      {object}  TenantDepartmentActivatedResponse
// @Failure      400      {object}  ErrorResponse
// @Failure      403      {object}  ErrorResponse
// @Failure      409      {object}  ErrorResponse  "record_version mismatch (CONC-4)"
// @Failure      422      {object}  ErrorResponse  "system_department_cannot_be_retired"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/departments/{dept_id} [patch]
func (h *DepartmentHandler) Patch(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	deptID, err := uuid.Parse(c.Param("dept_id"))
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "department id is not a valid UUID").
			WithDetails(map[string]any{"code": "invalid_uuid"}))
		return
	}
	rc, ok := requestctx.FromContext(c.Request.Context())
	if !ok {
		HandleError(c, domain.NewError(domain.ErrMissingIdentity, "missing identity"))
		return
	}
	if rc.TenantID != tenantID {
		HandleError(c, domain.NewError(domain.ErrInsufficientRole, "cannot mutate another tenant"))
		return
	}
	if !rc.IsAdmin() {
		HandleError(c, domain.NewError(domain.ErrInsufficientRole, "tenant_admin or tenant_owner required"))
		return
	}
	var req TenantDepartmentPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	if req.IsActive == nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "is_active is required"))
		return
	}
	td, err := h.svc.SetActive(c.Request.Context(), tenantID, deptID, *req.IsActive, req.RecordVersion)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"department_id":  td.DepartmentID,
		"is_active":      td.IsActive,
		"record_version": td.RecordVersion,
	})
}
