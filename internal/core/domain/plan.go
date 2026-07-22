package domain

import "time"

// BrandingLevel mirrors the branding_level enum (§16 A19).
type BrandingLevel string

const (
	BrandingNone BrandingLevel = "none"
	BrandingLogo BrandingLevel = "logo"
)

// Plan is the global operator entitlement catalog row (§16 A19).
// PK is the code (reuses tenant_plan enum). Operator PATCH-only (O-6).
//
// WorkflowTemplateLimit / TenderLimit are nil pointers meaning "unlimited"
// (LLD §19.3 resolution of the -1-sentinel ambiguity). NULL in the DB,
// unset in JSON.
type Plan struct {
	Code                  TenantPlan
	DisplayName           string
	WorkflowTemplateLimit *int // nil = unlimited
	TenderLimit           *int // nil = unlimited
	TrialDurationDays     int
	SSOEnabled            bool
	CustomBranding        BrandingLevel
	FeatureSet            map[string]any // baseline flags; effective = defaults ⊕ tenants.feature_flags (PLAN-6)
	RecordVersion         int64
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// PlanPatch is the partial-update payload for O-6.
type PlanPatch struct {
	DisplayName           *string
	WorkflowTemplateLimit **int // double pointer distinguishes "unset" from "set to nil (unlimited)"
	TenderLimit           **int
	TrialDurationDays     *int
	SSOEnabled            *bool
	CustomBranding        *BrandingLevel
	FeatureSet            map[string]any

	RecordVersion int64
}
