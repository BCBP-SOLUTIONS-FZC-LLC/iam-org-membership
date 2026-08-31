package http

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type MembershipHandler struct {
	svc *service.MembershipService
}

func NewMembershipHandler(svc *service.MembershipService) *MembershipHandler {
	return &MembershipHandler{svc: svc}
}

// List is P-4 — cursor-paginated members. Encodes cursor as base64(json).
//
// @Summary      P-4 — List members (cursor-paginated)
// @Description  Keyset paginate on idx_tm_tenant_created. Response includes `next_cursor` when more pages exist.
// @Tags         members
// @Produce      json
// @Param        id      path      string                   true   "Tenant UUID"                                              format(uuid)
// @Param        limit   query     integer                  false  "Page size (1..200, default 50)"
// @Param        cursor  query     string                   false  "Base64-encoded opaque cursor from previous response"
// @Success      200     {object}  MembershipListResponse
// @Failure      400     {object}  ErrorResponse
// @Failure      403     {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/members [get]
func (h *MembershipHandler) List(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	if err := requireSameTenantMember(c, tenantID); err != nil {
		HandleError(c, err)
		return
	}
	limit := 50
	if s, exists := c.GetQuery("limit"); exists {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 || n > 200 {
			HandleError(c, domain.NewError(domain.ErrValidation, "invalid limit").
				WithDetails(map[string]any{"code": "invalid_limit"}))
			return
		}
		limit = n
	}
	var cursor *domain.MembershipListCursor
	if raw := c.Query("cursor"); raw != "" {
		decoded, err := base64.URLEncoding.DecodeString(raw)
		if err != nil {
			HandleError(c, domain.NewError(domain.ErrValidation, "invalid cursor").
				WithDetails(map[string]any{"code": "invalid_cursor"}))
			return
		}
		cursor = &domain.MembershipListCursor{}
		if err := json.Unmarshal(decoded, cursor); err != nil {
			HandleError(c, domain.NewError(domain.ErrValidation, "invalid cursor").
				WithDetails(map[string]any{"code": "invalid_cursor"}))
			return
		}
	}
	page, err := h.svc.List(c.Request.Context(), tenantID, cursor, limit)
	if err != nil {
		HandleError(c, err)
		return
	}
	resp := gin.H{"items": pageItemsToResponse(page.Items)}
	if page.NextCursor != nil {
		b, _ := json.Marshal(page.NextCursor)
		resp["next_cursor"] = base64.URLEncoding.EncodeToString(b)
	}
	c.JSON(http.StatusOK, resp)
}

// Get is P-5 — single member with roles + dept memberships.
//
// @Summary      P-5 — Read a single member (with roles + departments)
// @Description  Returns the tenant_memberships row joined with tenant_roles and dept_memberships views.
// @Tags         members
// @Produce      json
// @Param        id       path      string               true  "Tenant UUID"  format(uuid)
// @Param        user_id  path      string               true  "User UUID"    format(uuid)
// @Success      200      {object}  MemberItemResponse
// @Failure      403      {object}  ErrorResponse
// @Failure      404      {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/members/{user_id} [get]
func (h *MembershipHandler) Get(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	userID, err := parseUUIDParam(c, "user_id")
	if err != nil {
		HandleError(c, err)
		return
	}
	if err := requireSameTenantMember(c, tenantID); err != nil {
		HandleError(c, err)
		return
	}
	item, err := h.svc.Get(c.Request.Context(), tenantID, userID)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, memberItemToResponse(*item))
}

// Patch is P-7 — status change (suspend/reactivate). tenant_admin/owner only.
//
// @Summary      P-7 — Suspend/reactivate member (status only)
// @Description  AUTH-2. On suspend: AUTH-8 best-effort RP RevokeUserSessions (fail-open). Applies optimistic locking (CONC-4).
// @Tags         members
// @Accept       json
// @Produce      json
// @Param        id       path      string                  true  "Tenant UUID"     format(uuid)
// @Param        user_id  path      string                  true  "User UUID"       format(uuid)
// @Param        request  body      MembershipPatchRequest  true  "Status payload"
// @Success      200      {object}  MembershipPatchResponse
// @Failure      400      {object}  ErrorResponse
// @Failure      403      {object}  ErrorResponse
// @Failure      409      {object}  ErrorResponse  "record_version mismatch (CONC-4)"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/members/{user_id} [patch]
func (h *MembershipHandler) Patch(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
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
	var req MembershipPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	if req.Status == nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "status is required"))
		return
	}
	status := domain.MembershipStatus(*req.Status)
	if status != domain.MembershipActive && status != domain.MembershipSuspended {
		HandleError(c, domain.NewError(domain.ErrValidation, "status must be 'active' or 'suspended'"))
		return
	}
	res, err := h.svc.SetStatus(c.Request.Context(), tenantID, userID, status, req.RecordVersion)
	if err != nil {
		HandleError(c, err)
		return
	}
	body := gin.H{
		"user_id":        res.Membership.UserID,
		"status":         res.Membership.Status,
		"record_version": res.Membership.RecordVersion,
		"updated_at":     res.Membership.UpdatedAt,
	}
	// WFI-13 advisory (§8.8.5) — attached to 200 on suspend when the user
	// is a delegate on active workflows, OR when the Workflow Service is
	// unavailable (checked=false). Absent on reactivate.
	if res.DelegateImpact != nil {
		body["delegate_impact"] = res.DelegateImpact
	}
	c.JSON(http.StatusOK, body)
}

// ReconcileRoles is P-28 — full-replacement multi-role reconcile with
// TM-8 last-owner guard.
//
// @Summary      P-28 — Full-replacement multi-role reconcile
// @Description  AUTH-2. §16 A14: one TenantRoleGranted/TenantRoleRevoked event per role delta. TM-8 last-owner guard → 422 last_owner_removal. `member` is barred (TR-7) — returns 400 invalid_role.
// @Tags         members
// @Accept       json
// @Produce      json
// @Param        id       path      string           true  "Tenant UUID"  format(uuid)
// @Param        user_id  path      string           true  "User UUID"    format(uuid)
// @Param        request  body      RolesPutRequest  true  "Desired roles"
// @Success      200      {object}  RolesReconcileResponse
// @Failure      400      {object}  ErrorResponse  "invalid_role (TR-7)"
// @Failure      403      {object}  ErrorResponse
// @Failure      422      {object}  ErrorResponse  "last_owner_removal"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/members/{user_id}/roles [put]
func (h *MembershipHandler) ReconcileRoles(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
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
	var req RolesPutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	rc, _ := requestctx.FromContext(c.Request.Context())
	desired := make([]domain.TenantRoleCode, len(req.Roles))
	for i, code := range req.Roles {
		desired[i] = domain.TenantRoleCode(code)
	}
	_, _, err = h.svc.ReconcileRoles(c.Request.Context(), tenantID, userID, desired, rc.UserID)
	if err != nil {
		HandleError(c, err)
		return
	}
	// P28-1: full-replacement — the post-reconcile elevated role set exactly
	// matches the request body, deduped (uq_tenant_roles_active already
	// collapses a duplicate role_code to one stored row) and sorted for a
	// deterministic response.
	roleSet := make(map[string]struct{}, len(req.Roles))
	for _, r := range req.Roles {
		roleSet[r] = struct{}{}
	}
	finalRoles := make([]string, 0, len(roleSet))
	for r := range roleSet {
		finalRoles = append(finalRoles, r)
	}
	sort.Strings(finalRoles)
	c.JSON(http.StatusOK, RolesReconcileResponse{
		UserID: userID,
		Roles:  finalRoles,
	})
}

// SeatUsage is P-27.
//
// @Summary      P-27 — Seat-usage projection
// @Description  AUTH-2. Returns `{active_users, pending_invitations, licensed_seats, over_cap, overage_since, grace_ends_at}` (SEAT-1..5).
// @Tags         members
// @Produce      json
// @Param        id   path      string             true  "Tenant UUID"  format(uuid)
// @Success      200  {object}  SeatUsageResponse
// @Failure      403  {object}  ErrorResponse
// @Failure      404  {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/seat-usage [get]
func (h *MembershipHandler) SeatUsage(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	if err := requireTenantAdmin(c, tenantID); err != nil {
		HandleError(c, err)
		return
	}
	usage, err := h.svc.SeatUsage(c.Request.Context(), tenantID)
	if err != nil {
		HandleError(c, err)
		return
	}
	resp := gin.H{
		"active_users":        usage.ActiveUsers,
		"pending_invitations": usage.PendingInvitations,
		"licensed_seats":      usage.LicensedSeats,
		"over_cap":            usage.OverCap,
	}
	if usage.OverageSince != nil {
		resp["overage_since"] = usage.OverageSince
	}
	if usage.GraceEndsAt != nil {
		resp["grace_ends_at"] = usage.GraceEndsAt
	}
	c.JSON(http.StatusOK, resp)
}

// Remove is P-8 (§8.8) — DELETE /tenants/:id/members/:user_id. Gated by
// WFI-1 delegate-impact pre-check: 409 workflow_resolution_required if
// the user is delegate on active workflows.
//
// @Summary      P-8 — Remove tenant member
// @Description  Gated by delegate-impact pre-check (§8.8, WFI-1). Returns 409 workflow_resolution_required with active_workflows/workflow_ids/allowed_actions if the user is a delegate. TM-8 last-owner refusal returns 422 last_owner_removal. AUTH-8 best-effort session revoke.
// @Tags         members
// @Param        id       path  string  true  "Tenant UUID"   format(uuid)
// @Param        user_id  path  string  true  "User UUID"     format(uuid)
// @Success      204
// @Failure      409      {object}  ErrorResponse  "workflow_resolution_required"
// @Failure      422      {object}  ErrorResponse  "last_owner_removal"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/members/{user_id} [delete]
func (h *MembershipHandler) Remove(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
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
	if err := h.svc.RemoveUser(c.Request.Context(), tenantID, userID, rc.UserID); err != nil {
		HandleError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ResetMFA is P-34 (§16 OQ-8/F6) — POST .../members/:user_id/reset-mfa.
// Clears the target user's MFA credentials via the Realm Provisioner
// (RP-9). Fail-closed: an RP outage surfaces as 503, never a silent no-op.
//
// @Summary      P-34 — Reset a member's MFA
// @Description  Clears the target user's TOTP/WebAuthn credentials via the Realm Provisioner (RP-9); their next login forces re-enrollment. Fail-closed on RP outage (§16 OQ-8). Emits MFAReset for the Audit Log consumer.
// @Tags         members
// @Param        id       path  string  true  "Tenant UUID"   format(uuid)
// @Param        user_id  path  string  true  "User UUID"     format(uuid)
// @Success      204
// @Failure      404      {object}  ErrorResponse  "member_not_found"
// @Failure      422      {object}  ErrorResponse  "member_not_active"
// @Failure      503      {object}  ErrorResponse  "realm_provisioner_unavailable"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/members/{user_id}/reset-mfa [post]
func (h *MembershipHandler) ResetMFA(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
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
	if err := h.svc.ResetUserMFA(c.Request.Context(), tenantID, userID, rc.UserID); err != nil {
		HandleError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// RemovalResolution is P-26 (§8.8.3) — POST .../removal-resolution.
// Body: {action: "replace_delegate"|"stop_workflows", replacement_user_id?: uuid}
// On success the caller may retry P-8; a subsequent 409 means another
// delegation appeared and the caller resubmits (WFI-6 race-safety).
//
// @Summary      P-26 — Resolve blocked user removal
// @Description  Reassigns or cancels workflows the target user is delegate on so P-8 can proceed (§8.8.3, WFI-6).
// @Tags         resolution
// @Accept       json
// @Param        id       path  string                       true  "Tenant UUID"   format(uuid)
// @Param        user_id  path  string                       true  "User UUID"     format(uuid)
// @Param        body     body  RemovalResolutionRequest     true  "action + replacement"
// @Success      204
// @Failure      422      {object}  ErrorResponse  "invalid_action | invalid_replacement"
// @Failure      503      {object}  ErrorResponse  "workflow_service_unavailable"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/users/{user_id}/removal-resolution [post]
func (h *MembershipHandler) RemovalResolution(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
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
	var req RemovalResolutionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	rc, _ := requestctx.FromContext(c.Request.Context())
	if err := h.svc.RemovalResolution(c.Request.Context(), tenantID, userID, service.RemovalAction(req.Action), req.ReplacementUserID, rc.UserID); err != nil {
		HandleError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ── shared helpers ─────────────────────────────────────────────────────

func parseUUIDParam(c *gin.Context, name string) (uuid.UUID, error) {
	raw := c.Param(name)
	if raw == "" {
		return uuid.Nil, domain.NewError(domain.ErrValidation, name+" is required").
			WithDetails(map[string]any{"code": "invalid_uuid"})
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, domain.NewError(domain.ErrValidation, name+" is not a valid UUID").
			WithDetails(map[string]any{"code": "invalid_uuid"})
	}
	return id, nil
}

func requireSameTenantMember(c *gin.Context, tenantID uuid.UUID) error {
	rc, ok := requestctx.FromContext(c.Request.Context())
	if !ok {
		return domain.NewError(domain.ErrMissingIdentity, "missing identity")
	}
	if rc.TenantID != tenantID {
		return domain.NewError(domain.ErrInsufficientRole, "cannot access another tenant")
	}
	return nil
}

func requireTenantAdmin(c *gin.Context, tenantID uuid.UUID) error {
	if err := requireSameTenantMember(c, tenantID); err != nil {
		return err
	}
	rc, _ := requestctx.FromContext(c.Request.Context())
	if !rc.IsAdmin() {
		return domain.NewError(domain.ErrInsufficientRole, "tenant_admin or tenant_owner required")
	}
	return nil
}

func requireTenderAdminOrHigher(c *gin.Context, tenantID uuid.UUID) error {
	if err := requireSameTenantMember(c, tenantID); err != nil {
		return err
	}
	rc, _ := requestctx.FromContext(c.Request.Context())
	if !rc.HasRole(string(domain.RoleTenderAdmin)) && !rc.IsAdmin() {
		return domain.NewError(domain.ErrInsufficientRole, "tender_admin, tenant_admin, or tenant_owner required")
	}
	return nil
}

func pageItemsToResponse(items []domain.MembershipListItem) []MemberItemResponse {
	out := make([]MemberItemResponse, len(items))
	for i, it := range items {
		out[i] = memberItemToResponse(it)
	}
	return out
}

func memberItemToResponse(it domain.MembershipListItem) MemberItemResponse {
	codes := make([]string, len(it.TenantRoles))
	for i, r := range it.TenantRoles {
		codes[i] = string(r)
	}
	deps := make([]DeptMemberView, len(it.Departments))
	for i, d := range it.Departments {
		deps[i] = DeptMemberView{DepartmentID: d.DepartmentID, Level: string(d.RoleLevel)}
	}
	return MemberItemResponse{
		UserID:        it.Membership.UserID,
		Status:        string(it.Membership.Status),
		TenantRoles:   codes,
		Departments:   deps,
		RecordVersion: it.Membership.RecordVersion,
		UpdatedAt:     it.Membership.UpdatedAt,
	}
}

func rolesToWire(rs []domain.TenantRole) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r.RoleCode)
	}
	return out
}
