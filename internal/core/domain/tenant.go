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

// Tenant is the aggregate root for the tenant subtree. All child entities
// (memberships, roles, departments, delegations, ACLs, invitations) FK-cascade
// to this row (§4.2, T-1..T-15).
type Tenant struct {
	ID                         uuid.UUID
	Slug                       string // immutable (T-1)
	Name                       string
	Plan                       TenantPlan     // FK to plans(code)
	FeatureFlags               map[string]any // override delta only (T-9)
	Status                     SubscriptionStatus
	TrialEndsAt                *time.Time
	TrialReactivationCount     int // 0..1 (T-14)
	SubscriptionStartedAt      *time.Time
	CancelledAt                *time.Time // biconditional with status (T-11)
	LastEventAt                *time.Time // EVT-14 high-water
	RealmID                    string
	RealmType                  RealmType
	KeycloakShard              string
	MFAFreshnessSeconds        int // 60..900 (T-10)
	LocalAccountsEnabled       bool
	RealmSyncPending           bool // T-15
	DefaultLocale              string
	LicensedSeats              int        // T-8
	OwnerlessSince             *time.Time // T-13
	OverageSince               *time.Time // SEAT-5
	DelegationMaxDurationDays  int        // 1..180 (DEL-14, §16 A71): caps fixed-end span
	DelegationReviewWindowDays int        // 1..180 (DEL-14, §16 A71): tenant default for open-ended review cycle
	RecordVersion              int64
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
	DeletedAt                  *time.Time
}

// TenantPatch is the partial-update payload for P-2. Fields left nil are
// untouched. RecordVersion carries the client's expected version for
// optimistic locking (CONC-1..4).
type TenantPatch struct {
	Name                       *string
	DefaultLocale              *string
	LocalAccountsEnabled       *bool
	MFAFreshnessSeconds        *int
	DelegationMaxDurationDays  *int           // §16 A71, DEL-14: 1..180
	DelegationReviewWindowDays *int           // §16 A71, DEL-14: 1..180
	FeatureFlags               map[string]any // O-4 only; ignored on P-2

	RecordVersion int64
}
