// Package unit_test hosts Phase 1 unit-level assertions.
// Phase 2+ populates this with handler/service tests; Phase 1 ships a
// smoke test that the domain event constants are wired to the correct
// SNS topic per LLD §7.3.
package unit_test

import (
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/stretchr/testify/assert"
)

func TestTopicForEvent_TenantEvents(t *testing.T) {
	// Only TenantCreated and TrialStarted route to iam.tenant.events (§7.3).
	assert.Equal(t, domain.TopicTenant, domain.TopicForEvent(domain.EventTenantCreated))
	assert.Equal(t, domain.TopicTenant, domain.TopicForEvent(domain.EventTrialStarted))
}

func TestTopicForEvent_MembershipEvents(t *testing.T) {
	// Everything else routes to iam.membership.events.
	membershipTypes := []string{
		domain.EventDepartmentMembershipGranted,
		domain.EventDepartmentMembershipRevoked,
		domain.EventDepartmentMembershipLevelChanged,
		domain.EventTenantRoleGranted,
		domain.EventTenantRoleRevoked,
		domain.EventDelegationStarted,
		domain.EventDelegationEnded,
		domain.EventTenderAssigneeOverridden,
		domain.EventTenantSeatOverageStarted,
		domain.EventTenantSeatOverageResolved,
		domain.EventTenantStateChanged,
	}
	for _, ev := range membershipTypes {
		assert.Equal(t, domain.TopicMembership, domain.TopicForEvent(ev),
			"event %s must route to %s", ev, domain.TopicMembership)
	}
}

func TestTopicForEvent_UnknownDefaultsToMembership(t *testing.T) {
	// Forward-compatibility: an unknown event type defaults to the
	// membership lane (catch-all) rather than tenant, so a new consumer
	// need only opt in via SNS filter policy.
	assert.Equal(t, domain.TopicMembership, domain.TopicForEvent("SomeFuturePayload"))
}
