package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
)

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

// DelegationCheckClient replaces the local DelegationRepository.
// FindActiveDeptDelegateForUser lookup that DeptMembershipService used
// before `delegations` moved to the standalone Delegation Service
// (ADR-0008 v2 §6.4/§7.6, iam-lld-delegation-service.md §11.5, WFI-11).
type DelegationCheckClient interface {
	// DeptDelegate returns the active delegation.id (if any) where userID
	// is the delegate for a department-scoped grant on deptID, for WFI-11
	// precision (LLD §8.8.4). Returns (nil, nil) when none exists. err is
	// non-nil ONLY when the check itself could not be performed (network/
	// timeout/5xx) — callers degrade to a nil delegationID (tenant-wide
	// impact, still correct, less precise) rather than failing the whole
	// Assign/Remove operation (LLD §11.5: "the gate degrades to
	// tenant-wide impact" on a Delegation Service outage).
	DeptDelegate(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*uuid.UUID, error)
}

// CatalogAdminClient is the outbound HTTP client for the Catalog / Admin
// Config Service (catalog-admin-config), which owns the global
// departments/plans catalogs as of ADR-0007 Wave 1. Read-only, mesh-only
// bulk endpoints (CAT-I1/CAT-I2) — this is the migration-runbook Phase 2
// "cut over reads" seam. Unlike the other outbound clients in this file,
// an unconfigured base URL is NOT a fail-open condition: fabricating
// department/plan data would be worse than erroring, so both methods
// return an error when the client isn't configured. Callers get
// resilience from service.CatalogService's cache + stale-if-error layer,
// not from this client silently inventing a safe default.
type CatalogAdminClient interface {
	// Departments returns the full global department catalog (LLD §7.1).
	Departments(ctx context.Context) ([]CatalogDepartment, error)
	// Plans returns the full three-tier plan catalog (LLD §7.2).
	Plans(ctx context.Context) ([]CatalogPlan, error)
}

// CatalogDepartment mirrors catalog-admin-config's wire shape for a
// single department row (its DepartmentResponse DTO).
type CatalogDepartment struct {
	ID            uuid.UUID
	Code          string
	Name          string
	IsSystem      bool
	IsActive      bool
	RecordVersion int64
}

// CatalogPlan mirrors catalog-admin-config's wire shape for a single plan
// row (its PlanResponse DTO).
type CatalogPlan struct {
	Code                  domain.TenantPlan
	DisplayName           string
	WorkflowTemplateLimit *int
	TenderLimit           *int
	TrialDurationDays     int
	SSOEnabled            bool
	CustomBranding        string
	FeatureSet            map[string]any
	RecordVersion         int64
}
