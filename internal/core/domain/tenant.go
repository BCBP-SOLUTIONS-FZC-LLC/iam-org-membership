package domain

import (
	"strings"
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

// DefaultSharedTrialRealm is the name of the shared Keycloak realm every
// trial tenant lives in (I-1, §8.1) when SHARED_TRIAL_REALM is unset. A
// tenant returns to it after an RP-4 conversion revert (TrialReactivated,
// RP-14). The name must match Realm Provisioner's SHARED_TRIAL_REALM, so it is
// configured, not hard-coded (LLD rev 2.64).
const DefaultSharedTrialRealm = "trial"

// DefaultTrialSeats is the trial seat allowance (T-8, HLD §6.6): what I-1
// gives a new trial tenant, and what an RP-4 conversion revert resets
// licensed_seats to (rev 2.65).
const DefaultTrialSeats = 10

// SharedTrialKeycloakShard is the shard of the shared trial realm. It stays a
// constant: there is no shared configuration for shard selection.
const SharedTrialKeycloakShard = "shard-0"

// SharedTrialRealm resolves the configured shared trial realm name:
// configured, trimmed, or DefaultSharedTrialRealm when empty.
func SharedTrialRealm(configured string) string {
	if v := strings.TrimSpace(configured); v != "" {
		return v
	}
	return DefaultSharedTrialRealm
}

// TrialReactivatedReason is the optional "reason" on Realm Provisioner's
// TrialReactivated payload: which RP flow sent it. Missing means a normal
// trial reactivation, since RP sent no reason before it added the field.
type TrialReactivatedReason string

const (
	// TrialReactivationReasonTrial is RP-10's one-time in-grace trial
	// reactivation. It counts against the TRIAL-5 cap (T-14).
	TrialReactivationReasonTrial TrialReactivatedReason = "trial_reactivation"
	// TrialReactivationReasonConversionRevert is RP-4 undoing a conversion
	// within its revert window. It is not a second trial, so it skips the
	// TRIAL-5 cap and clears subscription_started_at.
	TrialReactivationReasonConversionRevert TrialReactivatedReason = "conversion_revert"
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

// DefaultSubscriptionGraceDays is the read-only grace window after
// cancellation (SUBSCRIPTION_GRACE_DAYS, CD-23): Core suspends a cancelled
// tenant at cancelled_at + this many days (T-11).
const DefaultSubscriptionGraceDays = 30

// ReadOnlyUntil returns when a cancelled tenant's read-only grace ends and
// Core suspends it (cancelled_at + graceDays, T-11). It is nil in every
// other state, and when cancelled_at is unexpectedly unset. graceDays <= 0
// means DefaultSubscriptionGraceDays. Web-shell OM-2: the UI shows this
// date and must not compute it client-side.
func (t *Tenant) ReadOnlyUntil(graceDays int) *time.Time {
	if t == nil || t.Status != StatusCancelled || t.CancelledAt == nil {
		return nil
	}
	if graceDays <= 0 {
		graceDays = DefaultSubscriptionGraceDays
	}
	// UTC first: I-16 compares cancelled_at against now() - N days as an
	// absolute instant, so whole days must not shift across a DST change in
	// the pod's local zone.
	until := t.CancelledAt.UTC().AddDate(0, 0, graceDays)
	return &until
}
