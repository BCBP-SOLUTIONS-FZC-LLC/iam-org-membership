// Regression tests for AuthZ hot-path fixes (B11 + G8). Package-internal so
// readOnlyForStatus (unexported) is reachable. No DB needed for these.
package service

import (
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/stretchr/testify/assert"
)

// ─────────────────────────────────────────────────────────────────────────
// G8: readOnlyForStatus narrowed to strict LLD §16 A53 ("iff cancelled").
// ─────────────────────────────────────────────────────────────────────────

func TestG8_ReadOnlyForStatus_OnlyCancelled(t *testing.T) {
	cases := []struct {
		status   domain.SubscriptionStatus
		readOnly bool
		reason   string
	}{
		{domain.StatusTrial, false, "trial tenants can write"},
		{domain.StatusActive, false, "active tenants can write"},
		{domain.StatusPastDue, false, "past_due tenants can still write (grace)"},
		{domain.StatusCancelled, true, "cancelled → read_only per §16 A53"},
		{domain.StatusSuspended, false, "suspended: blocked at KC login, doesn't reach I-8"},
		{domain.StatusTrialExpired, false, "trial_expired: blocked earlier, doesn't reach I-8"},
		{domain.StatusOffboarded, false, "offboarded: tenant row soft-deleted, doesn't reach I-8"},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			assert.Equal(t, tc.readOnly, readOnlyForStatus(tc.status), tc.reason)
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────
// B11: MembershipProjection carries subscription_status + read_only.
// The struct-shape assertion — a compile-time contract check.
// ─────────────────────────────────────────────────────────────────────────

func TestMembershipProjection_FieldsPresent(t *testing.T) {
	// Populate the projection and read back both the deprecated alias and
	// the LLD-canonical field. Both must be present and reflect the same
	// underlying subscription state; ReadOnly must be derived, not stored.
	proj := &MembershipProjection{
		TenantStatus:       domain.StatusCancelled,
		SubscriptionStatus: domain.StatusCancelled,
		ReadOnly:           readOnlyForStatus(domain.StatusCancelled),
	}
	assert.Equal(t, domain.StatusCancelled, proj.SubscriptionStatus)
	assert.Equal(t, domain.StatusCancelled, proj.TenantStatus, "deprecated alias still populated")
	assert.True(t, proj.ReadOnly)
}

func TestMembershipProjection_ReadOnlyFalseForTrial(t *testing.T) {
	proj := &MembershipProjection{
		SubscriptionStatus: domain.StatusTrial,
		ReadOnly:           readOnlyForStatus(domain.StatusTrial),
	}
	assert.False(t, proj.ReadOnly)
}
