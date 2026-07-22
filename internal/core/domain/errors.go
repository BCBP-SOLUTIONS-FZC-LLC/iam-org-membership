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

	// Not-found (§17 404 family)
	ErrTenantNotFound     = errors.New("tenant_not_found")
	ErrMemberNotFound     = errors.New("member_not_found")
	ErrDepartmentNotFound = errors.New("department_not_found")
	ErrDelegationNotFound = errors.New("delegation_not_found")
	ErrInvitationNotFound = errors.New("invitation_not_found")

	// Conflict (§17 409 family)
	ErrConflict                    = errors.New("conflict")
	ErrSlugAlreadyTaken            = errors.New("slug_already_taken")
	ErrMemberAlreadyExists         = errors.New("member_already_exists")
	ErrDeptMembershipAlreadyExists = errors.New("dept_membership_already_exists")
	ErrWorkflowResolutionRequired  = errors.New("workflow_resolution_required")
	ErrSeatLimitReached            = errors.New("seat_limit_reached")
	ErrInvitationAlreadyExists     = errors.New("invitation_already_exists")
	ErrTenantOffboarded            = errors.New("tenant_offboarded")

	// Domain-rule (§17 422 family)
	ErrSelfDelegation                  = errors.New("self_delegation")
	ErrInvalidDelegate                 = errors.New("invalid_delegate")
	ErrDelegationWindowInverted        = errors.New("delegation_window_inverted")
	ErrScopeIDRequired                 = errors.New("scope_id_required")
	ErrCannotDeleteSystemDepartment    = errors.New("cannot_delete_system_department")
	ErrDepartmentNotActiveForTenant    = errors.New("department_not_active_for_tenant")
	ErrInvalidReplacement              = errors.New("invalid_replacement")
	ErrInvalidOwnerCandidate           = errors.New("invalid_owner_candidate")
	ErrInvalidExpiresAt                = errors.New("invalid_expires_at")
	ErrAssigneeIneligible              = errors.New("assignee_ineligible")
	ErrFieldImmutable                  = errors.New("field_immutable")
	ErrSystemNameImmutable             = errors.New("system_name_immutable")
	ErrSystemDepartmentCannotBeRetired = errors.New("system_department_cannot_be_retired")
	ErrLastOwnerRemoval                = errors.New("last_owner_removal")

	// Rate-limit (§17 429 family, §16 A41)
	ErrReinviteTooSoon   = errors.New("reinvite_too_soon")
	ErrInviteRateLimited = errors.New("invite_rate_limited")

	// Dependency (§17 503 family)
	ErrDBUnavailable               = errors.New("db_unavailable")
	ErrCacheUnavailable            = errors.New("cache_unavailable")
	ErrUserProfileUnavailable      = errors.New("user_profile_unavailable")
	ErrWorkflowServiceUnavailable  = errors.New("workflow_service_unavailable")
	ErrRealmProvisionerUnavailable = errors.New("realm_provisioner_unavailable")
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
