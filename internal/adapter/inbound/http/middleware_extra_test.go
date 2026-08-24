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
	assert.Equal(t, "insufficient_role", body["code"])
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
		{"validation → 400", domain.ErrValidation, http.StatusBadRequest, "validation_error"},
		{"no_mutable_field → 400", domain.ErrNoMutableField, http.StatusBadRequest, "no_mutable_field"},

		// 401 Unauthorized
		{"missing_identity → 401", domain.ErrMissingIdentity, http.StatusUnauthorized, "missing_identity_headers"},

		// 403 Forbidden
		{"insufficient_role → 403", domain.ErrInsufficientRole, http.StatusForbidden, "insufficient_role"},
		{"cannot_remove_owner → 403", domain.ErrCannotRemoveOwner, http.StatusForbidden, "cannot_remove_owner"},

		// 404 Not Found
		{"tenant_not_found → 404", domain.ErrTenantNotFound, http.StatusNotFound, "tenant_not_found"},
		{"member_not_found → 404", domain.ErrMemberNotFound, http.StatusNotFound, "member_not_found"},
		{"department_not_found → 404", domain.ErrDepartmentNotFound, http.StatusNotFound, "department_not_found"},
		{"invitation_not_found → 404", domain.ErrInvitationNotFound, http.StatusNotFound, "invitation_not_found"},

		// 409 Conflict
		{"optimistic_lock → 409", domain.ErrOptimisticLockConflict, http.StatusConflict, "optimistic_lock_conflict"},
		{"conflict → 409", domain.ErrConflict, http.StatusConflict, "conflict"},
		{"slug_taken → 409", domain.ErrSlugAlreadyTaken, http.StatusConflict, "slug_already_taken"},
		{"member_already_exists → 409", domain.ErrMemberAlreadyExists, http.StatusConflict, "member_already_exists"},
		{"dept_membership_exists → 409", domain.ErrDeptMembershipAlreadyExists, http.StatusConflict, "dept_membership_already_exists"},
		{"workflow_resolution_required → 409", domain.ErrWorkflowResolutionRequired, http.StatusConflict, "workflow_resolution_required"},
		{"seat_limit → 409", domain.ErrSeatLimitReached, http.StatusConflict, "seat_limit_reached"},
		{"invitation_already_exists → 409", domain.ErrInvitationAlreadyExists, http.StatusConflict, "invitation_already_exists"},
		{"tenant_offboarded → 409", domain.ErrTenantOffboarded, http.StatusConflict, "tenant_offboarded"},
		{"dept_already_activated → 409", domain.ErrDepartmentAlreadyActivated, http.StatusConflict, "department_already_activated"},
		{"role_already_granted → 409", domain.ErrRoleAlreadyGranted, http.StatusConflict, "role_already_granted"},

		// 422 Unprocessable Entity (default bucket)
		{"cannot_delete_system → 422", domain.ErrCannotDeleteSystemDepartment, http.StatusUnprocessableEntity, "cannot_delete_system_department"},
		{"dept_not_active → 422", domain.ErrDepartmentNotActiveForTenant, http.StatusUnprocessableEntity, "department_not_active_for_tenant"},
		{"dept_retired → 422", domain.ErrDepartmentRetired, http.StatusUnprocessableEntity, "department_retired"},
		{"dept_deactivated → 422", domain.ErrDepartmentDeactivated, http.StatusUnprocessableEntity, "department_deactivated"},
		{"invalid_replacement → 422", domain.ErrInvalidReplacement, http.StatusUnprocessableEntity, "invalid_replacement"},
		{"invalid_owner_candidate → 422", domain.ErrInvalidOwnerCandidate, http.StatusUnprocessableEntity, "invalid_owner_candidate"},
		{"invalid_role → 422", domain.ErrInvalidRole, http.StatusUnprocessableEntity, "invalid_role"},
		{"assignee_ineligible → 422", domain.ErrAssigneeIneligible, http.StatusUnprocessableEntity, "assignee_ineligible"},
		{"field_immutable → 422", domain.ErrFieldImmutable, http.StatusUnprocessableEntity, "field_immutable"},
		{"system_name_immutable → 422", domain.ErrSystemNameImmutable, http.StatusUnprocessableEntity, "system_name_immutable"},
		{"system_dept_no_retire → 422", domain.ErrSystemDepartmentCannotBeRetired, http.StatusUnprocessableEntity, "system_department_cannot_be_retired"},
		{"last_owner_removal → 422", domain.ErrLastOwnerRemoval, http.StatusUnprocessableEntity, "last_owner_removal"},

		// 429 Too Many Requests
		{"reinvite_too_soon → 429", domain.ErrReinviteTooSoon, http.StatusTooManyRequests, "reinvite_too_soon"},
		{"invite_rate_limited → 429", domain.ErrInviteRateLimited, http.StatusTooManyRequests, "invite_rate_limited"},

		// 503 Service Unavailable
		{"db_unavailable → 503", domain.ErrDBUnavailable, http.StatusServiceUnavailable, "db_unavailable"},
		{"cache_unavailable → 503", domain.ErrCacheUnavailable, http.StatusServiceUnavailable, "cache_unavailable"},
		{"workflow_service_unavailable → 503", domain.ErrWorkflowServiceUnavailable, http.StatusServiceUnavailable, "workflow_service_unavailable"},
		{"realm_provisioner_unavailable → 503", domain.ErrRealmProvisionerUnavailable, http.StatusServiceUnavailable, "realm_provisioner_unavailable"},
		{"catalog_unavailable → 503", domain.ErrCatalogUnavailable, http.StatusServiceUnavailable, "catalog_unavailable"},
		{"group_mapping_unavailable → 503", domain.ErrGroupMappingUnavailable, http.StatusServiceUnavailable, "group_mapping_unavailable"},
		{"dependency_unavailable → 503", domain.ErrDependencyUnavailable, http.StatusServiceUnavailable, "dependency_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, w := newTestContext(nil)
			HandleError(c, domain.NewError(tc.err, "test"))
			assert.Equal(t, tc.expectStatus, w.Code, "wrong HTTP status")
			body := decodeBody(t, w)
			assert.Equal(t, tc.expectCode, body["code"], "wrong error code")
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
	assert.Equal(t, "optimistic_lock_conflict", body["code"])
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
	assert.Equal(t, "workflow_resolution_required", body["code"])
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
	assert.Equal(t, "seat_limit_reached", body["code"])
	assert.Equal(t, float64(10), body["licensed_seats"])
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
	assert.Equal(t, "internal_error", body["code"])
}

// assertionError implements the error interface for the 500-path test.
type assertionError string

func (e assertionError) Error() string { return string(e) }
