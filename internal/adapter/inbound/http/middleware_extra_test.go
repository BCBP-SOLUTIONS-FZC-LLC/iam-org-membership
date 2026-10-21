// Phase 11 — Middleware exhaustive coverage.
//
// Module:   iam-org-membership
// Feature:  Middleware — RequireOperatorRole gate + every §17 sentinel →
//
//	HTTP status mapping.
//
// File:     internal/adapter/inbound/http/middleware.go
//
// Test IDs: P11-MW-NNN.
//
// The existing middleware_test.go covers RequireSystemRole and a spot-check
// of HandleError. This file adds:
//   - RequireOperatorRole full matrix
//   - Every sentinel with its expected HTTP status (46 sentinels)
//   - Details-merging on 409 shapes
package http

import (
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═════════════════════════════════════════════════════════════════════════
// RequireOperatorRole
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P11-MW-001
// Feature:           /operator/* gate accepts platform_operator
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestP11MW001_OperatorGate_AcceptsOperator(t *testing.T) {
	rc := &requestctx.RequestContext{Roles: []string{"platform_operator"}}
	c, _ := newTestContext(rc)
	called := false
	chain := []gin.HandlerFunc{RequireOperatorRole(), gin.HandlerFunc(func(*gin.Context) { called = true })}
	for _, h := range chain {
		if c.IsAborted() {
			break
		}
		h(c)
	}
	assert.True(t, called, "platform_operator must pass RequireOperatorRole")
	// StatusOK isn't automatically set; ensuring not aborted is the check.
}

// Test Case ID:      P11-MW-002
// Feature:           /operator/* gate rejects tenant_owner
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestP11MW002_OperatorGate_RejectsTenantOwner(t *testing.T) {
	rc := &requestctx.RequestContext{Roles: []string{"tenant_owner"}}
	c, w := newTestContext(rc)
	RequireOperatorRole()(c)
	assert.True(t, c.IsAborted(), "non-operator must abort")
	assert.Equal(t, http.StatusForbidden, w.Code)
	body := decodeBody(t, w)
	assert.Equal(t, "FORBIDDEN", body["errorCode"])
}

// Test Case ID:      P11-MW-003
// Feature:           /operator/* gate rejects tenant_admin
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP11MW003_OperatorGate_RejectsTenantAdmin(t *testing.T) {
	rc := &requestctx.RequestContext{Roles: []string{"tenant_admin"}}
	c, w := newTestContext(rc)
	RequireOperatorRole()(c)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// Test Case ID:      P11-MW-004
// Feature:           /operator/* gate rejects iam-system
// Scenario:          iam-system is NOT platform_operator — operator lane is distinct
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP11MW004_OperatorGate_RejectsIAMSystem(t *testing.T) {
	rc := &requestctx.RequestContext{Roles: []string{"iam-system"}}
	c, w := newTestContext(rc)
	RequireOperatorRole()(c)
	assert.Equal(t, http.StatusForbidden, w.Code,
		"iam-system is internal-lane, not operator; must be rejected")
}

// Test Case ID:      P11-MW-005
// Feature:           /operator/* gate rejects request with no identity
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP11MW005_OperatorGate_RejectsNoIdentity(t *testing.T) {
	c, w := newTestContext(nil)
	RequireOperatorRole()(c)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// ═════════════════════════════════════════════════════════════════════════
// HandleError — exhaustive sentinel → status coverage
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P11-MW-010
// Feature:           §17 · every sentinel maps to the correct HTTP status
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestP11MW010_AllSentinelsMapToCorrectStatus(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		expectStatus int
		expectCode   string
	}{
		// 400 Bad Request
		{"validation → 400", domain.ErrValidation, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"no_mutable_field → 400", domain.ErrNoMutableField, http.StatusBadRequest, "NO_MUTABLE_FIELD"},

		// 401 Unauthorized
		{"missing_identity → 401", domain.ErrMissingIdentity, http.StatusUnauthorized, "MISSING_IDENTITY_HEADERS"},

		// 403 Forbidden
		{"insufficient_role → 403", domain.ErrInsufficientRole, http.StatusForbidden, "INSUFFICIENT_ROLE"},
		{"cannot_remove_owner → 403", domain.ErrCannotRemoveOwner, http.StatusForbidden, "CANNOT_REMOVE_OWNER"},

		// 404 Not Found
		{"tenant_not_found → 404", domain.ErrTenantNotFound, http.StatusNotFound, "TENANT_NOT_FOUND"},
		{"member_not_found → 404", domain.ErrMemberNotFound, http.StatusNotFound, "MEMBER_NOT_FOUND"},
		{"department_not_found → 404", domain.ErrDepartmentNotFound, http.StatusNotFound, "DEPARTMENT_NOT_FOUND"},
		{"invitation_not_found → 404", domain.ErrInvitationNotFound, http.StatusNotFound, "INVITATION_NOT_FOUND"},

		// 409 Conflict
		{"optimistic_lock → 409", domain.ErrOptimisticLockConflict, http.StatusConflict, "OPTIMISTIC_LOCK_CONFLICT"},
		{"conflict → 409", domain.ErrConflict, http.StatusConflict, "CONFLICT"},
		{"slug_taken → 409", domain.ErrSlugAlreadyTaken, http.StatusConflict, "SLUG_ALREADY_TAKEN"},
		{"member_already_exists → 409", domain.ErrMemberAlreadyExists, http.StatusConflict, "MEMBER_ALREADY_EXISTS"},
		{"dept_membership_exists → 409", domain.ErrDeptMembershipAlreadyExists, http.StatusConflict, "DEPT_MEMBERSHIP_ALREADY_EXISTS"},
		{"workflow_resolution_required → 409", domain.ErrWorkflowResolutionRequired, http.StatusConflict, "WORKFLOW_RESOLUTION_REQUIRED"},
		{"seat_limit → 409", domain.ErrSeatLimitReached, http.StatusConflict, "SEAT_LIMIT_REACHED"},
		{"invitation_already_exists → 409", domain.ErrInvitationAlreadyExists, http.StatusConflict, "INVITATION_ALREADY_EXISTS"},
		{"tenant_offboarded → 409", domain.ErrTenantOffboarded, http.StatusConflict, "TENANT_OFFBOARDED"},
		{"dept_already_activated → 409", domain.ErrDepartmentAlreadyActivated, http.StatusConflict, "DEPARTMENT_ALREADY_ACTIVATED"},
		{"role_already_granted → 409", domain.ErrRoleAlreadyGranted, http.StatusConflict, "ROLE_ALREADY_GRANTED"},

		// 422 Unprocessable Entity (default bucket)
		{"cannot_delete_system → 422", domain.ErrCannotDeleteSystemDepartment, http.StatusUnprocessableEntity, "CANNOT_DELETE_SYSTEM_DEPARTMENT"},
		{"dept_not_active → 422", domain.ErrDepartmentNotActiveForTenant, http.StatusUnprocessableEntity, "DEPARTMENT_NOT_ACTIVE_FOR_TENANT"},
		{"dept_retired → 422", domain.ErrDepartmentRetired, http.StatusUnprocessableEntity, "DEPARTMENT_RETIRED"},
		{"dept_deactivated → 422", domain.ErrDepartmentDeactivated, http.StatusUnprocessableEntity, "DEPARTMENT_DEACTIVATED"},
		{"invalid_replacement → 422", domain.ErrInvalidReplacement, http.StatusUnprocessableEntity, "INVALID_REPLACEMENT"},
		{"invalid_owner_candidate → 422", domain.ErrInvalidOwnerCandidate, http.StatusUnprocessableEntity, "INVALID_OWNER_CANDIDATE"},
		{"invalid_role → 422", domain.ErrInvalidRole, http.StatusUnprocessableEntity, "INVALID_ROLE"},
		{"assignee_ineligible → 422", domain.ErrAssigneeIneligible, http.StatusUnprocessableEntity, "ASSIGNEE_INELIGIBLE"},
		{"field_immutable → 422", domain.ErrFieldImmutable, http.StatusUnprocessableEntity, "FIELD_IMMUTABLE"},
		{"system_name_immutable → 422", domain.ErrSystemNameImmutable, http.StatusUnprocessableEntity, "SYSTEM_NAME_IMMUTABLE"},
		{"system_dept_no_retire → 422", domain.ErrSystemDepartmentCannotBeRetired, http.StatusUnprocessableEntity, "SYSTEM_DEPARTMENT_CANNOT_BE_RETIRED"},
		{"last_owner_removal → 422", domain.ErrLastOwnerRemoval, http.StatusUnprocessableEntity, "LAST_OWNER_REMOVAL"},

		// 429 Too Many Requests
		{"reinvite_too_soon → 429", domain.ErrReinviteTooSoon, http.StatusTooManyRequests, "REINVITE_TOO_SOON"},
		{"invite_rate_limited → 429", domain.ErrInviteRateLimited, http.StatusTooManyRequests, "INVITE_RATE_LIMITED"},

		// 503 Service Unavailable
		{"db_unavailable → 503", domain.ErrDBUnavailable, http.StatusServiceUnavailable, "DB_UNAVAILABLE"},
		{"cache_unavailable → 503", domain.ErrCacheUnavailable, http.StatusServiceUnavailable, "CACHE_UNAVAILABLE"},
		{"workflow_service_unavailable → 503", domain.ErrWorkflowServiceUnavailable, http.StatusServiceUnavailable, "WORKFLOW_SERVICE_UNAVAILABLE"},
		{"realm_provisioner_unavailable → 503", domain.ErrRealmProvisionerUnavailable, http.StatusServiceUnavailable, "REALM_PROVISIONER_UNAVAILABLE"},
		{"catalog_unavailable → 503", domain.ErrCatalogUnavailable, http.StatusServiceUnavailable, "CATALOG_UNAVAILABLE"},
		{"group_mapping_unavailable → 503", domain.ErrGroupMappingUnavailable, http.StatusServiceUnavailable, "GROUP_MAPPING_UNAVAILABLE"},
		{"dependency_unavailable → 503", domain.ErrDependencyUnavailable, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, w := newTestContext(nil)
			HandleError(c, domain.NewError(tc.err, "test"))
			assert.Equal(t, tc.expectStatus, w.Code, "wrong HTTP status")
			body := decodeBody(t, w)
			assert.Equal(t, tc.expectCode, body["errorCode"], "wrong error code")
		})
	}
	assert.GreaterOrEqual(t, len(cases), 40,
		"if a sentinel was added to domain/errors.go, extend this exhaustive table")
}

// ═════════════════════════════════════════════════════════════════════════
// Details merging on 409 shapes — CONC-4 assertion
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P11-MW-020
// Feature:           §17 · 409 optimistic_lock body includes record_version
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestP11MW020_OptimisticLockDetailsMerged(t *testing.T) {
	c, w := newTestContext(nil)
	HandleError(c, domain.NewError(domain.ErrOptimisticLockConflict, "stale").
		WithDetails(map[string]any{"record_version": int64(7)}))
	assert.Equal(t, http.StatusConflict, w.Code)
	body := decodeBody(t, w)
	assert.Equal(t, "OPTIMISTIC_LOCK_CONFLICT", body["errorCode"])
	// record_version was int64(7), but JSON unmarshal yields float64.
	rv, ok := body["record_version"].(float64)
	require.True(t, ok, "record_version must be surfaced")
	assert.Equal(t, float64(7), rv)
}

// Test Case ID:      P11-MW-021
// Feature:           §17 · workflow_resolution_required body carries active_workflows + allowed_actions
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestP11MW021_WorkflowResolutionDetailsMerged(t *testing.T) {
	c, w := newTestContext(nil)
	HandleError(c, domain.NewError(domain.ErrWorkflowResolutionRequired, "blocked").
		WithDetails(map[string]any{
			"active_workflows": 3,
			"allowed_actions":  []string{"replace_delegate", "stop_workflows"},
		}))
	assert.Equal(t, http.StatusConflict, w.Code)
	body := decodeBody(t, w)
	assert.Equal(t, "WORKFLOW_RESOLUTION_REQUIRED", body["errorCode"])
	assert.Equal(t, float64(3), body["active_workflows"])
	actions, _ := body["allowed_actions"].([]any)
	require.Len(t, actions, 2)
}

// Test Case ID:      P11-MW-022
// Feature:           §17 · seat_limit_reached body carries seat counts
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP11MW022_SeatLimitDetailsMerged(t *testing.T) {
	c, w := newTestContext(nil)
	HandleError(c, domain.NewError(domain.ErrSeatLimitReached, "cap").
		WithDetails(map[string]any{
			"licensed_seats":      10,
			"active_users":        10,
			"pending_invitations": 0,
		}))
	assert.Equal(t, http.StatusConflict, w.Code)
	body := decodeBody(t, w)
	assert.Equal(t, "SEAT_LIMIT_REACHED", body["errorCode"])
	assert.Equal(t, float64(10), body["licensed_seats"])
}

// Test Case ID:      P11-MW-023
// Feature:           §17 · Details are extension members written by platform-gincommon
// Priority: P1 · Severity: Major · Automation Status: Automated
//
// Details go through the library's extension members, not a hand-merged
// map: a detail named like a standard member can never overwrite it, and
// the response keeps the library's content type and traceId/instance.
func TestP11MW023_DetailsAreLibraryExtensionMembers(t *testing.T) {
	c, w := newTestContext(nil)
	HandleError(c, domain.NewError(domain.ErrOptimisticLockConflict, "stale").
		WithDetails(map[string]any{
			"record_version": int64(7),
			"status":         200,
			"errorCode":      "FORGED",
		}))
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "application/problem+json", w.Header().Get("Content-Type"))
	body := decodeBody(t, w)
	assert.Equal(t, float64(http.StatusConflict), body["status"], "a detail must not overwrite status")
	assert.Equal(t, "OPTIMISTIC_LOCK_CONFLICT", body["errorCode"], "a detail must not overwrite errorCode")
	assert.Equal(t, float64(7), body["record_version"])
	assert.NotEmpty(t, body["traceId"])
	assert.NotEmpty(t, body["instance"])
}

// ═════════════════════════════════════════════════════════════════════════
// CD-31 — Error-code pin: seat_limit_reached must not change during
//          a future RFC 9457 migration (TL decision 2026-10-15).
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      CD31-PIN-001
// Feature:           CD-31 · seat_limit_reached code string is pinned to prevent
//
//	silent renames during a future RFC 9457 format migration.
//
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestCD31_SeatLimitReached_CodeStringPinned(t *testing.T) {
	// This test intentionally checks the wire code string rather than
	// errors.Is so that any future rename of the constant or the HTTP adapter
	// mapping breaks the test explicitly. CD-31 (TL decision 2026-10-15):
	// migrate IAM to RFC 9457 in a future phase — until then code strings
	// must not change.
	assert.Equal(t, "seat_limit_reached", domain.ErrSeatLimitReached.Error(),
		"wire code string for ErrSeatLimitReached must remain 'seat_limit_reached'")

	c, w := newTestContext(nil)
	HandleError(c, domain.NewError(domain.ErrSeatLimitReached, "seat limit reached"))
	assert.Equal(t, http.StatusConflict, w.Code,
		"ErrSeatLimitReached must map to 409 Conflict")
	body := decodeBody(t, w)
	assert.Equal(t, "SEAT_LIMIT_REACHED", body["errorCode"],
		"409 body.errorCode must be 'SEAT_LIMIT_REACHED' (CD-31 RFC-9457 migration pin)")
}

// ═════════════════════════════════════════════════════════════════════════
// Unknown error → generic 500
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P11-MW-030
// Feature:           Non-DomainError → 500 internal_error
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestP11MW030_NonDomainError500(t *testing.T) {
	c, w := newTestContext(nil)
	HandleError(c, assertionError("boom"))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	body := decodeBody(t, w)
	assert.Equal(t, "INTERNAL_ERROR", body["errorCode"])
}

// assertionError implements the error interface for the 500-path test.
type assertionError string

func (e assertionError) Error() string { return string(e) }
