package http

import (
	"net/http"
	"strconv"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type DelegationHandler struct {
	svc *service.DelegationService
}

func NewDelegationHandler(svc *service.DelegationService) *DelegationHandler {
	return &DelegationHandler{svc: svc}
}

// List is P-18 — list active delegations for the caller's tenant.
//
// @Summary      P-18 — List active delegations
// @Description  Returns active delegations for the caller's tenant (RLS-scoped).
// @Tags         delegations
// @Produce      json
// @Success      200  {object}  DelegationListResponse
// @Failure      403  {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /delegations [get]
func (h *DelegationHandler) List(c *gin.Context) {
	rc, ok := requestctx.FromContext(c.Request.Context())
	if !ok {
		HandleError(c, domain.NewError(domain.ErrMissingIdentity, "missing identity"))
		return
	}
	ds, err := h.svc.List(c.Request.Context(), rc.TenantID)
	if err != nil {
		HandleError(c, err)
		return
	}
	out := make([]DelegationResponse, len(ds))
	for i, d := range ds {
		out[i] = delegationToResponse(d)
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

// Create is P-19 — self-service delegation (AUTH-4).
//
// @Summary      P-19 — Create OOO delegation (self-service, AUTH-4)
// @Description  §8.6 availability-first — calls UP SetAvailability BEFORE inserting delegations row. On UP failure returns 422 invalid_delegate.
// @Tags         delegations
// @Accept       json
// @Produce      json
// @Param        request  body      DelegationCreateRequest  true  "Delegation payload"
// @Success      201      {object}  DelegationResponse
// @Failure      400      {object}  ErrorResponse
// @Failure      403      {object}  ErrorResponse
// @Failure      422      {object}  ErrorResponse  "invalid_delegate | delegate_unavailable | self_delegation | delegation_window_inverted | scope_id_required | delegation_start_in_past"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /delegations [post]
func (h *DelegationHandler) Create(c *gin.Context) {
	rc, ok := requestctx.FromContext(c.Request.Context())
	if !ok {
		HandleError(c, domain.NewError(domain.ErrMissingIdentity, "missing identity"))
		return
	}
	var req DelegationCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	if req.DelegateID == uuid.Nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "delegate_id is required").
			WithDetails(map[string]any{"code": "invalid_uuid"}))
		return
	}
	d, err := h.svc.Create(c.Request.Context(), rc.TenantID, rc.UserID, service.DelegationCreateInput{
		DelegateID: req.DelegateID,
		Scope:      req.Scope,
		ScopeID:    req.ScopeID,
		Reason:     req.Reason,
		StartsAt:   req.StartsAt,
		EndsAt:     req.EndsAt,
	})
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusCreated, delegationToResponse(*d))
}

// Cancel is P-20 — caller cancels own delegation, or admin cancels any.
//
// @Summary      P-20 — Cancel delegation
// @Description  §8.7 pointer-clear: calls UP with {delegate_id: null} first, then flips status to 'cancelled'. Self-service; admin can cancel any.
// @Tags         delegations
// @Produce      json
// @Param        id              path      string              true   "Delegation UUID"  format(uuid)
// @Param        record_version  query     integer             false  "Optimistic-lock version"
// @Success      200             {object}  DelegationResponse
// @Failure      403             {object}  ErrorResponse
// @Failure      404             {object}  ErrorResponse
// @Failure      409             {object}  ErrorResponse  "record_version mismatch (CONC-4)"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /delegations/{id} [delete]
func (h *DelegationHandler) Cancel(c *gin.Context) {
	id, err := parseUUIDParam(c, "id")
	if err != nil {
		HandleError(c, err)
		return
	}
	rc, ok := requestctx.FromContext(c.Request.Context())
	if !ok {
		HandleError(c, domain.NewError(domain.ErrMissingIdentity, "missing identity"))
		return
	}
	verStr := c.Query("record_version")
	v, _ := strconv.ParseInt(verStr, 10, 64)
	d, err := h.svc.Cancel(c.Request.Context(), rc.TenantID, id, v)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, delegationToResponse(*d))
}

// Extend is P-32 — push review_due_at forward for an open-ended delegation.
//
// @Summary      P-32 — Extend delegation review window
// @Description  Pushes review_due_at forward by extend_days (1..180, §16 A71). Defaults to tenant's delegation_review_window_days when omitted. Only valid for open-ended delegations (DEL-13).
// @Tags         delegations
// @Accept       json
// @Produce      json
// @Param        id       path      string                    true  "Delegation UUID"  format(uuid)
// @Param        request  body      DelegationExtendRequest   false "extend_days + record_version"
// @Success      200      {object}  DelegationResponse
// @Failure      403      {object}  ErrorResponse
// @Failure      404      {object}  ErrorResponse
// @Failure      409      {object}  ErrorResponse  "optimistic_lock_conflict"
// @Failure      422      {object}  ErrorResponse  "delegation_not_open_ended | extend_days_out_of_range"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /delegations/{id}/extend [post]
func (h *DelegationHandler) Extend(c *gin.Context) {
	id, err := parseUUIDParam(c, "id")
	if err != nil {
		HandleError(c, err)
		return
	}
	rc, ok := requestctx.FromContext(c.Request.Context())
	if !ok {
		HandleError(c, domain.NewError(domain.ErrMissingIdentity, "missing identity"))
		return
	}
	var req DelegationExtendRequest
	// Body is optional — fall back to query param for record_version if no body.
	if err := c.ShouldBindJSON(&req); err != nil {
		verStr := c.Query("record_version")
		req.RecordVersion, _ = strconv.ParseInt(verStr, 10, 64)
	}
	d, err := h.svc.Extend(c.Request.Context(), rc.TenantID, id, req.ExtendDays, req.RecordVersion)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, delegationToResponse(*d))
}

// Reassign is P-33 — reassign an open-ended delegation to a different delegate.
//
// @Summary      P-33 — Reassign delegation to a new delegate
// @Description  Ends the current delegation and creates a new one with the same scope but a different delegate.
// @Tags         delegations
// @Accept       json
// @Produce      json
// @Param        id       path      string                    true  "Delegation UUID"  format(uuid)
// @Param        request  body      DelegationReassignRequest true  "New delegate"
// @Success      201      {object}  DelegationResponse
// @Failure      400      {object}  ErrorResponse
// @Failure      403      {object}  ErrorResponse
// @Failure      404      {object}  ErrorResponse
// @Failure      422      {object}  ErrorResponse  "invalid_delegate | delegate_unavailable"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /delegations/{id}/reassign [post]
func (h *DelegationHandler) Reassign(c *gin.Context) {
	id, err := parseUUIDParam(c, "id")
	if err != nil {
		HandleError(c, err)
		return
	}
	rc, ok := requestctx.FromContext(c.Request.Context())
	if !ok {
		HandleError(c, domain.NewError(domain.ErrMissingIdentity, "missing identity"))
		return
	}
	var req DelegationReassignRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	if req.DelegateID == uuid.Nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "delegate_id is required"))
		return
	}
	d, err := h.svc.Reassign(c.Request.Context(), rc.TenantID, rc.UserID, id, req.DelegateID, req.RecordVersion)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusCreated, delegationToResponse(*d))
}

func delegationToResponse(d domain.Delegation) DelegationResponse {
	return DelegationResponse{
		ID:               d.ID,
		DelegatorID:      d.DelegatorID,
		DelegateID:       d.DelegateID,
		Scope:            string(d.Scope),
		ScopeID:          d.ScopeID,
		Reason:           d.Reason,
		StartsAt:         d.StartsAt,
		EndsAt:           d.EndsAt,
		Status:           string(d.Status),
		RecordVersion:    d.RecordVersion,
		ReviewDueAt:      d.ReviewDueAt,
		ReviewWindowDays: d.ReviewWindowDays,
	}
}
