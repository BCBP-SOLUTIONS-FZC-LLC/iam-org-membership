package http

import (
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
)

// ── Tenant ──────────────────────────────────────────────────────────────

// TenantResponse is the P-1 response body and the shape mutations echo
// (P-2 with the updated record_version + updated_at for CONC-4).
type TenantResponse struct {
	ID                    uuid.UUID                 `json:"id"`
	Slug                  string                    `json:"slug"`
	Name                  string                    `json:"name"`
	Plan                  domain.TenantPlan         `json:"plan"`
	Status                domain.SubscriptionStatus `json:"status"`
	TrialEndsAt           *time.Time                `json:"trial_ends_at,omitempty"`
	SubscriptionStartedAt *time.Time                `json:"subscription_started_at,omitempty"`
	RealmID               string                    `json:"realm_id"`
	RealmType             domain.RealmType          `json:"realm_type"`
	MFAFreshnessSeconds   int                       `json:"mfa_freshness_seconds"`
	LocalAccountsEnabled  bool                      `json:"local_accounts_enabled"`
	RealmSyncPending      bool                      `json:"realm_sync_pending"`
	DefaultLocale         string                    `json:"default_locale"`
	LicensedSeats         int                       `json:"licensed_seats"`
	OwnerlessSince        *time.Time                `json:"ownerless_since,omitempty"`
	OverageSince          *time.Time                `json:"overage_since,omitempty"`
	RecordVersion         int64                     `json:"record_version"`
	UpdatedAt             time.Time                 `json:"updated_at"`
}

func TenantToResponse(t *domain.Tenant) TenantResponse {
	return TenantResponse{
		ID:                    t.ID,
		Slug:                  t.Slug,
		Name:                  t.Name,
		Plan:                  t.Plan,
		Status:                t.Status,
		TrialEndsAt:           t.TrialEndsAt,
		SubscriptionStartedAt: t.SubscriptionStartedAt,
		RealmID:               t.RealmID,
		RealmType:             t.RealmType,
		MFAFreshnessSeconds:   t.MFAFreshnessSeconds,
		LocalAccountsEnabled:  t.LocalAccountsEnabled,
		RealmSyncPending:      t.RealmSyncPending,
		DefaultLocale:         t.DefaultLocale,
		LicensedSeats:         t.LicensedSeats,
		OwnerlessSince:        t.OwnerlessSince,
		OverageSince:          t.OverageSince,
		RecordVersion:         t.RecordVersion,
		UpdatedAt:             t.UpdatedAt,
	}
}

// TenantPatchRequest is P-2. All fields optional; record_version is required
// for CONC-1..4.
type TenantPatchRequest struct {
	Name                 *string `json:"name,omitempty"`
	DefaultLocale        *string `json:"default_locale,omitempty"`
	LocalAccountsEnabled *bool   `json:"local_accounts_enabled,omitempty"`
	MFAFreshnessSeconds  *int    `json:"mfa_freshness_seconds,omitempty"`
	RecordVersion        int64   `json:"record_version"`
}

func (r *TenantPatchRequest) ToDomain() *domain.TenantPatch {
	return &domain.TenantPatch{
		Name:                 r.Name,
		DefaultLocale:        r.DefaultLocale,
		LocalAccountsEnabled: r.LocalAccountsEnabled,
		MFAFreshnessSeconds:  r.MFAFreshnessSeconds,
		RecordVersion:        r.RecordVersion,
	}
}

// ── Departments ─────────────────────────────────────────────────────────

// DepartmentResponse is one entry in the P-3 list.
type DepartmentResponse struct {
	DepartmentID  uuid.UUID `json:"department_id"`
	Code          string    `json:"code"`
	Name          string    `json:"name"`
	IsSystem      bool      `json:"is_system"`
	IsActive      bool      `json:"is_active"`
	RecordVersion int64     `json:"record_version"`
}

// TenantDepartmentPatchRequest is P-25 (toggle is_active).
type TenantDepartmentPatchRequest struct {
	IsActive      *bool `json:"is_active,omitempty"`
	RecordVersion int64 `json:"record_version"`
}

// ── Membership ──────────────────────────────────────────────────────────

type MemberItemResponse struct {
	UserID        uuid.UUID        `json:"user_id"`
	Status        string           `json:"status"`
	TenantRoles   []string         `json:"tenant_roles"`
	Departments   []DeptMemberView `json:"departments"`
	RecordVersion int64            `json:"record_version"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

type DeptMemberView struct {
	DepartmentID uuid.UUID `json:"department_id"`
	Level        string    `json:"level"`
}

// MembershipPatchRequest is P-7 (suspend/reactivate).
type MembershipPatchRequest struct {
	Status        *string `json:"status,omitempty"`
	RecordVersion int64   `json:"record_version"`
}

// RolesPutRequest is P-28 (full-replacement multi-role reconcile).
type RolesPutRequest struct {
	Roles []string `json:"roles"`
}

// ── Dept memberships ───────────────────────────────────────────────────

type DeptMembershipPutRequest struct {
	Level string `json:"level"`
}

// ── Role labels ────────────────────────────────────────────────────────

type RoleLabelResponse struct {
	RoleCode      string `json:"role_code"`
	DisplayName   string `json:"display_name"`
	RecordVersion int64  `json:"record_version"`
}

type RoleLabelPatchRequest struct {
	DisplayName   string `json:"display_name"`
	RecordVersion int64  `json:"record_version"`
}

// ── Group mappings ─────────────────────────────────────────────────────

type GroupDeptRoleMappingWire struct {
	KeycloakGroupName string `json:"keycloak_group_name"`
	RoleCode          string `json:"role_code"`
}

type GroupTenantRoleMappingWire struct {
	KeycloakGroupName string `json:"keycloak_group_name"`
	RoleCode          string `json:"role_code"`
}

type GroupDeptMappingWire struct {
	KeycloakGroupName string    `json:"keycloak_group_name"`
	DepartmentID      uuid.UUID `json:"department_id"`
}

// ── Delegation ─────────────────────────────────────────────────────────

type DelegationCreateRequest struct {
	DelegateID uuid.UUID  `json:"delegate_id"`
	Scope      string     `json:"scope"`
	ScopeID    *uuid.UUID `json:"scope_id,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	StartsAt   *time.Time `json:"starts_at,omitempty"`
	EndsAt     *time.Time `json:"ends_at,omitempty"`
}

type DelegationResponse struct {
	ID            uuid.UUID  `json:"id"`
	DelegatorID   uuid.UUID  `json:"delegator_id"`
	DelegateID    uuid.UUID  `json:"delegate_id"`
	Scope         string     `json:"scope"`
	ScopeID       *uuid.UUID `json:"scope_id,omitempty"`
	Reason        string     `json:"reason,omitempty"`
	StartsAt      time.Time  `json:"starts_at"`
	EndsAt        *time.Time `json:"ends_at,omitempty"`
	Status        string     `json:"status"`
	RecordVersion int64      `json:"record_version"`
}

// ── Tender ACL ─────────────────────────────────────────────────────────

type TenderACLGrantRequest struct {
	UserID      uuid.UUID  `json:"user_id"`
	AccessLevel string     `json:"access_level"`
	Reason      string     `json:"reason,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

type TenderACLResponse struct {
	UserID      uuid.UUID  `json:"user_id"`
	AccessLevel string     `json:"access_level"`
	Reason      string     `json:"reason,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	GrantedBy   uuid.UUID  `json:"granted_by"`
}

// ── Invitations ────────────────────────────────────────────────────────

type InvitationCreateRequest struct {
	Email               string                         `json:"email"`
	FullName            string                         `json:"full_name"`
	InitialTenantRoles  []string                       `json:"initial_tenant_roles,omitempty"`
	InitialDeptMappings []domain.InvitationDeptMapping `json:"initial_dept_mappings,omitempty"`
}

type InvitationResponse struct {
	ID            uuid.UUID `json:"id"`
	Email         string    `json:"email"`
	FullName      string    `json:"full_name"`
	Status        string    `json:"status"`
	ExpiresAt     time.Time `json:"expires_at"`
	RecordVersion int64     `json:"record_version"`
}

// ── Operator ───────────────────────────────────────────────────────────

type OperatorDepartmentCreateRequest struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	IsSystem bool   `json:"is_system,omitempty"`
}

type OperatorDepartmentPatchRequest struct {
	Name          *string `json:"name,omitempty"`
	IsActive      *bool   `json:"is_active,omitempty"`
	RecordVersion int64   `json:"record_version"`
}

type OperatorFeatureFlagsRequest struct {
	FeatureFlags map[string]any `json:"feature_flags"`
}

type OperatorReassignOwnerRequest struct {
	NewOwnerUserID uuid.UUID `json:"new_owner_user_id"`
}

// ── Error envelope (§17) ───────────────────────────────────────────────
//
// ErrorResponse is the unified error body for all non-2xx responses.
// It mirrors the sibling iam-user-profile2 shape so tooling can correlate
// by trace/request ID. Details is populated for 422 validation failures.
type ErrorResponse struct {
	// Error is the machine-readable error code (LLD §17 error taxonomy).
	Error string `json:"error" example:"not_found"`
	// Status is the HTTP status code echoed in the body.
	Status int `json:"status" example:"404"`
	// TraceID is the OTel W3C trace ID for cross-service correlation.
	TraceID string `json:"trace_id,omitempty" example:"a3f1b2c8d4e5f60718293a4b5c6d7e8f"`
	// RequestID is the per-request UUID injected by platform-gincommon.
	RequestID string `json:"request_id,omitempty" example:"01H8XYZ0000000000000000000"`
	// Code is a legacy alias for Error kept for backwards compatibility.
	Code string `json:"code" example:"not_found"`
	// Message is a human-readable explanation of the error.
	Message string `json:"message" example:"resource not found"`
	// Details is populated with per-field errors for 422 validation failures.
	Details []ValidationError `json:"details,omitempty"`
	// RecordVersion is populated on 409 optimistic_lock_conflict responses
	// so the client can re-read and retry (CONC-4).
	RecordVersion *int64 `json:"record_version,omitempty" example:"5"`
	// ActiveWorkflows is populated on 409 workflow_resolution_required
	// responses (§8.8). Count of open workflows blocking the mutation.
	ActiveWorkflows *int `json:"active_workflows,omitempty" example:"3"`
	// WorkflowIDs is populated on 409 workflow_resolution_required responses.
	WorkflowIDs []uuid.UUID `json:"workflow_ids,omitempty"`
	// AllowedActions lists the P-26 resolution actions the caller may invoke.
	AllowedActions []string `json:"allowed_actions,omitempty" example:"replace_delegate,stop_workflows"`
}

// ValidationError is a single field-level validation failure embedded in
// ErrorResponse.Details for 422 responses.
type ValidationError struct {
	// Field is the JSON path of the failing field.
	Field string `json:"field" example:"mfa_freshness_seconds"`
	// Code is the machine-readable violation code.
	Code string `json:"code" example:"out_of_range"`
	// Message is a human-readable explanation of the field violation.
	Message string `json:"message" example:"must be between 60 and 900"`
}

// ── Swagger-only wrappers ──────────────────────────────────────────────
// These structs mirror the gin.H maps returned by list/collection handlers
// so the generated spec contains accurate JSON schemas.

// DepartmentListResponse wraps the P-3 department list payload.
type DepartmentListResponse struct {
	// Items is the collection of active departments for the tenant.
	Items []DepartmentResponse `json:"items"`
}

// TenantDepartmentActivatedResponse is the P-24 success shape.
type TenantDepartmentActivatedResponse struct {
	DepartmentID  uuid.UUID `json:"department_id" format:"uuid"`
	IsActive      bool      `json:"is_active"`
	RecordVersion int64     `json:"record_version" example:"1"`
}

// DeptMemberListItem is one row of the P-9 dept-member list.
type DeptMemberListItem struct {
	UserID uuid.UUID `json:"user_id" format:"uuid"`
	Level  string    `json:"level" enums:"preparator,reviewer,approver"`
}

// DeptMemberListResponse wraps the P-9 payload.
type DeptMemberListResponse struct {
	Items []DeptMemberListItem `json:"items"`
}

// DeptMembershipAssignResponse is the P-10 success shape.
type DeptMembershipAssignResponse struct {
	UserID        uuid.UUID `json:"user_id" format:"uuid"`
	DepartmentID  uuid.UUID `json:"department_id" format:"uuid"`
	Level         string    `json:"level" enums:"preparator,reviewer,approver"`
	RecordVersion int64     `json:"record_version" example:"1"`
}

// DeptMembershipRemoveResponse is the P-11 success shape.
type DeptMembershipRemoveResponse struct {
	UserID       uuid.UUID `json:"user_id" format:"uuid"`
	DepartmentID uuid.UUID `json:"department_id" format:"uuid"`
	Removed      bool      `json:"removed" example:"true"`
}

// MembershipListResponse wraps the P-4 payload.
type MembershipListResponse struct {
	Items      []MemberItemResponse `json:"items"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

// MembershipPatchResponse is the P-7 success shape.
type MembershipPatchResponse struct {
	UserID        uuid.UUID `json:"user_id" format:"uuid"`
	Status        string    `json:"status" enums:"active,suspended"`
	RecordVersion int64     `json:"record_version" example:"2"`
	UpdatedAt     time.Time `json:"updated_at" format:"date-time"`
}

// RolesReconcileResponse is the P-28 success shape.
type RolesReconcileResponse struct {
	Granted []string `json:"granted" example:"tenant_admin"`
	Revoked []string `json:"revoked" example:"tender_admin"`
}

// SeatUsageResponse is the P-27 / I-11 payload.
type SeatUsageResponse struct {
	ActiveUsers        int        `json:"active_users" example:"12"`
	PendingInvitations int        `json:"pending_invitations" example:"3"`
	LicensedSeats      int        `json:"licensed_seats" example:"25"`
	OverCap            bool       `json:"over_cap" example:"false"`
	OverageSince       *time.Time `json:"overage_since,omitempty" format:"date-time"`
	GraceEndsAt        *time.Time `json:"grace_ends_at,omitempty" format:"date-time"`
}

// RoleLabelListResponse wraps the P-12 payload.
type RoleLabelListResponse struct {
	Items []RoleLabelResponse `json:"items"`
}

// GroupDeptRoleMappingsRequest is the P-15 full-replacement body.
type GroupDeptRoleMappingsRequest struct {
	Mappings []GroupDeptRoleMappingWire `json:"mappings"`
}

// GroupDeptRoleMappingsResponse wraps the P-14 / P-15 payload.
type GroupDeptRoleMappingsResponse struct {
	Items []GroupDeptRoleMappingWire `json:"items"`
}

// GroupDeptMappingsRequest is the P-17 full-replacement body.
type GroupDeptMappingsRequest struct {
	Mappings []GroupDeptMappingWire `json:"mappings"`
}

// GroupDeptMappingsResponse wraps the P-16 / P-17 payload.
type GroupDeptMappingsResponse struct {
	Items []GroupDeptMappingWire `json:"items"`
}

// GroupTenantRoleMappingsRequest is the P-29 full-replacement body.
type GroupTenantRoleMappingsRequest struct {
	Mappings []GroupTenantRoleMappingWire `json:"mappings"`
}

// GroupTenantRoleMappingsResponse wraps the P-29 payload.
type GroupTenantRoleMappingsResponse struct {
	Items []GroupTenantRoleMappingWire `json:"items"`
}

// DelegationListResponse wraps the P-18 payload.
type DelegationListResponse struct {
	Items []DelegationResponse `json:"items"`
}

// TenderACLListResponse wraps the P-21 payload.
type TenderACLListResponse struct {
	Items []TenderACLResponse `json:"items"`
}

// TenderACLRevokeResponse is the P-23 success shape.
type TenderACLRevokeResponse struct {
	UserID  uuid.UUID `json:"user_id" format:"uuid"`
	Revoked bool      `json:"revoked" example:"true"`
}

// InvitationListResponse wraps the P-30 payload.
type InvitationListResponse struct {
	Items []InvitationResponse `json:"items"`
}

// InternalProvisionRequest is the I-1 body.
type InternalProvisionRequest struct {
	// TenantID is the pre-allocated tenant UUID.
	TenantID uuid.UUID `json:"tenant_id" format:"uuid"`
	// Slug is the tenant slug (immutable once set).
	Slug string `json:"slug" example:"acme"`
	// Name is the display name.
	Name string `json:"name" example:"Acme Corp"`
	// Plan is the entitlement tier.
	Plan string `json:"plan" enums:"starter,pro,enterprise"`
	// OwnerUserID is the initial tenant_owner.
	OwnerUserID uuid.UUID `json:"owner_user_id" format:"uuid"`
	// DefaultLocale is an optional BCP-47 locale (e.g. "en-US").
	DefaultLocale string `json:"default_locale,omitempty" example:"en-US"`
}

// InternalRealmPatchRequest is the I-2 body (RP sets realm identity).
type InternalRealmPatchRequest struct {
	RealmID       string `json:"realm_id" example:"acme"`
	RealmType     string `json:"realm_type" enums:"shared,dedicated"`
	KeycloakShard string `json:"keycloak_shard" example:"shard-1"`
}

// InternalMembershipPatchRequest is the I-4 body.
type InternalMembershipPatchRequest struct {
	Status        string `json:"status" enums:"active,suspended"`
	RecordVersion int64  `json:"record_version" example:"1"`
}

// InternalTenantRealmResponse is the I-2 success shape.
type InternalTenantRealmResponse struct {
	TenantID  uuid.UUID `json:"tenant_id" format:"uuid"`
	RealmID   string    `json:"realm_id"`
	RealmType string    `json:"realm_type" enums:"shared,dedicated"`
}

// InternalMembershipPatchResponse is the I-4 success shape.
type InternalMembershipPatchResponse struct {
	UserID        uuid.UUID `json:"user_id" format:"uuid"`
	Status        string    `json:"status" enums:"active,suspended"`
	RecordVersion int64     `json:"record_version" example:"2"`
}

// InternalMemberDeleteResponse is the I-5 success shape.
type InternalMemberDeleteResponse struct {
	UserID  uuid.UUID `json:"user_id" format:"uuid"`
	Removed bool      `json:"removed" example:"true"`
}

// InternalLocaleResponse is the I-9 success shape.
type InternalLocaleResponse struct {
	TenantID      uuid.UUID `json:"tenant_id" format:"uuid"`
	DefaultLocale string    `json:"default_locale" example:"en-US"`
}

// InternalTenderACLCheckResponse is the I-12 success shape.
type InternalTenderACLCheckResponse struct {
	HasAccess   bool   `json:"has_access" example:"true"`
	AccessLevel string `json:"access_level,omitempty" enums:"view,edit,approve"`
}

// AssigneeOverrideRequest is the I-13 body.
type AssigneeOverrideRequest struct {
	NewUserID     uuid.UUID `json:"new_user_id" format:"uuid"`
	DepartmentID  uuid.UUID `json:"department_id" format:"uuid"`
	RequiredLevel string    `json:"required_level" enums:"preparator,reviewer,approver"`
	ActorID       uuid.UUID `json:"actor_id" format:"uuid"`
}

// AssigneeOverrideResponse is the I-13 success shape.
type AssigneeOverrideResponse struct {
	Validated bool      `json:"validated" example:"true"`
	TenderID  uuid.UUID `json:"tender_id" format:"uuid"`
	TenantID  uuid.UUID `json:"tenant_id" format:"uuid"`
	UserID    uuid.UUID `json:"user_id" format:"uuid"`
}

// OperatorDepartmentResponse is the O-1 / O-2 success shape.
type OperatorDepartmentResponse struct {
	ID            uuid.UUID `json:"id" format:"uuid"`
	Code          string    `json:"code" example:"engineering"`
	Name          string    `json:"name" example:"Engineering"`
	IsSystem      bool      `json:"is_system,omitempty"`
	IsActive      bool      `json:"is_active"`
	RecordVersion int64     `json:"record_version,omitempty" example:"1"`
}

// OperatorPlanResponse is one entry in the O-5 list / O-6 result.
type OperatorPlanResponse struct {
	Code                  string         `json:"code" enums:"starter,pro,enterprise"`
	DisplayName           string         `json:"display_name"`
	WorkflowTemplateLimit *int           `json:"workflow_template_limit,omitempty"`
	TenderLimit           *int           `json:"tender_limit,omitempty"`
	TrialDurationDays     int            `json:"trial_duration_days" example:"14"`
	SSOEnabled            bool           `json:"sso_enabled" example:"false"`
	CustomBranding        string         `json:"custom_branding,omitempty" enums:"none,logo,full"`
	FeatureSet            map[string]any `json:"feature_set,omitempty"`
	RecordVersion         int64          `json:"record_version" example:"1"`
}

// OperatorPlansListResponse wraps the O-5 payload.
type OperatorPlansListResponse struct {
	Items []OperatorPlanResponse `json:"items"`
}

// OperatorPlanPatchRequest is the O-6 body.
type OperatorPlanPatchRequest struct {
	DisplayName           *string        `json:"display_name,omitempty"`
	WorkflowTemplateLimit *int           `json:"workflow_template_limit,omitempty"`
	TenderLimit           *int           `json:"tender_limit,omitempty"`
	TrialDurationDays     *int           `json:"trial_duration_days,omitempty"`
	SSOEnabled            *bool          `json:"sso_enabled,omitempty"`
	CustomBranding        *string        `json:"custom_branding,omitempty" enums:"none,logo,full"`
	FeatureSet            map[string]any `json:"feature_set,omitempty"`
	RecordVersion         int64          `json:"record_version" example:"1"`
}

// OperatorReassignOwnerResponse is the O-7 success shape.
type OperatorReassignOwnerResponse struct {
	TenantID uuid.UUID `json:"tenant_id" format:"uuid"`
	UserID   uuid.UUID `json:"user_id" format:"uuid"`
	RoleCode string    `json:"role_code" example:"tenant_owner"`
}
