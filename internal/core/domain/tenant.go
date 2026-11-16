package domain

import (
	"time"

	"github.com/google/uuid"
)

// TenantPlan mirrors the tenant_plan enum in the DB (§4.1).
type TenantPlan string

const (
	PlanStarter    TenantPlan = "starter"
	PlanPro        TenantPlan = "pro"
	PlanEnterprise TenantPlan = "enterprise"
)

// KnownPlans returns every tenant_plan enum value, in a fixed order. Used
// where a caller must cover each tier, e.g. the per-plan generation keys
// that a PlanCatalogChanged bumps (port.PlanGenerationKey, LLD §6.3).
func KnownPlans() []TenantPlan {
	return []TenantPlan{PlanStarter, PlanPro, PlanEnterprise}
}

// SubscriptionStatus mirrors the subscription_status enum (§4.1).
type SubscriptionStatus string

const (
	StatusTrial        SubscriptionStatus = "trial"
	StatusActive       SubscriptionStatus = "active"
	StatusPastDue      SubscriptionStatus = "past_due"
	StatusCancelled    SubscriptionStatus = "cancelled"
	StatusSuspended    SubscriptionStatus = "suspended"
	StatusTrialExpired SubscriptionStatus = "trial_expired"
	StatusOffboarded   SubscriptionStatus = "offboarded"
)

// RealmType mirrors the realm_type enum (§16 A22).
type RealmType string

const (
	RealmShared    RealmType = "shared"
	RealmDedicated RealmType = "dedicated"
)

// TenantResolution is PUB-1's result (LLD §5.4, IAM HLD §8.2.7): which
// Keycloak realm an anonymous, pre-login browser should authenticate
// against. Found=false carries no other field (PUBRES-2). TenantID is set
// only for a shared-realm (trial) tenant, whose realm name alone can't
// identify it (PUBRES-4); a dedicated-realm tenant's realm name is enough.
type TenantResolution struct {
	Found     bool
	Realm     string
	RealmType RealmType
	TenantID  *uuid.UUID
}

// SuspensionSource mirrors the suspension_source enum (T-16, new — resolves
// RP-11). Non-empty iff Status == StatusSuspended.
type SuspensionSource string

const (
	SuspensionSourceBillingLapse SuspensionSource = "billing_lapse"
	SuspensionSourceOperator     SuspensionSource = "operator"
)

// ResidencyMode is the data-residency mode for the tenant (CD-37, TL decision
// 2026-10-15). Available on ALL plans; only tenant_owner may change it.
// AuthZ Enrichment injects the effective value as x-residency-mode.
type ResidencyMode string

const (
	ResidencyModeStandard ResidencyMode = "standard"
	ResidencyModeStrict   ResidencyMode = "strict"
)

// Tenant is the aggregate root for the tenant subtree. All child entities
// this service owns (memberships, roles, department activations, invitations)
// FK-cascade to this row (§4.2, T-1..T-16). Delegations and tender-ACL entries
// no longer live here — they moved to their own services' databases (ADR-0008).
type Tenant struct {
	ID                     uuid.UUID
	Slug                   string // immutable (T-1)
	Name                   string
	Plan                   TenantPlan     // ENUM-bound; validated against om:plans (Catalog Service), no DB FK (ADR-0007 §4)
	FeatureFlags           map[string]any // override delta only (T-9)
	Status                 SubscriptionStatus
	TrialEndsAt            *time.Time
	TrialReactivationCount int // 0..1 (T-14)
	SubscriptionStartedAt  *time.Time
	CancelledAt            *time.Time        // biconditional with status/suspension_source (T-11); NOT set for an operator-sourced suspension (T-16)
	SuspensionSource       *SuspensionSource // T-16, new — resolves RP-11; non-nil iff Status == StatusSuspended
	LastEventAt            *time.Time        // EVT-14 high-water
	RealmID                string
	RealmType              RealmType
	KeycloakShard          string
	MFAFreshnessSeconds    int // 60..900 (T-10)
	LocalAccountsEnabled   bool
	RealmSyncPending       bool // T-15
	DefaultLocale          string
	LicensedSeats          int           // T-8
	OwnerlessSince         *time.Time    // T-13
	ResidencyMode          ResidencyMode // CD-37: 'standard' (default) | 'strict'
	RecordVersion          int64
	CreatedAt              time.Time
	UpdatedAt              time.Time
	DeletedAt              *time.Time
}

// TenantPatch is the partial-update payload for P-2. Fields left nil are
// untouched. RecordVersion carries the client's expected version for
// optimistic locking (CONC-1..4).
type TenantPatch struct {
	Name                 *string
	DefaultLocale        *string
	LocalAccountsEnabled *bool
	MFAFreshnessSeconds  *int
	ResidencyMode        *ResidencyMode // CD-37: tenant_owner only; nil = no-op
	FeatureFlags         map[string]any // O-4 only; ignored on P-2

	RecordVersion int64
}
