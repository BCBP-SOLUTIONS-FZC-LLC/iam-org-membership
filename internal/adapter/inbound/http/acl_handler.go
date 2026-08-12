package http

import (
	"net/http"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/gin-gonic/gin"
)

type ACLHandler struct {
	svc *service.TenderACLService
}

func NewACLHandler(svc *service.TenderACLService) *ACLHandler {
	return &ACLHandler{svc: svc}
}

// List is P-21.
//
// @Summary      P-21 — List ACL entries for a tender (active + passively expired)
// @Description  AUTH-3 tender_admin/tenant_admin/owner. Returns all non-revoked grants (TAE-7): active AND passively expired (expires_at in past but not explicitly revoked). Expired entries are visible so admins can explicitly revoke them via P-23. Check expires_at in response to distinguish active vs expired.
// @Tags         acl
// @Produce      json
// @Param        id         path      string                 true  "Tenant UUID"  format(uuid)
// @Param        tender_id  path      string                 true  "Tender UUID"  format(uuid)
// @Success      200        {object}  TenderACLListResponse
// @Failure      403        {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/tenders/{tender_id}/acl [get]
func (h *ACLHandler) List(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	tenderID, err := parseUUIDParam(c, "tender_id")
	if err != nil {
		HandleError(c, err)
		return
	}
	if err := requireTenderAdminOrHigher(c, tenantID); err != nil {
		HandleError(c, err)
		return
	}
	entries, err := h.svc.List(c.Request.Context(), tenantID, tenderID)
	if err != nil {
		HandleError(c, err)
		return
	}
	out := make([]TenderACLResponse, len(entries))
	for i, e := range entries {
		out[i] = TenderACLResponse{
			UserID: e.UserID, AccessLevel: string(e.AccessLevel),
			Reason: e.Reason, ExpiresAt: e.ExpiresAt, GrantedBy: e.GrantedBy,
		}
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

// Grant is P-22.
//
// @Summary      P-22 — Grant tender ACL (view/edit/approve)
// @Description  AUTH-3 tender_admin/tenant_admin/owner. Optional expires_at (future-only).
// @Tags         acl
// @Accept       json
// @Produce      json
// @Param        id         path      string                 true  "Tenant UUID"  format(uuid)
// @Param        tender_id  path      string                 true  "Tender UUID"  format(uuid)
// @Param        request    body      TenderACLGrantRequest  true  "Grant payload"
// @Success      201        {object}  TenderACLResponse
// @Failure      400        {object}  ErrorResponse
// @Failure      403        {object}  ErrorResponse
// @Failure      400        {object}  ErrorResponse  "invalid_access_level (bad enum value)"
// @Failure      422        {object}  ErrorResponse  "invalid_expires_at (past timestamp)"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/tenders/{tender_id}/acl [post]
func (h *ACLHandler) Grant(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	tenderID, err := parseUUIDParam(c, "tender_id")
	if err != nil {
		HandleError(c, err)
		return
	}
	if err := requireTenderAdminOrHigher(c, tenantID); err != nil {
		HandleError(c, err)
		return
	}
	var req TenderACLGrantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	rc, _ := requestctx.FromContext(c.Request.Context())
	e, err := h.svc.Grant(c.Request.Context(), tenantID, tenderID, req.UserID, domain.TenderACLLevel(req.AccessLevel), rc.UserID, req.Reason, req.ExpiresAt)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusCreated, TenderACLResponse{
		UserID: e.UserID, AccessLevel: string(e.AccessLevel),
		Reason: e.Reason, ExpiresAt: e.ExpiresAt, GrantedBy: e.GrantedBy,
	})
}

// Revoke is P-23.
//
// @Summary      P-23 — Revoke tender ACL
// @Description  AUTH-3 tender_admin/tenant_admin/owner. Soft-deletes the ACL row.
// @Tags         acl
// @Produce      json
// @Param        id         path      string  true  "Tenant UUID"  format(uuid)
// @Param        tender_id  path      string  true  "Tender UUID"  format(uuid)
// @Param        user_id    path      string  true  "User UUID"    format(uuid)
// @Success      200        {object}  TenderACLRevokeResponse
// @Failure      403        {object}  ErrorResponse
// @Failure      404        {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /tenants/{id}/tenders/{tender_id}/acl/{user_id} [delete]
func (h *ACLHandler) Revoke(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	tenderID, err := parseUUIDParam(c, "tender_id")
	if err != nil {
		HandleError(c, err)
		return
	}
	userID, err := parseUUIDParam(c, "user_id")
	if err != nil {
		HandleError(c, err)
		return
	}
	if err := requireTenderAdminOrHigher(c, tenantID); err != nil {
		HandleError(c, err)
		return
	}
	_, err = h.svc.Revoke(c.Request.Context(), tenantID, tenderID, userID)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user_id": userID, "revoked": true})
}
