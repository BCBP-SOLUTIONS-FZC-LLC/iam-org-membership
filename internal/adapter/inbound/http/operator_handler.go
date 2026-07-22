package http

import (
	"net/http"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/gin-gonic/gin"
)

type OperatorHandler struct {
	svc *service.OperatorService
}

func NewOperatorHandler(svc *service.OperatorService) *OperatorHandler {
	return &OperatorHandler{svc: svc}
}

// ── O-1 POST /operator/departments ────────────────────────────────────

// CreateDepartment is O-1 — add a global-catalog department.
//
// @Summary      O-1 — Add global-catalog department
// @Description  Creates a department in the global operator catalog. is_system and code are immutable after creation.
// @Tags         operator
// @Accept       json
// @Produce      json
// @Param        request  body      OperatorDepartmentCreateRequest  true  "Department payload"
// @Success      201      {object}  OperatorDepartmentResponse
// @Failure      400      {object}  ErrorResponse
// @Failure      403      {object}  ErrorResponse
// @Failure      409      {object}  ErrorResponse  "duplicate_code"
// @Security     UserID
// @Security     TenantID
// @Router       /operator/departments [post]
func (h *OperatorHandler) CreateDepartment(c *gin.Context) {
	if err := requireOperator(c); err != nil {
		HandleError(c, err)
		return
	}
	var req OperatorDepartmentCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	d, err := h.svc.CreateDepartment(c.Request.Context(), req.Code, req.Name, req.IsSystem)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": d.ID, "code": d.Code, "name": d.Name, "is_system": d.IsSystem, "is_active": d.IsActive})
}

// ── O-2 PATCH /operator/departments/:id ───────────────────────────────

// PatchDepartment is O-2 — update name or is_active (code + is_system immutable).
//
// @Summary      O-2 — Update name or is_active (code + is_system immutable)
// @Description  System-department deactivation is blocked by chk_system_department_active (422 system_department_cannot_be_retired). Applies optimistic locking (CONC-4).
// @Tags         operator
// @Accept       json
// @Produce      json
// @Param        id       path      string                          true  "Department UUID"  format(uuid)
// @Param        request  body      OperatorDepartmentPatchRequest  true  "Patch payload"
// @Success      200      {object}  OperatorDepartmentResponse
// @Failure      400      {object}  ErrorResponse
// @Failure      403      {object}  ErrorResponse
// @Failure      404      {object}  ErrorResponse
// @Failure      409      {object}  ErrorResponse  "record_version mismatch (CONC-4)"
// @Failure      422      {object}  ErrorResponse  "field_immutable | system_department_cannot_be_retired"
// @Security     UserID
// @Security     TenantID
// @Router       /operator/departments/{id} [patch]
func (h *OperatorHandler) PatchDepartment(c *gin.Context) {
	if err := requireOperator(c); err != nil {
		HandleError(c, err)
		return
	}
	id, err := parseUUIDParam(c, "id")
	if err != nil {
		HandleError(c, err)
		return
	}
	var req OperatorDepartmentPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	d, err := h.svc.PatchDepartment(c.Request.Context(), id, req.Name, req.IsActive, req.RecordVersion)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": d.ID, "code": d.Code, "name": d.Name, "is_active": d.IsActive, "record_version": d.RecordVersion})
}

// ── O-3 DELETE /operator/departments/:id → 405 ────────────────────────

// DeleteDepartmentBlocked is O-3 — always returns 405 (retire via O-2 is_active=false).
//
// @Summary      O-3 — Blocked (405); retire via O-2 is_active=false
// @Description  Departments cannot be physically deleted — always returns 405 with cannot_delete_system_department.
// @Tags         operator
// @Produce      json
// @Param        id   path      string          true  "Department UUID"  format(uuid)
// @Failure      405  {object}  ErrorResponse   "Method not allowed"
// @Security     UserID
// @Security     TenantID
// @Router       /operator/departments/{id} [delete]
func (h *OperatorHandler) DeleteDepartmentBlocked(c *gin.Context) {
	if err := requireOperator(c); err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusMethodNotAllowed, gin.H{
		"code":    "cannot_delete_system_department",
		"message": "departments cannot be deleted; retire via PATCH is_active=false",
	})
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
	t, err := h.svc.SetFeatureFlags(c.Request.Context(), tenantID, req.FeatureFlags)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, TenantToResponse(t))
}

// ── O-5 GET /operator/plans ───────────────────────────────────────────

// ListPlans is O-5 — list plan entitlement catalog.
//
// @Summary      O-5 — List plan entitlement catalog
// @Description  Returns all rows from the global `plans` table (§16 A19).
// @Tags         operator
// @Produce      json
// @Success      200  {object}  OperatorPlansListResponse
// @Failure      403  {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Router       /operator/plans [get]
func (h *OperatorHandler) ListPlans(c *gin.Context) {
	if err := requireOperator(c); err != nil {
		HandleError(c, err)
		return
	}
	plans, err := h.svc.ListPlans(c.Request.Context())
	if err != nil {
		HandleError(c, err)
		return
	}
	out := make([]gin.H, len(plans))
	for i, p := range plans {
		out[i] = gin.H{
			"code":                    p.Code,
			"display_name":            p.DisplayName,
			"workflow_template_limit": p.WorkflowTemplateLimit,
			"tender_limit":            p.TenderLimit,
			"trial_duration_days":     p.TrialDurationDays,
			"sso_enabled":             p.SSOEnabled,
			"custom_branding":         p.CustomBranding,
			"feature_set":             p.FeatureSet,
			"record_version":          p.RecordVersion,
		}
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

// ── O-6 PATCH /operator/plans/:code ───────────────────────────────────

// PatchPlan is O-6 — edit plan entitlements (PATCH-only, PLAN-4).
//
// @Summary      O-6 — Edit plan entitlements (PATCH-only, PLAN-4)
// @Description  PLAN-4: plans may not be deleted, only updated. Applies optimistic locking (CONC-4).
// @Tags         operator
// @Accept       json
// @Produce      json
// @Param        code     path      string                     true  "Plan code"  Enums(starter, pro, enterprise)
// @Param        request  body      OperatorPlanPatchRequest   true  "Plan patch"
// @Success      200      {object}  OperatorPlanResponse
// @Failure      400      {object}  ErrorResponse
// @Failure      403      {object}  ErrorResponse
// @Failure      404      {object}  ErrorResponse
// @Failure      409      {object}  ErrorResponse  "record_version mismatch (CONC-4)"
// @Security     UserID
// @Security     TenantID
// @Router       /operator/plans/{code} [patch]
func (h *OperatorHandler) PatchPlan(c *gin.Context) {
	if err := requireOperator(c); err != nil {
		HandleError(c, err)
		return
	}
	code := domain.TenantPlan(c.Param("code"))
	var body struct {
		DisplayName           *string        `json:"display_name,omitempty"`
		WorkflowTemplateLimit *int           `json:"workflow_template_limit,omitempty"`
		TenderLimit           *int           `json:"tender_limit,omitempty"`
		TrialDurationDays     *int           `json:"trial_duration_days,omitempty"`
		SSOEnabled            *bool          `json:"sso_enabled,omitempty"`
		CustomBranding        *string        `json:"custom_branding,omitempty"`
		FeatureSet            map[string]any `json:"feature_set,omitempty"`
		RecordVersion         int64          `json:"record_version"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	patch := &domain.PlanPatch{
		DisplayName:       body.DisplayName,
		TrialDurationDays: body.TrialDurationDays,
		SSOEnabled:        body.SSOEnabled,
		FeatureSet:        body.FeatureSet,
		RecordVersion:     body.RecordVersion,
	}
	if body.WorkflowTemplateLimit != nil {
		patch.WorkflowTemplateLimit = &body.WorkflowTemplateLimit
	}
	if body.TenderLimit != nil {
		patch.TenderLimit = &body.TenderLimit
	}
	if body.CustomBranding != nil {
		v := domain.BrandingLevel(*body.CustomBranding)
		patch.CustomBranding = &v
	}
	p, err := h.svc.PatchPlan(c.Request.Context(), code, patch)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"code":                    p.Code,
		"display_name":            p.DisplayName,
		"workflow_template_limit": p.WorkflowTemplateLimit,
		"tender_limit":            p.TenderLimit,
		"trial_duration_days":     p.TrialDurationDays,
		"record_version":          p.RecordVersion,
	})
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
	rc, _ := requestctx.FromContext(c.Request.Context())
	tr, err := h.svc.ReassignOwner(c.Request.Context(), tenantID, req.NewOwnerUserID, rc.UserID)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"tenant_id": tr.TenantID,
		"user_id":   tr.UserID,
		"role_code": string(tr.RoleCode),
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
