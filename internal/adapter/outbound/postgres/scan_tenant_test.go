package postgres

// scan_tenant_test.go — unit tests for scanTenant.
//
// scanTenant has two uncovered branches beyond the early-error path:
//  1. The Scan succeeds, suspensionSource is non-nil (the *string → *SuspensionSource
//     copy at line 32-34).
//  2. featureFlagsJSON is non-nil, non-"null" JSON → json.Unmarshal path
//     (line 37-39).
//  3. featureFlagsJSON is nil / "null" or empty → default empty map path
//     (line 41-43).
//  4. featureFlagsJSON is invalid JSON → json.Unmarshal returns an error
//     (line 37-39 error arm).
//  5. The Scan itself fails → early nil,err return (line 26-28).
//
// We use the existing fakeRow helper (fakes_test.go, same package) whose
// Scan copies values via reflection — all 26 tenantSelectColumns must be
// provided in the exact order the function declares them.

import (
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minimalTenantRow returns a slice of values in the exact column order of
// tenantSelectColumns so fakeRow.Scan can populate every field without a
// length mismatch error:
//
//	id, slug, name, plan, feature_flags, status,
//	trial_ends_at, trial_reactivation_count, subscription_started_at,
//	cancelled_at, suspension_source, last_event_at,
//	realm_id, realm_type, keycloak_shard, mfa_freshness_seconds,
//	local_accounts_enabled, realm_sync_pending, default_locale,
//	licensed_seats, ownerless_since, overage_since,
//	record_version, created_at, updated_at, deleted_at  (26 total)
func minimalTenantRow(overrides ...func(vals []any)) []any {
	id := uuid.New()
	now := time.Now().UTC().Truncate(time.Second)

	vals := []any{
		// id, slug, name
		id, "test-slug", "Test Tenant",
		// plan (string, not enum — scanned into a local var then converted)
		string(domain.PlanStarter),
		// feature_flags jsonb → []byte, nil means NULL
		[]byte(nil),
		// status
		string(domain.StatusTrial),
		// trial_ends_at (*time.Time)
		(*time.Time)(nil),
		// trial_reactivation_count
		int(0),
		// subscription_started_at (*time.Time)
		(*time.Time)(nil),
		// cancelled_at (*time.Time)
		(*time.Time)(nil),
		// suspension_source (*string)
		(*string)(nil),
		// last_event_at (*time.Time)
		(*time.Time)(nil),
		// realm_id, realm_type, keycloak_shard
		"realm-123", string(domain.RealmShared), "shard-a",
		// mfa_freshness_seconds
		int(300),
		// local_accounts_enabled, realm_sync_pending
		false, false,
		// default_locale
		"en",
		// licensed_seats
		int(5),
		// ownerless_since, overage_since (*time.Time)
		(*time.Time)(nil), (*time.Time)(nil),
		// record_version
		int64(1),
		// created_at, updated_at
		now, now,
		// deleted_at (*time.Time)
		(*time.Time)(nil),
	}
	for _, fn := range overrides {
		fn(vals)
	}
	return vals
}

// TestScanTenant_ScanError verifies the early-return path when row.Scan fails
// (e.g. pgx.ErrNoRows or a type mismatch). scanTenant must return nil,err.
func TestScanTenant_ScanError(t *testing.T) {
	sentinel := errors.New("scan boom")
	row := &fakeRow{err: sentinel}
	got, err := scanTenant(row)
	require.ErrorIs(t, err, sentinel)
	assert.Nil(t, got, "must return nil tenant on scan error")
}

// TestScanTenant_MinimalSuccess covers the happy path with no nullable fields
// set. All *time.Time and *string fields are nil; feature_flags is nil (JSON
// NULL → default empty map). Lines 29-44 are all executed.
func TestScanTenant_MinimalSuccess(t *testing.T) {
	row := &fakeRow{values: minimalTenantRow()}
	got, err := scanTenant(row)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, domain.PlanStarter, got.Plan)
	assert.Equal(t, domain.StatusTrial, got.Status)
	assert.Equal(t, domain.RealmShared, got.RealmType)
	assert.Nil(t, got.SuspensionSource, "suspensionSource nil → SuspensionSource stays nil")
	assert.NotNil(t, got.FeatureFlags, "nil featureFlagsJSON must produce empty map, not nil")
	assert.Empty(t, got.FeatureFlags)
	assert.Equal(t, int64(1), got.RecordVersion)
}

// TestScanTenant_SuspensionSourceNonNil covers the branch where suspensionSource
// is a non-nil *string — lines 32-35 must copy it into *SuspensionSource.
func TestScanTenant_SuspensionSourceNonNil(t *testing.T) {
	src := string(domain.SuspensionSourceBillingLapse)
	vals := minimalTenantRow(func(v []any) {
		// suspension_source is index 10 in the 26-column slice
		v[10] = &src
		// status must be "suspended" for coherence, though scanTenant does not
		// validate the invariant — we set it to avoid a misleading test.
		v[5] = string(domain.StatusSuspended)
	})
	row := &fakeRow{values: vals}
	got, err := scanTenant(row)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.SuspensionSource, "non-nil *string must produce non-nil *SuspensionSource")
	assert.Equal(t, domain.SuspensionSourceBillingLapse, *got.SuspensionSource)
}

// TestScanTenant_FeatureFlagsPopulated covers the json.Unmarshal branch
// (lines 36-39): featureFlagsJSON is non-nil, non-"null" JSON.
func TestScanTenant_FeatureFlagsPopulated(t *testing.T) {
	flagsJSON := []byte(`{"beta_feature":true,"max_uploads":42}`)
	vals := minimalTenantRow(func(v []any) {
		// feature_flags is index 4
		v[4] = flagsJSON
	})
	row := &fakeRow{values: vals}
	got, err := scanTenant(row)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, true, got.FeatureFlags["beta_feature"])
	assert.Equal(t, float64(42), got.FeatureFlags["max_uploads"],
		"json.Unmarshal into map[string]any uses float64 for numbers")
}

// TestScanTenant_FeatureFlagsNullLiteral covers the "null" JSON string branch:
// featureFlagsJSON == []byte("null") → treated as absent → default empty map.
func TestScanTenant_FeatureFlagsNullLiteral(t *testing.T) {
	vals := minimalTenantRow(func(v []any) {
		v[4] = []byte("null")
	})
	row := &fakeRow{values: vals}
	got, err := scanTenant(row)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.NotNil(t, got.FeatureFlags)
	assert.Empty(t, got.FeatureFlags, `"null" JSON must yield an empty map, not nil`)
}

// TestScanTenant_FeatureFlagsInvalidJSON covers the json.Unmarshal error arm
// (lines 37-39): featureFlagsJSON is non-empty but not valid JSON.
func TestScanTenant_FeatureFlagsInvalidJSON(t *testing.T) {
	vals := minimalTenantRow(func(v []any) {
		v[4] = []byte("{not valid json")
	})
	row := &fakeRow{values: vals}
	got, err := scanTenant(row)
	require.Error(t, err, "invalid JSON in featureFlagsJSON must return an error")
	assert.Nil(t, got)
}

// TestScanTenant_OperatorSuspension covers the second SuspensionSource value
// ("operator") to confirm both enum variants are copyable.
func TestScanTenant_OperatorSuspension(t *testing.T) {
	src := string(domain.SuspensionSourceOperator)
	vals := minimalTenantRow(func(v []any) {
		v[5] = string(domain.StatusSuspended)
		v[10] = &src
	})
	row := &fakeRow{values: vals}
	got, err := scanTenant(row)
	require.NoError(t, err)
	require.NotNil(t, got.SuspensionSource)
	assert.Equal(t, domain.SuspensionSourceOperator, *got.SuspensionSource)
}

// TestScanTenant_AllTimeFieldsPopulated exercises the non-nil *time.Time
// columns so the reflection-based scanInto correctly copies pointer-to-time
// values. Incidentally tests the OverageSince / OwnerlessSince paths.
//
// Column index map (0-based):
//
//	6  = trial_ends_at, 8 = subscription_started_at, 9 = cancelled_at,
//	11 = last_event_at, 20 = ownerless_since, 21 = overage_since,
//	25 = deleted_at.
func TestScanTenant_AllTimeFieldsPopulated(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	vals := minimalTenantRow(func(v []any) {
		v[6] = &now  // trial_ends_at
		v[8] = &now  // subscription_started_at
		v[9] = &now  // cancelled_at
		v[11] = &now // last_event_at
		v[20] = &now // ownerless_since
		v[21] = &now // overage_since
		v[25] = &now // deleted_at
	})
	row := &fakeRow{values: vals}
	got, err := scanTenant(row)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.TrialEndsAt)
	require.NotNil(t, got.SubscriptionStartedAt)
	require.NotNil(t, got.CancelledAt)
	require.NotNil(t, got.LastEventAt)
	require.NotNil(t, got.OwnerlessSince)
	require.NotNil(t, got.OverageSince)
	require.NotNil(t, got.DeletedAt)
	assert.Equal(t, now, *got.TrialEndsAt)
}
