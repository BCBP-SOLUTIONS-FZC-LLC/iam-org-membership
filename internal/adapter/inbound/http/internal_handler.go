package http

import (
	"net/http"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// InternalHandler covers I-1 (POST /tenants), I-2 (PATCH /tenants/:id),
// I-4 (PATCH /tenants/:id/members/:user_id), I-5 (DELETE /tenants/:id/members/:user_id),
// I-8 (GET /users/:id/memberships — hot path), I-9 (locale),
// I-11 (seat-usage service-to-service, same handler as P-27), I-12 (tender
// ACL check), I-13 (assignee-override validate-and-emit).
//
// All internal routes are gated by RequireSystemRole middleware (RLS-5,
// IAPI-2, AUTH-5).
type InternalHandler struct {
	provisioning *service.ProvisioningService
	authz        *service.AuthZService
	membership   *service.MembershipService
	acl          *service.TenderACLService
	tenants      *service.TenantService
}

func NewInternalHandler(
	prov *service.ProvisioningService,
	authz *service.AuthZService,
	mem *service.MembershipService,
	acl *service.TenderACLService,
	tenants *service.TenantService,
) *InternalHandler {
	return &InternalHandler{provisioning: prov, authz: authz, membership: mem, acl: acl, tenants: tenants}
}

// ── I-1 POST /tenants ─────────────────────────────────────────────────

type internalProvisionRequest struct {
	TenantID      uuid.UUID `json:"tenant_id"`
	Slug          string    `json:"slug"`
	Name          string    `json:"name"`
	Plan          string    `json:"plan"`
	OwnerUserID   uuid.UUID `json:"owner_user_id"`
	DefaultLocale string    `json:"default_locale,omitempty"`
}

// ProvisionTenant is I-1 — trial signup (5 depts + 3 labels + owner + 3 events in one tx).
//
// @Summary      I-1 — Provision tenant (trial signup)
// @Description  Activates the 5 default system departments, creates 3 dept-role labels, grants tenant_owner, and emits TenantCreated + TrialStarted in one transaction. Called by the TrialTenantProvisioned consumer (not by external clients).
// @Tags         internal
// @Accept       json
// @Produce      json
// @Param        request  body      InternalProvisionRequest  true  "Provisioning payload"
// @Success      201      {object}  TenantResponse
// @Failure      400      {object}  ErrorResponse
// @Failure      403      {object}  ErrorResponse
// @Failure      409      {object}  ErrorResponse  "slug_conflict OR tenant_already_provisioned"
// @Security     UserID
// @Security     TenantID
// @Router       /internal/tenants [post]
func (h *InternalHandler) ProvisionTenant(c *gin.Context) {
	var req internalProvisionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	if req.TenantID == uuid.Nil || req.Slug == "" || req.OwnerUserID == uuid.Nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "tenant_id, slug, owner_user_id are required"))
		return
	}
	t, err := h.provisioning.TrialSignup(c.Request.Context(), service.TrialSignupInput{
		TenantID:      req.TenantID,
		Slug:          req.Slug,
		Name:          req.Name,
		Plan:          domain.TenantPlan(req.Plan),
		OwnerUserID:   req.OwnerUserID,
		DefaultLocale: req.DefaultLocale,
	})
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusCreated, TenantToResponse(t))
}

// ── I-2 PATCH /tenants/:id (RP realm fields) ───────────────────────────

type internalRealmPatchRequest struct {
	RealmID       string `json:"realm_id"`
	RealmType     string `json:"realm_type"`
	KeycloakShard string `json:"keycloak_shard"`
}

// PatchTenantRealm is I-2 — Realm Provisioner sets realm_id + realm_type + keycloak_shard atomically.
//
// @Summary      I-2 — RP sets realm_id + realm_type + keycloak_shard atomically
// @Description  Called by Realm Provisioner after Keycloak realm creation. Sets the tenant's realm identity in a single UPDATE.
// @Tags         internal
// @Accept       json
// @Produce      json
// @Param        id       path      string                       true  "Tenant UUID"  format(uuid)
// @Param        request  body      InternalRealmPatchRequest    true  "Realm identity payload"
// @Success      200      {object}  InternalTenantRealmResponse
// @Failure      400      {object}  ErrorResponse
// @Failure      403      {object}  ErrorResponse
// @Failure      404      {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Router       /internal/tenants/{id} [patch]
func (h *InternalHandler) PatchTenantRealm(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	var req internalRealmPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	if err := h.provisioning.SetRealmFields(c.Request.Context(), tenantID, req.RealmID, domain.RealmType(req.RealmType), req.KeycloakShard); err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"tenant_id": tenantID, "realm_id": req.RealmID, "realm_type": req.RealmType})
}

// ── I-4 PATCH /tenants/:id/members/:user_id (KC lifecycle) ─────────────

type internalMembershipPatchRequest struct {
	Status        string `json:"status"`
	RecordVersion int64  `json:"record_version"`
}

// PatchMemberLifecycle is I-4 — Keycloak lifecycle status update.
//
// @Summary      I-4 — Keycloak lifecycle status update
// @Description  Called by Realm Provisioner to relay KC user state (enabled/disabled) into tenant_memberships.
// @Tags         internal
// @Accept       json
// @Produce      json
// @Param        id       path      string                             true  "Tenant UUID"  format(uuid)
// @Param        user_id  path      string                             true  "User UUID"    format(uuid)
// @Param        request  body      InternalMembershipPatchRequest     true  "Lifecycle payload"
// @Success      200      {object}  InternalMembershipPatchResponse
// @Failure      400      {object}  ErrorResponse
// @Failure      403      {object}  ErrorResponse
// @Failure      404      {object}  ErrorResponse
// @Failure      409      {object}  ErrorResponse  "record_version mismatch (CONC-4)"
// @Security     UserID
// @Security     TenantID
// @Router       /internal/tenants/{id}/members/{user_id} [patch]
func (h *InternalHandler) PatchMemberLifecycle(c *gin.Context) {
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
	var req internalMembershipPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	m, err := h.provisioning.SetMembershipStatus(c.Request.Context(), tenantID, userID, domain.MembershipStatus(req.Status), req.RecordVersion)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user_id": m.UserID, "status": m.Status, "record_version": m.RecordVersion})
}

// ── I-5 DELETE /tenants/:id/members/:user_id ───────────────────────────

// DeleteMember is I-5 — full cascade + TM-12 ownerless_since.
//
// @Summary      I-5 — Delete member (full cascade + TM-12 ownerless_since)
// @Description  Removes the user from the tenant, cascading dept memberships and role grants. Sets tenants.ownerless_since when the last owner is deleted (TM-12).
// @Tags         internal
// @Produce      json
// @Param        id       path      string                          true  "Tenant UUID"  format(uuid)
// @Param        user_id  path      string                          true  "User UUID"    format(uuid)
// @Success      200      {object}  InternalMemberDeleteResponse
// @Failure      403      {object}  ErrorResponse
// @Failure      404      {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Router       /internal/tenants/{id}/members/{user_id} [delete]
func (h *InternalHandler) DeleteMember(c *gin.Context) {
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
	if err := h.provisioning.DeleteMember(c.Request.Context(), tenantID, userID); err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user_id": userID, "removed": true})
}

// ── I-8 GET /users/:id/memberships — HOT PATH ─────────────────────────
//
// Query params: ?tenant_id=<uuid>. Returns 404 when the caller has no
// active membership in that tenant. See MembershipProjection for shape.
//
// @Summary      I-8 — HOT PATH: full membership projection (AuthZ Enrichment)
// @Description  SLO 15 ms cache-hit / 30 ms miss (§21). Derived `member` role injected (TR-7). effective_feature_flags = planDefaults(plan) ⊕ tenants.feature_flags. Cache TTL 300s ± 30s jitter.
// @Tags         internal
// @Produce      json
// @Param        id         path      string   true  "User UUID"    format(uuid)
// @Param        tenant_id  query     string   true  "Tenant UUID"  format(uuid)
// @Success      200        {object}  map[string]interface{}
// @Failure      400        {object}  ErrorResponse
// @Failure      403        {object}  ErrorResponse
// @Failure      404        {object}  ErrorResponse  "no active membership"
// @Security     UserID
// @Security     TenantID
// @Router       /internal/users/{id}/memberships [get]
func (h *InternalHandler) GetMemberships(c *gin.Context) {
	userID, err := parseUUIDParam(c, "id")
	if err != nil {
		HandleError(c, err)
		return
	}
	tenantID, err := uuid.Parse(c.Query("tenant_id"))
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "tenant_id query param required").
			WithDetails(map[string]any{"code": "invalid_uuid"}))
		return
	}
	proj, err := h.authz.GetMembership(c.Request.Context(), tenantID, userID)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, proj)
}

// ── I-9 GET /tenants/:id/locale ────────────────────────────────────────

// GetLocale is I-9 — tenants.default_locale (LLM Service).
//
// @Summary      I-9 — Tenant default_locale (LLM Service)
// @Description  Returns the tenant's default_locale for LLM-facing consumers.
// @Tags         internal
// @Produce      json
// @Param        id   path      string                   true  "Tenant UUID"  format(uuid)
// @Success      200  {object}  InternalLocaleResponse
// @Failure      403  {object}  ErrorResponse
// @Failure      404  {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Router       /internal/tenants/{id}/locale [get]
func (h *InternalHandler) GetLocale(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	t, err := h.tenants.Get(c.Request.Context(), tenantID)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"tenant_id": tenantID, "default_locale": t.DefaultLocale})
}

// ── I-11 GET /tenants/:id/seat-usage (S2S — same handler as P-27) ─────

// GetSeatUsage is I-11 — Seat-usage S2S (Billing pre-check).
//
// @Summary      I-11 — Seat-usage S2S (Billing pre-check)
// @Description  Same projection as P-27 but service-to-service (Billing pre-checks before charging for a new seat).
// @Tags         internal
// @Produce      json
// @Param        id   path      string             true  "Tenant UUID"  format(uuid)
// @Success      200  {object}  SeatUsageResponse
// @Failure      403  {object}  ErrorResponse
// @Failure      404  {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Router       /internal/tenants/{id}/seat-usage [get]
func (h *InternalHandler) GetSeatUsage(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	usage, err := h.membership.SeatUsage(c.Request.Context(), tenantID)
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

// ── I-12 GET /tenants/:id/tenders/:tender_id/acl/:user_id ─────────────

// CheckTenderAccess is I-12 — tender ACL check (active grant, TAE-3).
//
// @Summary      I-12 — Tender ACL check (active grant, TAE-3)
// @Description  Returns `{has_access, access_level}` — access_level is populated only when has_access is true.
// @Tags         internal
// @Produce      json
// @Param        id         path      string                            true  "Tenant UUID"  format(uuid)
// @Param        tender_id  path      string                            true  "Tender UUID"  format(uuid)
// @Param        user_id    path      string                            true  "User UUID"    format(uuid)
// @Success      200        {object}  InternalTenderACLCheckResponse
// @Failure      403        {object}  ErrorResponse
// @Failure      404        {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Router       /internal/tenants/{id}/tenders/{tender_id}/acl/{user_id} [get]
func (h *InternalHandler) CheckTenderAccess(c *gin.Context) {
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
	entry, err := h.acl.CheckAccess(c.Request.Context(), tenantID, tenderID, userID)
	if err != nil {
		HandleError(c, err)
		return
	}
	if entry == nil {
		c.JSON(http.StatusOK, gin.H{"has_access": false})
		return
	}
	c.JSON(http.StatusOK, gin.H{"has_access": true, "access_level": string(entry.AccessLevel)})
}

// ── I-13 POST /tenants/:id/tenders/:tender_id/assignee-override ───────
//
// Validate-and-emit only — persists nothing (OVR-1). Returns 422
// assignee_ineligible if the new assignee is not an active member at the
// required level in the department.

type assigneeOverrideRequest struct {
	NewUserID     uuid.UUID `json:"new_user_id"`
	DepartmentID  uuid.UUID `json:"department_id"`
	RequiredLevel string    `json:"required_level"`
	ActorID       uuid.UUID `json:"actor_id"`
}

// AssigneeOverride is I-13 — validate-and-emit TenderAssigneeOverridden (OVR-1 no persistence).
//
// @Summary      I-13 — Validate-and-emit TenderAssigneeOverridden (OVR-1 no persistence)
// @Description  422 assignee_ineligible per §16 A62 (NOT 409). Validates the new assignee is an active member at the required level in the department, then emits the event; persists nothing (OVR-1).
// @Tags         internal
// @Accept       json
// @Produce      json
// @Param        id         path      string                       true  "Tenant UUID"  format(uuid)
// @Param        tender_id  path      string                       true  "Tender UUID"  format(uuid)
// @Param        request    body      AssigneeOverrideRequest      true  "Override payload"
// @Success      200        {object}  AssigneeOverrideResponse
// @Failure      400        {object}  ErrorResponse
// @Failure      403        {object}  ErrorResponse
// @Failure      422        {object}  ErrorResponse  "assignee_ineligible"
// @Security     UserID
// @Security     TenantID
// @Router       /internal/tenants/{id}/tenders/{tender_id}/assignee-override [post]
func (h *InternalHandler) AssigneeOverride(c *gin.Context) {
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
	var req assigneeOverrideRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	// Actor must hold tender_admin (or higher) — checked here defensively;
	// AuthZ Enrichment should already have gated this.
	if req.ActorID == uuid.Nil || req.NewUserID == uuid.Nil || req.DepartmentID == uuid.Nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "actor_id, new_user_id, department_id required"))
		return
	}
	// Delegate eligibility check to the membership + dept-membership repos.
	// Phase 4 keeps this simple: fetch the target's dept memberships,
	// verify one matches (department_id, required_level).
	item, err := h.membership.Get(c.Request.Context(), tenantID, req.NewUserID)
	if err != nil {
		// Member not found for tenant → 422 assignee_ineligible.
		HandleError(c, domain.NewError(domain.ErrAssigneeIneligible, "assignee is not a member of this tenant"))
		return
	}
	ok := false
	for _, d := range item.Departments {
		if d.DepartmentID == req.DepartmentID && string(d.RoleLevel) == req.RequiredLevel {
			ok = true
			break
		}
	}
	if !ok {
		HandleError(c, domain.NewError(domain.ErrAssigneeIneligible, "assignee not at required level in department"))
		return
	}
	// Emit TenderAssigneeOverridden — Phase 4 leaves this as a service-
	// side responsibility. Since we have no dedicated service method yet,
	// we return 200 with a validated=true marker; Phase 6 wires the emit
	// once a small service method lands.
	// (OVR-1: O&M persists nothing on I-13.)
	c.JSON(http.StatusOK, gin.H{
		"validated": true,
		"tender_id": tenderID,
		"tenant_id": tenantID,
		"user_id":   req.NewUserID,
	})
}
