package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tenantRow builds a scripted row matching scanTenant's Scan(...) argument
// order (26 columns) exactly.
func tenantRow(featureFlagsJSON []byte, suspensionSource *string) []any {
	now := time.Now()
	return []any{
		uuid.New(), "acme", "Acme Inc", "starter", featureFlagsJSON, "trial",
		(*time.Time)(nil), 0, (*time.Time)(nil),
		(*time.Time)(nil), suspensionSource, (*time.Time)(nil),
		"realm-1", "shared", "shard-1", 120,
		true, false, "en-US",
		10, (*time.Time)(nil), (*time.Time)(nil),
		int64(1), now, now, (*time.Time)(nil),
	}
}

func TestScanTenant_HappyPathWithFeatureFlags(t *testing.T) {
	row := &fakeRow{values: tenantRow([]byte(`{"beta":true}`), nil)}
	got, err := scanTenant(row)
	require.NoError(t, err)
	assert.Equal(t, "acme", got.Slug)
	assert.Equal(t, domain.TenantPlan("starter"), got.Plan)
	assert.Equal(t, domain.SubscriptionStatus("trial"), got.Status)
	assert.Equal(t, domain.RealmType("shared"), got.RealmType)
	assert.Equal(t, true, got.FeatureFlags["beta"])
	assert.Nil(t, got.SuspensionSource)
}

func TestScanTenant_NilFeatureFlagsJSONDefaultsToEmptyMap(t *testing.T) {
	row := &fakeRow{values: tenantRow(nil, nil)}
	got, err := scanTenant(row)
	require.NoError(t, err)
	require.NotNil(t, got.FeatureFlags)
	assert.Empty(t, got.FeatureFlags)
}

func TestScanTenant_LiteralNullFeatureFlagsJSONDefaultsToEmptyMap(t *testing.T) {
	row := &fakeRow{values: tenantRow([]byte("null"), nil)}
	got, err := scanTenant(row)
	require.NoError(t, err)
	require.NotNil(t, got.FeatureFlags)
	assert.Empty(t, got.FeatureFlags)
}

func TestScanTenant_InvalidFeatureFlagsJSONReturnsError(t *testing.T) {
	row := &fakeRow{values: tenantRow([]byte("{not-json"), nil)}
	_, err := scanTenant(row)
	require.Error(t, err)
}

func TestScanTenant_NonNilSuspensionSourceIsMapped(t *testing.T) {
	src := "operator"
	row := &fakeRow{values: tenantRow(nil, &src)}
	got, err := scanTenant(row)
	require.NoError(t, err)
	require.NotNil(t, got.SuspensionSource)
	assert.Equal(t, domain.SuspensionSource("operator"), *got.SuspensionSource)
}

func TestScanTenant_ScanErrorPassesThrough(t *testing.T) {
	scanErr := errors.New("boom")
	row := &fakeRow{err: scanErr}
	_, err := scanTenant(row)
	assert.ErrorIs(t, err, scanErr)
}
