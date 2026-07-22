package http

import (
	"net/http"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// TenantHandler wires P-1 (GET) and P-2 (PATCH) to TenantService.
type TenantHandler struct {
	svc *service.TenantService
}

func NewTenantHandler(svc *service.TenantService) *TenantHandler {
	return &TenantHandler{svc: svc}
}

// Get is P-1: any active member of the target tenant may read (AUTH-1).
// RLS enforces at DB level regardless of the header, so a cross-tenant
// request returns 404 (0 rows via policy) not 403.
//
// @Summary      P-1 — Read tenant
// @Description  AUTH-1: any active member of the target tenant.
// @Tags         tenant
// @Produce      json
// @Param        id   path      string          true  "Tenant UUID"  format(uuid)
// @Success      200  {object}  TenantResponse
// @Failure      403  {object}  ErrorResponse  "AUTH-1..8 gate failed"
// @Failure      404  {object}  ErrorResponse  "Row not visible (RLS returns 0 rows OR row absent)"
// @Security     UserID
// @Security     TenantID
// @Router       /tenants/{id} [get]
func (h *TenantHandler) Get(c *gin.Context) {
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
	// AUTH-1: caller must be reading their own tenant (API-2 mismatch → 404
	// via RLS anyway; we return 403 explicitly for clarity).
	if rc.TenantID != tenantID {
		HandleError(c, domain.NewError(domain.ErrInsufficientRole, "cannot read another tenant"))
		return
	}

	t, err := h.svc.Get(c.Request.Context(), tenantID)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, TenantToResponse(t))
}

// Patch is P-2: tenant_owner only.
//
// On local_accounts_enabled change, the service calls RP PatchRealmConfig
// synchronously. If that returns non-nil (Phase 4 wires the real client;
// Phase 2 stub always returns nil), the tenant row is written with
// realm_sync_pending=true and we return 202 Accepted per LLD §16 A58 / T-15.
// Otherwise 200 OK with the updated tenant.
//
// @Summary      P-2 — Update tenant (name, locale, local_accounts_enabled, mfa_freshness)
// @Description  AUTH-1: tenant_owner only. Applies optimistic-locking (CONC-1..4). T-10 range: mfa_freshness_seconds in [60, 900]. T-15: on local_accounts_enabled change, calls RP synchronously — if RP errors, sets realm_sync_pending=true and returns 202.
// @Tags         tenant
// @Accept       json
// @Produce      json
// @Param        id       path      string              true  "Tenant UUID"  format(uuid)
// @Param        request  body      TenantPatchRequest  true  "Patch payload"
// @Success      200      {object}  TenantResponse
// @Success      202      {object}  TenantResponse  "Accepted (T-15 deferred realm sync)"
// @Failure      400      {object}  ErrorResponse
// @Failure      403      {object}  ErrorResponse  "AUTH-1..8 gate failed"
// @Failure      409      {object}  ErrorResponse  "record_version mismatch (CONC-4)"
// @Security     UserID
// @Security     TenantID
// @Router       /tenants/{id} [patch]
func (h *TenantHandler) Patch(c *gin.Context) {
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
		HandleError(c, domain.NewError(domain.ErrInsufficientRole, "cannot update another tenant"))
		return
	}
	// AUTH-1 write: tenant_owner only (§10.4).
	if !rc.HasRole("tenant_owner") {
		HandleError(c, domain.NewError(domain.ErrInsufficientRole, "tenant_owner required"))
		return
	}

	var req TenantPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}

	updated, realmSyncDeferred, err := h.svc.Patch(c.Request.Context(), tenantID, req.ToDomain())
	if err != nil {
		HandleError(c, err)
		return
	}
	if realmSyncDeferred {
		c.JSON(http.StatusAccepted, TenantToResponse(updated))
		return
	}
	c.JSON(http.StatusOK, TenantToResponse(updated))
}

// ── shared helpers ──────────────────────────────────────────────────────

// parseTenantIDParam extracts and validates :id from the URL path.
// A malformed UUID returns 400 invalid_uuid.
func parseTenantIDParam(c *gin.Context) (uuid.UUID, error) {
	raw := c.Param("id")
	if raw == "" {
		return uuid.Nil, domain.NewError(domain.ErrValidation, "tenant id is required").
			WithDetails(map[string]any{"code": "invalid_uuid"})
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, domain.NewError(domain.ErrValidation, "tenant id is not a valid UUID").
			WithDetails(map[string]any{"code": "invalid_uuid"})
	}
	return id, nil
}
