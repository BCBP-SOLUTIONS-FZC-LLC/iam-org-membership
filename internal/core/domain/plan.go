package domain

import "time"

// BrandingLevel mirrors the branding_level enum (§16 A19).
type BrandingLevel string

const (
	BrandingNone BrandingLevel = "none"
	BrandingLogo BrandingLevel = "logo"
)

// Plan is the read-through cache's response type for the global plan
// entitlement catalog, now owned by the Catalog / Admin Config Service
// (ADR-0007 §2.2, §4.1) — no longer PATCH'd locally (O-6 retired, moved
// to Catalog Service). Populated via CatalogService's om:plans read-through
// against CatalogAdminClient (§16 A19).
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
