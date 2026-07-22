package http

import (
	"net/http"
	"strconv"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/gin-gonic/gin"
)

type InvitationHandler struct {
	svc *service.InvitationService
}

func NewInvitationHandler(svc *service.InvitationService) *InvitationHandler {
	return &InvitationHandler{svc: svc}
}

// List is P-30.
//
// @Summary      P-30 — List pending invitations
// @Description  AUTH-2. Returns pending_invitations rows for the tenant (invite→accept staging).
// @Tags         invitations
// @Produce      json
// @Param        id   path      string                    true  "Tenant UUID"  format(uuid)
// @Success      200  {object}  InvitationListResponse
// @Failure      403  {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Router       /tenants/{id}/invitations [get]
func (h *InvitationHandler) List(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	if err := requireTenantAdmin(c, tenantID); err != nil {
		HandleError(c, err)
		return
	}
	invs, err := h.svc.List(c.Request.Context(), tenantID)
	if err != nil {
		HandleError(c, err)
		return
	}
	out := make([]InvitationResponse, len(invs))
	for i, inv := range invs {
		out[i] = invitationToResponse(inv)
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

// Invite is P-6 — POST /tenants/:id/members. 202 Accepted with pending row.
//
// @Summary      P-6 — Invite user (staged pending_invitation)
// @Description  AUTH-2 tenant_admin/owner. SEAT-1 transactional cap: `active + pending <= licensed_seats`. Two-step invite→accept. Returns 409 seat_limit_reached at/above cap; 409 invitation_already_exists on duplicate; 429 reinvite_too_soon (PI-11) or invite_rate_limited (PI-12).
// @Tags         invitations
// @Accept       json
// @Produce      json
// @Param        id       path      string                   true  "Tenant UUID"  format(uuid)
// @Param        request  body      InvitationCreateRequest  true  "Invite payload"
// @Success      202      {object}  InvitationResponse       "Pending row created + RP CreateInvitedUser dispatched"
// @Failure      400      {object}  ErrorResponse
// @Failure      403      {object}  ErrorResponse
// @Failure      409      {object}  ErrorResponse  "seat_limit_reached OR invitation_already_exists"
// @Failure      429      {object}  ErrorResponse  "reinvite_too_soon OR invite_rate_limited"
// @Security     UserID
// @Security     TenantID
// @Router       /tenants/{id}/members [post]
func (h *InvitationHandler) Invite(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	if err := requireTenantAdmin(c, tenantID); err != nil {
		HandleError(c, err)
		return
	}
	var req InvitationCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	roles := make([]domain.TenantRoleCode, len(req.InitialTenantRoles))
	for i, r := range req.InitialTenantRoles {
		roles[i] = domain.TenantRoleCode(r)
	}
	rc, _ := requestctx.FromContext(c.Request.Context())
	inv, err := h.svc.Invite(c.Request.Context(), tenantID, service.InvitationInput{
		Email:               req.Email,
		FullName:            req.FullName,
		InitialTenantRoles:  roles,
		InitialDeptMappings: req.InitialDeptMappings,
	}, rc.UserID)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, invitationToResponse(*inv))
}

// Revoke is P-31.
//
// @Summary      P-31 — Revoke pending invitation (sets kc_cleanup_pending, PI-9)
// @Description  AUTH-2. Marks the invitation as revoked and sets kc_cleanup_pending for durable RP compensation.
// @Tags         invitations
// @Produce      json
// @Param        id              path      string              true   "Tenant UUID"       format(uuid)
// @Param        invitation_id   path      string              true   "Invitation UUID"   format(uuid)
// @Param        record_version  query     integer             false  "Optimistic-lock version"
// @Success      200             {object}  InvitationResponse
// @Failure      403             {object}  ErrorResponse
// @Failure      404             {object}  ErrorResponse
// @Failure      409             {object}  ErrorResponse  "record_version mismatch (CONC-4)"
// @Security     UserID
// @Security     TenantID
// @Router       /tenants/{id}/invitations/{invitation_id} [delete]
func (h *InvitationHandler) Revoke(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	invID, err := parseUUIDParam(c, "invitation_id")
	if err != nil {
		HandleError(c, err)
		return
	}
	if err := requireTenantAdmin(c, tenantID); err != nil {
		HandleError(c, err)
		return
	}
	verStr := c.Query("record_version")
	v, _ := strconv.ParseInt(verStr, 10, 64)
	inv, err := h.svc.Revoke(c.Request.Context(), tenantID, invID, v)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, invitationToResponse(*inv))
}

func invitationToResponse(inv domain.PendingInvitation) InvitationResponse {
	return InvitationResponse{
		ID:            inv.ID,
		Email:         inv.Email,
		FullName:      inv.FullName,
		Status:        string(inv.Status),
		ExpiresAt:     inv.ExpiresAt,
		RecordVersion: inv.RecordVersion,
	}
}
