package postgres

import (
	"encoding/json"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/jackc/pgx/v5"
)

// scanTenant reads the tenantSelectColumns projection into a domain.Tenant.
// FeatureFlags is a jsonb column deserialised into a map[string]any.
func scanTenant(row pgx.Row) (*domain.Tenant, error) {
	var t domain.Tenant
	var featureFlagsJSON []byte
	var plan, status, realmType string
	err := row.Scan(
		&t.ID, &t.Slug, &t.Name, &plan, &featureFlagsJSON, &status,
		&t.TrialEndsAt, &t.TrialReactivationCount, &t.SubscriptionStartedAt,
		&t.CancelledAt, &t.LastEventAt,
		&t.RealmID, &realmType, &t.KeycloakShard, &t.MFAFreshnessSeconds,
		&t.LocalAccountsEnabled, &t.RealmSyncPending, &t.DefaultLocale,
		&t.LicensedSeats, &t.OwnerlessSince, &t.OverageSince,
		&t.DelegationMaxDurationDays, &t.DelegationReviewWindowDays,
		&t.RecordVersion, &t.CreatedAt, &t.UpdatedAt, &t.DeletedAt,
	)
	if err != nil {
		return nil, err
	}
	t.Plan = domain.TenantPlan(plan)
	t.Status = domain.SubscriptionStatus(status)
	t.RealmType = domain.RealmType(realmType)
	if len(featureFlagsJSON) > 0 && string(featureFlagsJSON) != "null" {
		if err := json.Unmarshal(featureFlagsJSON, &t.FeatureFlags); err != nil {
			return nil, err
		}
	}
	if t.FeatureFlags == nil {
		t.FeatureFlags = map[string]any{}
	}
	return &t, nil
}
