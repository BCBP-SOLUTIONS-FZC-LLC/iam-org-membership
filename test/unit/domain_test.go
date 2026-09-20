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
	// TenantCreated and TrialStarted route to iam.tenant.events (§7.3).
	// TenantMembershipsPurged is Core's own signal and routes to
	// iam.membership.events instead — see TestTopicForEvent_MembershipEvents.
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
		domain.EventMembershipRevoked,
		domain.EventTenantMembershipsPurged,
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

func TestIsProducedEvent_OutboundCatalogue(t *testing.T) {
	produced := []string{
		domain.EventDepartmentMembershipGranted,
		domain.EventDepartmentMembershipRevoked,
		domain.EventDepartmentMembershipLevelChanged,
		domain.EventTenantRoleGranted,
		domain.EventTenantRoleRevoked,
		domain.EventTenderAssigneeOverridden,
		domain.EventMFAReset,
		domain.EventTenantSeatOverageStarted,
		domain.EventTenantSeatOverageResolved,
		domain.EventTenantStateChanged,
		domain.EventMembershipRevoked,
		domain.EventTenantMembershipsPurged,
		domain.EventTenantCreated,
		domain.EventTrialStarted,
	}
	assert.Len(t, produced, 14)
	for _, ev := range produced {
		assert.True(t, domain.IsProducedEvent(ev), "produced event %s must be recognized", ev)
	}
	assert.False(t, domain.IsProducedEvent("TenantOffboarded"),
		"consumed producer-owned events must not be treated as produced")
	assert.False(t, domain.IsProducedEvent("mfa_reset"),
		"snake_case extract stems are not Glue names")
}
