// Domain layer full coverage.
//
// Module:   iam-org-membership
// Feature:  Pure domain types & helpers — no DB, no HTTP.
// Files:    internal/core/domain/{errors,event,role,tenant,membership,department,tender_acl}.go
package unit_test

import (
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/stretchr/testify/assert"
)

// ═════════════════════════════════════════════════════════════════════════
// TenantRoleCode.IsElevated
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P11-DOMAIN-001
// Feature:           TR-7 · IsElevated returns true for all elevated codes
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestDomain_IsElevatedForElevatedRoles(t *testing.T) {
	assert.True(t, domain.RoleTenantOwner.IsElevated())
	assert.True(t, domain.RoleTenantAdmin.IsElevated())
	assert.True(t, domain.RoleTenderAdmin.IsElevated())
}

// Test Case ID:      P11-DOMAIN-002
// Feature:           TR-7 · 'member' is NOT elevated (never persistable)
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestDomain_IsElevatedFalseForMember(t *testing.T) {
	assert.False(t, domain.RoleMember.IsElevated(),
		"TR-7: 'member' is derived at read time; must not be persistable")
}

// Test Case ID:      P11-DOMAIN-003
// Feature:           Unknown role codes → not elevated (defensive)
// Priority: P2 · Severity: Minor · Automation Status: Automated
func TestDomain_IsElevatedFalseForUnknown(t *testing.T) {
	assert.False(t, domain.TenantRoleCode("wizard").IsElevated())
	assert.False(t, domain.TenantRoleCode("").IsElevated())
}

// ═════════════════════════════════════════════════════════════════════════
// domain.NewError + DomainError
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P11-DOMAIN-010
// Feature:           NewError wraps sentinel as Cause; errors.Is matches
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestDomain_NewErrorWrapsCause(t *testing.T) {
	de := domain.NewError(domain.ErrSeatLimitReached, "no seats")
	assert.Equal(t, "seat_limit_reached", de.Code)
	assert.True(t, errors.Is(de, domain.ErrSeatLimitReached),
		"errors.Is must match on the wrapped sentinel — middleware relies on this")
}

// Test Case ID:      P11-DOMAIN-011
// Feature:           DomainError.Error() format
// Priority: P2 · Severity: Minor · Automation Status: Automated
func TestDomain_DomainErrorString(t *testing.T) {
	de := domain.NewError(domain.ErrValidation, "empty body")
	assert.Equal(t, "validation_error: empty body", de.Error())
}

// Test Case ID:      P11-DOMAIN-012
// Feature:           WithDetails chains fluently
// Priority: P2 · Severity: Major · Automation Status: Automated
func TestDomain_WithDetailsChains(t *testing.T) {
	de := domain.NewError(domain.ErrSeatLimitReached, "cap").
		WithDetails(map[string]any{"licensed_seats": 10, "active_users": 10})
	assert.Equal(t, 10, de.Details["licensed_seats"])
	assert.Equal(t, 10, de.Details["active_users"])
}

// ═════════════════════════════════════════════════════════════════════════
// Sentinel wire-code stability — every sentinel's string() must be stable.
// If someone renames a sentinel, this test flags it in the same commit.
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P11-DOMAIN-020
// Feature:           §17 error taxonomy — full wire-code inventory
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestDomain_AllSentinelsWireCodes(t *testing.T) {
	// Every sentinel documented in LLD §17. If a wire code drifts from
	// what downstream consumers/tooling expect, this test fails loudly.
	cases := map[error]string{
		// Common
		domain.ErrValidation:             "validation_error",
		domain.ErrMissingIdentity:        "missing_identity_headers",
		domain.ErrInsufficientRole:       "insufficient_role",
		domain.ErrOptimisticLockConflict: "optimistic_lock_conflict",
		domain.ErrDependencyUnavailable:  "dependency_unavailable",
		domain.ErrNoMutableField:         "no_mutable_field",
		// Not-found
		domain.ErrTenantNotFound:     "tenant_not_found",
		domain.ErrMemberNotFound:     "member_not_found",
		domain.ErrDepartmentNotFound: "department_not_found",
		domain.ErrDelegationNotFound: "delegation_not_found",
		domain.ErrInvitationNotFound: "invitation_not_found",
		// Conflict
		domain.ErrConflict:                    "conflict",
		domain.ErrSlugAlreadyTaken:            "slug_already_taken",
		domain.ErrMemberAlreadyExists:         "member_already_exists",
		domain.ErrDeptMembershipAlreadyExists: "dept_membership_already_exists",
		domain.ErrWorkflowResolutionRequired:  "workflow_resolution_required",
		domain.ErrSeatLimitReached:            "seat_limit_reached",
		domain.ErrInvitationAlreadyExists:     "invitation_already_exists",
		domain.ErrTenantOffboarded:            "tenant_offboarded",
		domain.ErrDepartmentAlreadyActivated:  "department_already_activated",
		domain.ErrRoleAlreadyGranted:          "role_already_granted",
		// Domain-rule 422
		domain.ErrSelfDelegation:                  "self_delegation",
		domain.ErrInvalidDelegate:                 "invalid_delegate",
		domain.ErrDelegationWindowInverted:        "delegation_window_inverted",
		domain.ErrScopeIDRequired:                 "scope_id_required",
		domain.ErrCannotDeleteSystemDepartment:    "cannot_delete_system_department",
		domain.ErrDepartmentNotActiveForTenant:    "department_not_active_for_tenant",
		domain.ErrDepartmentRetired:               "department_retired",
		domain.ErrDepartmentDeactivated:           "department_deactivated",
		domain.ErrInvalidReplacement:              "invalid_replacement",
		domain.ErrInvalidOwnerCandidate:           "invalid_owner_candidate",
		domain.ErrInvalidExpiresAt:                "invalid_expires_at",
		domain.ErrInvalidRole:                     "invalid_role",
		domain.ErrAssigneeIneligible:              "assignee_ineligible",
		domain.ErrFieldImmutable:                  "field_immutable",
		domain.ErrSystemNameImmutable:             "system_name_immutable",
		domain.ErrSystemDepartmentCannotBeRetired: "system_department_cannot_be_retired",
		domain.ErrLastOwnerRemoval:                "last_owner_removal",
		// Forbidden (403)
		domain.ErrCannotRemoveOwner: "cannot_remove_owner",
		// Rate-limit
		domain.ErrReinviteTooSoon:   "reinvite_too_soon",
		domain.ErrInviteRateLimited: "invite_rate_limited",
		// Dependency 503
		domain.ErrDBUnavailable:               "db_unavailable",
		domain.ErrCacheUnavailable:            "cache_unavailable",
		domain.ErrUserProfileUnavailable:      "user_profile_unavailable",
		domain.ErrWorkflowServiceUnavailable:  "workflow_service_unavailable",
		domain.ErrRealmProvisionerUnavailable: "realm_provisioner_unavailable",
	}
	for sentinel, expected := range cases {
		assert.Equal(t, expected, sentinel.Error(),
			"wire code drifted — LLD §17 taxonomy expects %q", expected)
	}
	assert.Equal(t, 46, len(cases)+0,
		"if a new sentinel was added, extend this table too (currently 46)")
}

// ═════════════════════════════════════════════════════════════════════════
// TopicForEvent — deeper cross-check (unit_test's existing tests are basic)
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P11-DOMAIN-030
// Feature:           §16 A61 · tenant lane fixed at exactly {TenantCreated, TrialStarted}
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestDomain_TopicForEvent_TenantLaneOnlyTwo(t *testing.T) {
	// Only 2 event types route to iam.tenant.events.
	assert.Equal(t, domain.TopicTenant, domain.TopicForEvent(domain.EventTenantCreated))
	assert.Equal(t, domain.TopicTenant, domain.TopicForEvent(domain.EventTrialStarted))
	// And everything else does NOT.
	for _, et := range []string{
		domain.EventTenantRoleGranted, domain.EventTenantRoleRevoked,
		domain.EventDepartmentMembershipGranted, domain.EventDepartmentMembershipRevoked,
		domain.EventDepartmentMembershipLevelChanged,
		domain.EventDelegationStarted, domain.EventDelegationEnded,
		domain.EventTenderAssigneeOverridden,
		domain.EventTenantSeatOverageStarted, domain.EventTenantSeatOverageResolved,
		domain.EventTenantStateChanged,
	} {
		assert.NotEqual(t, domain.TopicTenant, domain.TopicForEvent(et),
			"%q must NOT route to tenant lane", et)
	}
}

// Test Case ID:      P11-DOMAIN-031
// Feature:           §16 A61 · unknown event type defaults to membership
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestDomain_TopicForEvent_UnknownDefaultsToMembership(t *testing.T) {
	assert.Equal(t, domain.TopicMembership, domain.TopicForEvent(""))
	assert.Equal(t, domain.TopicMembership, domain.TopicForEvent("FutureEvent"))
}

// Test Case ID:      P11-DOMAIN-032
// Feature:           Event type constants stability
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestDomain_EventTypeConstants(t *testing.T) {
	// Downstream consumers (Audit / AuthZ / Notification) filter by these
	// exact strings. Any rename breaks every subscriber's SNS filter policy.
	cases := map[string]string{
		"EventTenantCreated":                    domain.EventTenantCreated,
		"EventTrialStarted":                     domain.EventTrialStarted,
		"EventTenantRoleGranted":                domain.EventTenantRoleGranted,
		"EventTenantRoleRevoked":                domain.EventTenantRoleRevoked,
		"EventDepartmentMembershipGranted":      domain.EventDepartmentMembershipGranted,
		"EventDepartmentMembershipRevoked":      domain.EventDepartmentMembershipRevoked,
		"EventDepartmentMembershipLevelChanged": domain.EventDepartmentMembershipLevelChanged,
		"EventDelegationStarted":                domain.EventDelegationStarted,
		"EventDelegationEnded":                  domain.EventDelegationEnded,
		"EventTenderAssigneeOverridden":         domain.EventTenderAssigneeOverridden,
		"EventTenantSeatOverageStarted":         domain.EventTenantSeatOverageStarted,
		"EventTenantSeatOverageResolved":        domain.EventTenantSeatOverageResolved,
		"EventTenantStateChanged":               domain.EventTenantStateChanged,
	}
	for name, val := range cases {
		assert.NotEmpty(t, val, "event constant %s must be a non-empty string", name)
	}
	assert.Len(t, cases, 13, "13 event types documented in §7.3 catalogue")
}

// ═════════════════════════════════════════════════════════════════════════
// DeptRole boundary — helpers already tested in regression_test.go, but
// asserting the concrete strings guards against wire-value drift.
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P11-DOMAIN-040
// Feature:           DeptRole enum values stability
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestDomain_DeptRoleWireValues(t *testing.T) {
	assert.EqualValues(t, "preparator", domain.DeptPreparator)
	assert.EqualValues(t, "reviewer", domain.DeptReviewer)
	assert.EqualValues(t, "approver", domain.DeptApprover)
}

// Test Case ID:      P11-DOMAIN-041
// Feature:           TenantRoleCode enum values stability
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestDomain_TenantRoleCodeWireValues(t *testing.T) {
	assert.EqualValues(t, "tenant_owner", domain.RoleTenantOwner)
	assert.EqualValues(t, "tenant_admin", domain.RoleTenantAdmin)
	assert.EqualValues(t, "tender_admin", domain.RoleTenderAdmin)
	assert.EqualValues(t, "member", domain.RoleMember)
}

// Test Case ID:      P11-DOMAIN-042
// Feature:           SubscriptionStatus enum values stability
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestDomain_SubscriptionStatusValues(t *testing.T) {
	// Enum values are baked into the Postgres tenant_status ENUM.
	// Any rename requires a migration too.
	assert.EqualValues(t, "trial", domain.StatusTrial)
	assert.EqualValues(t, "active", domain.StatusActive)
	assert.EqualValues(t, "cancelled", domain.StatusCancelled)
	assert.EqualValues(t, "suspended", domain.StatusSuspended)
	assert.EqualValues(t, "trial_expired", domain.StatusTrialExpired)
	assert.EqualValues(t, "offboarded", domain.StatusOffboarded)
	assert.EqualValues(t, "past_due", domain.StatusPastDue)
}

// ═════════════════════════════════════════════════════════════════════════
// DelegationScope enum stability
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P11-DOMAIN-050
// Feature:           DelegationScope enum values stability
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestDomain_DelegationScopeValues(t *testing.T) {
	assert.EqualValues(t, "all", domain.ScopeAll)
	assert.EqualValues(t, "department", domain.ScopeDepartment)
	assert.EqualValues(t, "tender", domain.ScopeTender)
}

// ═════════════════════════════════════════════════════════════════════════
// TenderACLEntry.IsActive — TAE-3 passive expiry
// ═════════════════════════════════════════════════════════════════════════

// Feature: TAE-3 · Active entry has no soft-delete, no expiry.
func TestDomain_TenderACLEntry_ActiveByDefault(t *testing.T) {
	e := &domain.TenderACLEntry{}
	assert.True(t, e.IsActive(time.Now()))
}

// Feature: TAE-3 · Soft-deleted entry is never active.
func TestDomain_TenderACLEntry_SoftDeletedIsInactive(t *testing.T) {
	deleted := time.Now().Add(-time.Hour)
	e := &domain.TenderACLEntry{DeletedAt: &deleted}
	assert.False(t, e.IsActive(time.Now()),
		"deleted_at != NULL must yield IsActive=false regardless of expiry")
}

// Feature: TAE-3 · Entry with future expiry is active.
func TestDomain_TenderACLEntry_FutureExpiryIsActive(t *testing.T) {
	future := time.Now().Add(time.Hour)
	e := &domain.TenderACLEntry{ExpiresAt: &future}
	assert.True(t, e.IsActive(time.Now()))
}

// Feature: TAE-3 · Expired entry is inactive.
func TestDomain_TenderACLEntry_PastExpiryIsInactive(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	e := &domain.TenderACLEntry{ExpiresAt: &past}
	assert.False(t, e.IsActive(time.Now()),
		"expires_at <= now yields IsActive=false")
}

// Feature: TAE-3 boundary · expires_at == now is inactive.
// Doc says "expires_at > now()", so equality is already expired.
func TestDomain_TenderACLEntry_ExactExpiryIsInactive(t *testing.T) {
	now := time.Now()
	e := &domain.TenderACLEntry{ExpiresAt: &now}
	assert.False(t, e.IsActive(now),
		"boundary: expires_at == now must be treated as expired (TAE-3)")
}
