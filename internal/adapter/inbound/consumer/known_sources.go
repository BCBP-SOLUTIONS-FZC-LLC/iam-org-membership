package consumer

// Commercial domain envelope source identifiers — CD-17 (Commercial HLD v0.10).
//
// These are the agreed stable names for the three Commercial domain services.
// The repo name, service name, envelope source, and metric prefix are all the
// same string for each service, per the IAM platform convention.
//
// Usage: use these constants whenever consumer code needs to reference a
// Commercial service by its envelope source (e.g. in allow-lists, metric
// labels, or actor mappings). Do NOT compare against old names such as
// "iam-billing", "billing", "usage-metering", or "entitlements" — those are
// retired identifiers that no deployed service uses.
//
// Note: iam-org-membership's billing-orgm-q consumer does NOT validate or
// branch on the envelope source field (env.Source). It routes exclusively on
// env.Type. These constants exist so any future validation or routing code
// has a single source of truth rather than scattered string literals.
const (
	// SourceCommercialBilling is the envelope source emitted by the
	// Billing Service on billing.events (TenantPlanChanged,
	// TenantPaymentPastDue, TenantSubscriptionCancelled, TenantReactivated,
	// TenantSeatsChanged). Replaces the retired name "iam-billing".
	SourceCommercialBilling = "commercial-billing"

	// SourceCommercialEntitlements is the envelope source emitted by the
	// Entitlements Service on entitlement.events (TenantEntitlementsChanged).
	// Replaces the retired names "entitlements" and "iam-entitlements".
	SourceCommercialEntitlements = "commercial-entitlements"

	// SourceCommercialUsageMetering is the envelope source emitted by the
	// Usage & Metering Service on usage.events (TenantQuotaWarning,
	// TenantQuotaExceeded, TenantQuotaRestored, UsagePeriodClosed).
	// Replaces the retired names "usage-metering" and "iam-usage-metering".
	SourceCommercialUsageMetering = "commercial-usage-metering"
)
