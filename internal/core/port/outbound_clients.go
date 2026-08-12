package port

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
)

// UserProfileClient is the outbound HTTP client for the iam-user-profile
// service. Used for delegation coordination (§8.6/§8.7 availability-first
// commit ordering). Phase 4 wires the real HTTP client; Phase 2 ships a
// fail-open stub that degrades gracefully.
//
// SetAvailability variants (per UP LLD §5.3 row 18):
//   - Create:   {status:"ooo", ooo_from, ooo_until, delegate_id}
//   - End:      {delegate_id: null} — pointer-clear only (DEL-6);
//     NEVER {status:"available"} (UP owns the return-to-available
//     transition via its own OOO sweep).
type UserProfileClient interface {
	SetAvailability(ctx context.Context, req SetAvailabilityRequest) error
}

// SetAvailabilityRequest models the two shapes documented above. Fields
// left zero-value are omitted from the wire payload.
type SetAvailabilityRequest struct {
	TenantID      uuid.UUID
	UserID        uuid.UUID
	Status        *string // "ooo" on create; nil on end (pointer-clear)
	OOOFrom       *time.Time
	OOOUntil      *time.Time
	DelegateID    *uuid.UUID // *uuid.UUID{nil} → wire "delegate_id: null"; nil → field omitted
	ClearDelegate bool       // true → send delegate_id: null explicitly
	Note          string     // free-text OOO note shown in UP dashboard (≤500 chars, mapped from delegation reason)
}

// WorkflowClient calls the Workflow Service for delegate-impact checks
// (§8.8 removal-resolution gate). Phase 4 wires the real client; Phase 2
// ships a fail-open stub that returns "no active workflows".
type WorkflowClient interface {
	// GetDelegateImpact returns the count and IDs of active workflows for
	// which userID is currently the resolved assignee. The optional
	// delegationID scopes to a specific delegation (WFI-11, department-
	// scoped queries in §8.8.4).
	GetDelegateImpact(ctx context.Context, tenantID, userID uuid.UUID, delegationID *uuid.UUID) (*DelegateImpact, error)

	// ReassignDelegate reassigns active workflows from oldUserID to
	// newUserID (P-26 replace_delegate branch).
	ReassignDelegate(ctx context.Context, tenantID, oldUserID, newUserID uuid.UUID, delegationID *uuid.UUID) error

	// CancelByDelegate cancels/pauses active workflows delegated from
	// userID (P-26 stop_workflows branch).
	CancelByDelegate(ctx context.Context, tenantID, userID uuid.UUID, delegationID *uuid.UUID) error
}

// DelegateImpact is the response envelope for GetDelegateImpact.
type DelegateImpact struct {
	ActiveWorkflows int
	WorkflowIDs     []uuid.UUID
}

// RealmProvisionerClient is the outbound HTTP client for the Realm
// Provisioner service. Only PatchRealmConfig is HLD-ratified (HLD §5.2 /
// LLD §16 A7/A58). The other three methods are O&M proposals awaiting
// RP-side ownership formalization (§16 A46 for RevokeUserSessions).
//
// Phase 4 wires the real HTTP client with fail-open semantics on
// RevokeUserSessions and Option A local-first + reconcile on
// PatchRealmConfig (T-15).
type RealmProvisionerClient interface {
	// CreateInvitedUser creates a Keycloak user for a pending invitation
	// (P-6). Used inside the seat-hold tx. Idempotent by (tenant_id, email).
	CreateInvitedUser(ctx context.Context, req CreateInvitedUserRequest) (*CreateInvitedUserResponse, error)

	// DeleteUser is called by the invitation-kc-cleanup reconciler (PI-9).
	// Idempotent — a 404 on the RP side is treated as success.
	DeleteUser(ctx context.Context, tenantID, keycloakUserID uuid.UUID) error

	// PatchRealmConfig applies realm-affecting settings (local_accounts_enabled,
	// IdP/federation) via Keycloak Admin API. On non-200 the caller sets
	// realm_sync_pending=true and returns 202 to the client per LLD §16 A58 / T-15.
	PatchRealmConfig(ctx context.Context, tenantID uuid.UUID, patch RealmConfigPatch) error

	// RevokeUserSessions is the AUTH-8 privilege-reduction call. Best-effort
	// fail-open — if RP is down, the caller relies on the TTL backstop
	// (access-token lifetime + 300s membership cache eviction). §16 A46:
	// recommend-and-confirm; contract not yet ratified.
	RevokeUserSessions(ctx context.Context, tenantID, keycloakUserID uuid.UUID) error
}

type CreateInvitedUserRequest struct {
	TenantID uuid.UUID
	Email    string
	FullName string
}

type CreateInvitedUserResponse struct {
	KeycloakUserID uuid.UUID
}

type RealmConfigPatch struct {
	LocalAccountsEnabled *bool
	// Future: IdP/federation config, MFA policy, etc.
}

// _ silences the "unused domain" compile-check on some builds — we import
// domain even though this file doesn't directly reference it, for the
// type comments to be linkable.
var _ domain.TenantPlan
