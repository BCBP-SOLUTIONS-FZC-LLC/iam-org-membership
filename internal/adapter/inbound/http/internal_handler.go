package http

import (
	"errors"
	"net/http"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// InternalHandler covers I-1 (POST /tenants), I-2 (PATCH /tenants/:id),
// I-4 (PATCH /tenants/:id/members/:user_id), I-5 (DELETE /tenants/:id/members/:user_id),
// I-8 (GET /users/:id/memberships — hot path), I-9 (locale),
// I-11 (seat-usage service-to-service, same handler as P-27), I-13
// (assignee-override validate-and-emit), I-14 (GET
// /tenants/:id/mfa-freshness — §16 A72, AuthZ Enrichment step-up gate).
// I-12 (tender ACL check) retired ADR-0007 Wave 3 Phase 6 — moved to
// iam-tender-acl's TAC-4; ID never reused.
//
// All internal routes are gated by RequireSystemRole middleware (RLS-5,
// IAPI-2, AUTH-5).
type InternalHandler struct {
	provisioning      *service.ProvisioningService
	authz             *service.AuthZService
	membership        *service.MembershipService
	invitation        *service.InvitationService
	groupMappings     *service.GroupMappingService
	tenants           *service.TenantService
	subscriptionLapse *service.SubscriptionLapseService
}

func NewInternalHandler(
	prov *service.ProvisioningService,
	authz *service.AuthZService,
	mem *service.MembershipService,
	inv *service.InvitationService,
	gm *service.GroupMappingService,
	tenants *service.TenantService,
	subscriptionLapse *service.SubscriptionLapseService,
) *InternalHandler {
	return &InternalHandler{
		provisioning: prov, authz: authz, membership: mem, invitation: inv,
		groupMappings: gm, tenants: tenants, subscriptionLapse: subscriptionLapse,
	}
}

// ── I-1 POST /tenants ─────────────────────────────────────────────────

type internalProvisionRequest struct {
	TenantID      uuid.UUID `json:"tenant_id"`
	Slug          string    `json:"slug"`
	Name          string    `json:"name"`
	Plan          string    `json:"plan"`
	OwnerUserID   uuid.UUID `json:"owner_user_id"`
	DefaultLocale string    `json:"default_locale,omitempty"`
	LicensedSeats int       `json:"licensed_seats"`
}

// ProvisionTenant is I-1 — trial signup (5 depts + 3 labels + owner + 3 events in one tx).
//
// @Summary      I-1 — Provision tenant (trial signup)
// @Description  Activates the 5 default system departments, creates 3 dept-role labels, grants tenant_owner, and emits TenantCreated + TrialStarted in one transaction. Called directly by the Signup BFF / Realm Provisioner over the internal mesh — every tenant is created trial-shaped regardless of plan; a consumed TrialTenantProvisioned event, if received, is an idempotent no-op reconcile, not a trigger for this handler.
// @Tags         internal
// @Accept       json
// @Produce      json
// @Param        request  body      InternalProvisionRequest  true  "Provisioning payload"
// @Success      201      {object}  TenantResponse  "Fresh create"
// @Success      200      {object}  TenantResponse  "Idempotent replay (LLD I1-1: same id already provisioned)"
// @Failure      400      {object}  ErrorResponse   "validation_error (missing/malformed required fields)"
// @Failure      403      {object}  ErrorResponse   "insufficient_role (non iam-system caller)"
// @Failure      409      {object}  ErrorResponse   "slug_already_taken (LLD I1-2)"
// @Failure      422      {object}  ErrorResponse   "invalid_plan (plan not in {starter,pro,enterprise})"
// @Failure      500      {object}  ErrorResponse   "internal_error"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /internal/tenants [post]
func (h *InternalHandler) ProvisionTenant(c *gin.Context) {
	var req internalProvisionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	// LLD line 2456-2461: tenant_id, slug, name, plan, owner_user_id are all required.
	if req.TenantID == uuid.Nil || req.Slug == "" || req.Name == "" || req.OwnerUserID == uuid.Nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "tenant_id, slug, name, owner_user_id are required"))
		return
	}
	if req.Plan == "" {
		HandleError(c, domain.NewError(domain.ErrValidation, "plan is required"))
		return
	}
	// Plan-value whitelist — reject unknown tiers with 422 before the DB
	// ENUM cast fails as a raw pgconn.PgError → generic 500 (§5.5, §16 A62).
	switch domain.TenantPlan(req.Plan) {
	case domain.PlanStarter, domain.PlanPro, domain.PlanEnterprise:
	default:
		HandleError(c, domain.NewError(domain.ErrInvalidPlan,
			"plan must be one of starter, pro, enterprise").
			WithDetails(map[string]any{"received": req.Plan}))
		return
	}
	t, wasCreated, err := h.provisioning.TrialSignup(c.Request.Context(), service.TrialSignupInput{
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
	// LLD I-1: 201 on fresh create; 200 on idempotent replay when the id
	// already existed (ON CONFLICT (id) DO NOTHING at the repo layer).
	status := http.StatusCreated
	if !wasCreated {
		status = http.StatusOK
	}
	c.JSON(status, TenantToResponse(t))
}

// ── I-2 PATCH /tenants/:id (RP realm fields) ───────────────────────────

type internalRealmPatchRequest struct {
	RealmID       string `json:"realm_id"`
	RealmType     string `json:"realm_type"`
	KeycloakShard string `json:"keycloak_shard"`
	RecordVersion int64  `json:"record_version"` // CONC-4 optimistic lock (BUG-I2-2)
}

// PatchTenantRealm is I-2 — Realm Provisioner sets realm_id + realm_type + keycloak_shard atomically.
//
// @Summary      I-2 — RP sets realm_id + realm_type + keycloak_shard atomically
// @Description  Called by Realm Provisioner after Keycloak realm creation. Sets the tenant's realm identity in a single UPDATE. All three fields (realm_id, realm_type, keycloak_shard) are required per LLD §5.4 I-2.
// @Tags         internal
// @Accept       json
// @Produce      json
// @Param        id       path      string                       true  "Tenant UUID"  format(uuid)
// @Param        request  body      InternalRealmPatchRequest    true  "Realm identity payload"
// @Success      200      {object}  InternalTenantRealmResponse
// @Failure      400      {object}  ErrorResponse  "validation_error (missing required field)"
// @Failure      403      {object}  ErrorResponse  "insufficient_role (non iam-system caller)"
// @Failure      404      {object}  ErrorResponse  "tenant_not_found"
// @Failure      422      {object}  ErrorResponse  "invalid_realm_type (realm_type not in {shared, dedicated})"
// @Failure      500      {object}  ErrorResponse  "internal_error"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
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
	// LLD §5.4 I-2: all three realm fields are required. Without this
	// gate an empty request lands raw pgconn CHECK-constraint failures on
	// tenants_realm_id_not_empty / tenants_keycloak_shard_not_empty and
	// a realm_type ENUM cast error, which surface as generic 500s.
	if req.RealmID == "" || req.KeycloakShard == "" || req.RealmType == "" {
		HandleError(c, domain.NewError(domain.ErrValidation,
			"realm_id, realm_type, keycloak_shard are required"))
		return
	}
	// Whitelist realm_type against the DB ENUM values so an unknown tier
	// surfaces as 422 invalid_realm_type (§5.5, mirrors §16 A62) rather
	// than a raw pgconn PgError from the enum cast.
	switch domain.RealmType(req.RealmType) {
	case domain.RealmShared, domain.RealmDedicated:
	default:
		HandleError(c, domain.NewError(domain.ErrInvalidRealmType,
			"realm_type must be one of shared, dedicated").
			WithDetails(map[string]any{"received": req.RealmType}))
		return
	}
	if err := h.provisioning.SetRealmFields(c.Request.Context(), tenantID, req.RealmID, domain.RealmType(req.RealmType), req.KeycloakShard, req.RecordVersion); err != nil {
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
// @Security     TenantRoles
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

// ── I-3 POST /tenants/:id/members ──────────────────────────────────────
//
// Called by the Event Consumer's REGISTER webhook (§8.10). If a matching
// pending_invitations row exists, flip it to accepted and materialise the
// tenant_membership + queued initial_tenant_roles/initial_dept_mappings in
// one tx (PI-4). Otherwise plain add. PI-10 idempotency: uq_tm_active_user
// + the pending→accepted flip make webhook redelivery a safe no-op.
type internalAddMemberRequest struct {
	UserID         uuid.UUID `json:"user_id"`
	KeycloakUserID uuid.UUID `json:"keycloak_user_id"`
	Email          string    `json:"email,omitempty"`
}

// AddMember is I-3 — Realm Provisioner REGISTER webhook.
//
// @Summary      I-3 — Accept invitation / plain add (Realm Provisioner webhook)
// @Description  §8.10 acceptance leg. If a matching pending_invitations row exists (by keycloak_user_id or email), flip to accepted and materialise tenant_membership + queued initial_tenant_roles/initial_dept_mappings in one tx (PI-4). Otherwise plain add. PI-10 idempotent via uq_tm_active_user.
// @Tags         internal
// @Accept       json
// @Produce      json
// @Param        id       path      string                       true  "Tenant UUID"  format(uuid)
// @Param        request  body      InternalAddMemberRequest     true  "Register payload"
// @Success      201      {object}  MembershipItemResponse
// @Failure      400      {object}  ErrorResponse
// @Failure      403      {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /internal/tenants/{id}/members [post]
func (h *InternalHandler) AddMember(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	var req internalAddMemberRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	if req.UserID == uuid.Nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "user_id is required"))
		return
	}
	kcID := req.KeycloakUserID
	if kcID == uuid.Nil {
		kcID = req.UserID
	}
	mem, err := h.invitation.AddFromRegister(c.Request.Context(), tenantID, req.UserID, kcID, req.Email)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"tenant_id":      mem.TenantID,
		"user_id":        mem.UserID,
		"status":         mem.Status,
		"record_version": mem.RecordVersion,
	})
}

// ── I-10 POST /tenants/:id/dept-memberships ────────────────────────────
//
// SAML JIT dept + tenant-role assignment (§8.5, GTRM-4). Called by the
// Event Consumer on Keycloak login for federated (SAML) users. Additive
// only — never revokes a role that a group set no longer implies.
type internalJITRequest struct {
	UserID uuid.UUID `json:"user_id"`
	Groups []string  `json:"groups"`
}

// AssignFromGroups is I-10 — SAML JIT provisioning.
//
// @Summary      I-10 — SAML JIT provisioning (additive-only, GTRM-4)
// @Description  §8.5 JIT dept + tenant-role assignment. Called by the Event Consumer on Keycloak login for federated (SAML) users. Additive only — never revokes a role a group set no longer implies. Emits DepartmentMembershipGranted / DepartmentMembershipLevelChanged per resolved (dept, role_level) touch per TRG-3.
// @Tags         internal
// @Accept       json
// @Produce      json
// @Param        id       path      string                 true  "Tenant UUID"  format(uuid)
// @Param        request  body      InternalJITRequest     true  "JIT payload — user_id + Keycloak group list"
// @Success      200      {object}  InternalJITResponse
// @Failure      400      {object}  ErrorResponse
// @Failure      403      {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /internal/tenants/{id}/dept-memberships [post]
func (h *InternalHandler) AssignFromGroups(c *gin.Context) {
	tenantID, err := parseTenantIDParam(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	var req internalJITRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	if req.UserID == uuid.Nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "user_id is required"))
		return
	}
	res, err := h.groupMappings.AssignFromGroups(c.Request.Context(), tenantID, req.UserID, req.Groups)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
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
// @Security     TenantRoles
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
// @Description  SLO 15 ms cache-hit / 30 ms miss (§21). Derived `member` role injected (TR-7). feature_flags is the sorted list of enabled flag names from planDefaults(plan) ⊕ tenants.feature_flags. Cache TTL 300s ± 30s jitter.
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
// @Security     TenantRoles
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
// @Security     TenantRoles
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

// ── I-14 GET /tenants/:id/mfa-freshness ────────────────────────────────
//
// Returns the tenant's mfa_freshness_seconds from the om:tenant cache
// (same key P-2 evicts). AuthZ Enrichment uses this as the authoritative
// read path for the Approver step-up gate (§16 A72, AE-16). Modeled on I-9.

// GetMFAFreshness is I-14 — tenant mfa_freshness_seconds (AuthZ Enrichment step-up gate).
//
// @Summary      I-14 — Tenant mfa_freshness_seconds (AuthZ Enrichment)
// @Description  Returns mfa_freshness_seconds from the om:tenant cache (same key P-2 evicts). Authoritative read path for the Approver step-up gate — preferred over the I-8 per-user snapshot (CACHE-9, §16 A72).
// @Tags         internal
// @Produce      json
// @Param        id   path      string                          true  "Tenant UUID"  format(uuid)
// @Success      200  {object}  InternalMFAFreshnessResponse
// @Failure      403  {object}  ErrorResponse
// @Failure      404  {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /internal/tenants/{id}/mfa-freshness [get]
func (h *InternalHandler) GetMFAFreshness(c *gin.Context) {
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
	c.JSON(http.StatusOK, gin.H{"tenant_id": tenantID, "mfa_freshness_seconds": t.MFAFreshnessSeconds})
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
// @Security     TenantRoles
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

// I-12 (GET /tenants/:id/tenders/:tender_id/acl/:user_id, "tender ACL
// check") retired ADR-0007 Wave 3 Phase 6 — moved to iam-tender-acl's
// TAC-4. ID never reused.

// ── GET /tenants/:id/members/:user_id/exists ───────────────────────────

// CheckMemberExists backs iam-tender-acl's grant-time membership-existence
// check (ADR-0007 Wave 3 extraction, Phase 3 — see
// iam-tender-acl/O_AND_M_DELTA.md §4). It replaces the composite FK
// (tender_acl_entries.tenant_membership_id → tenant_memberships) that
// could not survive tender_acl_entries moving to its own database.
//
// This is an existence check, not a resource fetch: a member who does not
// exist, or exists but is not MembershipActive, is a normal 200 response
// (active:false), never a 404 — the caller (iam-tender-acl's TAC-2 grant
// flow) treats both cases identically as "block the grant", and a 404
// would collapse into the same generic-error handling as a genuine
// dependency failure on the caller's side.
//
// @Summary      Membership existence/active-status check (iam-tender-acl grant-time dependency)
// @Description  Returns `{active, tenant_membership_id}` — tenant_membership_id is populated only when active is true. Never 404.
// @Tags         internal
// @Produce      json
// @Param        id       path      string  true  "Tenant UUID"  format(uuid)
// @Param        user_id  path      string  true  "User UUID"    format(uuid)
// @Success      200      {object}  MemberExistsResponse
// @Failure      400      {object}  ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /internal/tenants/{id}/members/{user_id}/exists [get]
func (h *InternalHandler) CheckMemberExists(c *gin.Context) {
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

	// LLD §11.2 doesn't give I-15 any caller-identifying signal today (both
	// iam-tender-acl and iam-delegation authenticate as the same
	// "iam-system" internal principal) — labelled "unknown" rather than
	// guessing which service made the call.
	const caller = "unknown"

	m, err := h.membership.CheckActiveMembership(c.Request.Context(), tenantID, userID)
	if err != nil {
		if errors.Is(err, domain.ErrMemberNotFound) {
			metrics.IncMembershipExistsCheck(caller, "inactive")
			c.JSON(http.StatusOK, gin.H{"active": false, "tenant_membership_id": nil})
			return
		}
		HandleError(c, err)
		return
	}
	if m.Status != domain.MembershipActive {
		metrics.IncMembershipExistsCheck(caller, "inactive")
		c.JSON(http.StatusOK, gin.H{"active": false, "tenant_membership_id": nil})
		return
	}
	metrics.IncMembershipExistsCheck(caller, "active")
	c.JSON(http.StatusOK, gin.H{"active": true, "tenant_membership_id": m.ID})
}

// ── I-16 GET /subscription-lapses ──────────────────────────────────────

// ListSubscriptionLapses is I-16 (§16 RP-C3 of the RP↔O&M alignment
// review): the Realm Provisioner's subscription-lapse sweep has no way to
// learn a tenant's cancelled_at, so it polls this instead of tracking
// cancellation timestamps itself. O&M does the grace-period math
// (SUBSCRIPTION_GRACE_DAYS) so the 30-day rule stays single-sourced.
//
// Cross-tenant by design — not scoped to a single :id like every other
// internal route, so it deliberately does not sit under /tenants/:id.
// Self-idempotent: once RP acts on an entry (emitting TenantSuspended),
// the tenant naturally drops off the next poll.
//
// @Summary      List tenants past their subscription-cancellation grace period (RP-C3 subscription-lapse sweep)
// @Description  Returns tenants with status='cancelled' whose cancelled_at is older than SUBSCRIPTION_GRACE_DAYS. Self-idempotent — RP suspending a tenant removes it from the next poll.
// @Tags         internal
// @Produce      json
// @Success      200 {object} SubscriptionLapseListResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /internal/subscription-lapses [get]
func (h *InternalHandler) ListSubscriptionLapses(c *gin.Context) {
	tenants, err := h.subscriptionLapse.List(c.Request.Context())
	if err != nil {
		HandleError(c, err)
		return
	}
	items := make([]SubscriptionLapseItem, len(tenants))
	for i, t := range tenants {
		var cancelledAt time.Time
		if t.CancelledAt != nil {
			cancelledAt = *t.CancelledAt
		}
		items[i] = SubscriptionLapseItem{
			TenantID:    t.ID,
			RealmID:     t.RealmID,
			RealmType:   string(t.RealmType),
			CancelledAt: cancelledAt,
		}
	}
	c.JSON(http.StatusOK, SubscriptionLapseListResponse{Tenants: items})
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
// @Security     TenantRoles
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
	// I-13 / A55 / OVR-1: validate + emit TenderAssigneeOverridden. Persists
	// nothing — the assignee_overrides record is Workflow-owned (§2.2/A32(d)).
	err = h.membership.ValidateAndEmitAssigneeOverride(
		c.Request.Context(),
		tenantID, tenderID, req.NewUserID, req.DepartmentID,
		domain.DeptRole(req.RequiredLevel), req.ActorID,
	)
	if err != nil {
		HandleError(c, err)
		return
	}
	// LLD line 2194: response shape is {eligible: true}.
	c.JSON(http.StatusOK, gin.H{
		"eligible":  true,
		"tender_id": tenderID,
		"tenant_id": tenantID,
		"user_id":   req.NewUserID,
	})
}
