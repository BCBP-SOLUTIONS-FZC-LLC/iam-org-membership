// Package domain holds the core entity types, value objects, and error
// catalogue for the Org & Membership service. It imports nothing outside the
// standard library and third-party value libraries — no framework, no
// adapter, no infrastructure. See LLD §17 for the full error taxonomy.
package domain

import "errors"

// Sentinel error codes. String value = wire code returned in the response
// body. Kept as errors.New so callers can wrap+unwrap with errors.Is.
//
// Phase 0 declares only the codes required for infrastructure wiring
// (ErrDependencyUnavailable) and the shared shape (§17). Business-specific
// codes are added as their owning phase lands them:
//   - Phase 2: seat/removal/optimistic-lock/last-owner codes
//   - Phase 3: EVT-14/15 stale/poison event codes
//   - Phase 4: outbound-client 503 codes
var (
	// Common / cross-cutting
	ErrValidation             = errors.New("validation_error")
	ErrMissingIdentity        = errors.New("missing_identity_headers")
	ErrInsufficientRole       = errors.New("insufficient_role")
	ErrOptimisticLockConflict = errors.New("optimistic_lock_conflict")
	ErrDependencyUnavailable  = errors.New("dependency_unavailable")
	ErrNoMutableField         = errors.New("no_mutable_field")

	// Not-found (§17 404 family)
	ErrTenantNotFound     = errors.New("tenant_not_found")
	ErrMemberNotFound     = errors.New("member_not_found")
	ErrDepartmentNotFound = errors.New("department_not_found")
	ErrDelegationNotFound = errors.New("delegation_not_found")
	ErrInvitationNotFound = errors.New("invitation_not_found")
	ErrPlanNotFound       = errors.New("plan_not_found")

	// Conflict (§17 409 family)
	ErrConflict                    = errors.New("conflict")
	ErrSlugAlreadyTaken            = errors.New("slug_already_taken")
	ErrMemberAlreadyExists         = errors.New("member_already_exists")
	ErrDeptMembershipAlreadyExists = errors.New("dept_membership_already_exists")
	ErrWorkflowResolutionRequired  = errors.New("workflow_resolution_required")
	ErrSeatLimitReached            = errors.New("seat_limit_reached")
	ErrInvitationAlreadyExists     = errors.New("invitation_already_exists")
	ErrTenantOffboarded            = errors.New("tenant_offboarded")
	// Lifecycle gates (TRIAL-4, §16 A53). Surfaced by RequireActiveTenant
	// middleware as defense-in-depth for the upstream Keycloak session
	// disable — if RP's session-revoke fails or a long-lived JWT slips
	// through, the service still refuses API access on these states.
	ErrTenantTrialExpired         = errors.New("tenant_trial_expired")
	ErrTenantSuspended            = errors.New("tenant_suspended")
	ErrTenantReadOnly             = errors.New("tenant_read_only")
	ErrDepartmentAlreadyActivated = errors.New("department_already_activated")
	ErrRoleAlreadyGranted         = errors.New("role_already_granted")
	ErrACLAlreadyExists           = errors.New("acl_already_exists")
	ErrMemberNotActive            = errors.New("member_not_active")

	// Domain-rule (§17 422 family)
	ErrSelfDelegation                  = errors.New("self_delegation")
	ErrDelegationStartInPast           = errors.New("delegation_start_in_past")
	ErrDelegationStartTooFarFuture     = errors.New("delegation_start_too_far_future") // §16 A71, DEL-14
	ErrDelegationWindowTooLong         = errors.New("delegation_window_too_long")      // §16 A71, DEL-14: span > delegation_max_duration_days
	ErrExtendDaysOutOfRange            = errors.New("extend_days_out_of_range")        // §16 A71: extend_days outside [1, 180]
	ErrDelegateUnavailable             = errors.New("delegate_unavailable")
	ErrDelegationNotOpenEnded          = errors.New("delegation_not_open_ended") // P-32: extend only applies to open-ended delegations
	ErrInvalidDelegate                 = errors.New("invalid_delegate")
	ErrDelegationWindowInverted        = errors.New("delegation_window_inverted")
	ErrScopeIDRequired                 = errors.New("scope_id_required")
	ErrCannotDeleteSystemDepartment    = errors.New("cannot_delete_system_department")
	ErrDepartmentNotActiveForTenant    = errors.New("department_not_active_for_tenant")
	ErrDepartmentRetired               = errors.New("department_retired")
	ErrDepartmentDeactivated           = errors.New("department_deactivated")
	ErrInvalidAction                   = errors.New("invalid_action")
	ErrInvalidReplacement              = errors.New("invalid_replacement")
	ErrInvalidOwnerCandidate           = errors.New("invalid_owner_candidate")
	ErrInvalidExpiresAt                = errors.New("invalid_expires_at")
	ErrInvalidRole                     = errors.New("invalid_role")
	ErrInvalidPlan                     = errors.New("invalid_plan")
	ErrInvalidRealmType                = errors.New("invalid_realm_type")
	ErrAssigneeIneligible              = errors.New("assignee_ineligible")
	ErrFieldImmutable                  = errors.New("field_immutable")
	ErrSystemNameImmutable             = errors.New("system_name_immutable")
	ErrSystemDepartmentCannotBeRetired = errors.New("system_department_cannot_be_retired")
	ErrLastOwnerRemoval                = errors.New("last_owner_removal")

	// Forbidden (§17 403 family — distinct from insufficient_role)
	ErrCannotRemoveOwner = errors.New("cannot_remove_owner")

	// Rate-limit (§17 429 family, §16 A41)
	ErrReinviteTooSoon   = errors.New("reinvite_too_soon")
	ErrInviteRateLimited = errors.New("invite_rate_limited")

	// Dependency (§17 503 family)
	ErrDBUnavailable               = errors.New("db_unavailable")
	ErrCacheUnavailable            = errors.New("cache_unavailable")
	ErrUserProfileUnavailable      = errors.New("user_profile_unavailable")
	ErrWorkflowServiceUnavailable  = errors.New("workflow_service_unavailable")
	ErrRealmProvisionerUnavailable = errors.New("realm_provisioner_unavailable")
	// ErrCatalogServiceUnavailable is returned by service.CatalogService
	// when both the cat:departments/plans-equivalent primary cache AND the
	// 24h stale-if-error fallback are empty and the live call to
	// catalog-admin-config also failed — i.e. there is truly no data to
	// serve, not merely stale data. Never a silent wrong answer (LLD §11,
	// CAT-FAIL-2).
	ErrCatalogServiceUnavailable = errors.New("catalog_service_unavailable")
)

// DomainError wraps a sentinel with a human-readable message and optional
// cause. Handlers translate DomainError.Code into an HTTP status via the
// mapping in the http adapter (§17).
type DomainError struct {
	Code    string
	Message string
	Cause   error
	// Details is a free-form map serialised into the response body. Used for
	// error-specific context — active_workflows on ErrWorkflowResolutionRequired,
	// licensed_seats on ErrSeatLimitReached, retry_after_seconds on 429, etc.
	Details map[string]any
}

func (e *DomainError) Error() string { return e.Code + ": " + e.Message }
func (e *DomainError) Unwrap() error { return e.Cause }

// NewError creates a DomainError whose Cause is the given sentinel and whose
// wire Code is the sentinel's string value.
func NewError(sentinel error, message string) *DomainError {
	return &DomainError{Code: sentinel.Error(), Message: message, Cause: sentinel}
}

// WithDetails attaches structured detail fields to the error body. Chainable.
func (e *DomainError) WithDetails(d map[string]any) *DomainError {
	e.Details = d
	return e
}
