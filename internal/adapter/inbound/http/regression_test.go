// Regression tests for HTTP-adapter-level audit fixes (Tier 1-3 + Round 2).
// Pure unit — no DB. Sibling of middleware_test.go.
//
// Each test names the fix ID it locks in.
package http

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────
// G2/G3: error → HTTP status mappings for new sentinels.
// ─────────────────────────────────────────────────────────────────────────

func TestG3_ErrNoMutableField_MapsTo400(t *testing.T) {
	c, w := newTestContext(nil)
	HandleError(c, domain.NewError(domain.ErrNoMutableField, "at least one of name or is_active"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	body := decodeBody(t, w)
	assert.Equal(t, "no_mutable_field", body["code"])
}

func TestG2_ErrCannotRemoveOwner_MapsTo403(t *testing.T) {
	c, w := newTestContext(nil)
	HandleError(c, domain.NewError(domain.ErrCannotRemoveOwner, "cannot remove sole owner without operator"))
	assert.Equal(t, http.StatusForbidden, w.Code)
	body := decodeBody(t, w)
	assert.Equal(t, "cannot_remove_owner", body["code"])
}

func TestG2_ErrDepartmentAlreadyActivated_MapsTo409(t *testing.T) {
	c, w := newTestContext(nil)
	HandleError(c, domain.NewError(domain.ErrDepartmentAlreadyActivated, "dept already active"))
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestG2_ErrRoleAlreadyGranted_MapsTo409(t *testing.T) {
	c, w := newTestContext(nil)
	HandleError(c, domain.NewError(domain.ErrRoleAlreadyGranted, "role already granted"))
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestG2_ErrDepartmentRetired_MapsTo422(t *testing.T) {
	c, w := newTestContext(nil)
	HandleError(c, domain.NewError(domain.ErrDepartmentRetired, "dept is retired"))
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

func TestG2_ErrInvalidRole_MapsTo422(t *testing.T) {
	c, w := newTestContext(nil)
	HandleError(c, domain.NewError(domain.ErrInvalidRole, "'member' cannot be persisted"))
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────
// N2: InvitationResponse carries both `id` (legacy) and `invitation_id`
//     (canonical per LLD §5.4 P-6).
// ─────────────────────────────────────────────────────────────────────────

func TestN2_InvitationResponse_HasBothIDFields(t *testing.T) {
	invID := uuid.New()
	inv := domain.PendingInvitation{
		ID: invID, Email: "u@example.com", FullName: "User",
		Status: domain.InvitePending, RecordVersion: 1,
	}
	resp := invitationToResponse(inv)
	assert.Equal(t, invID, resp.ID)
	assert.Equal(t, invID, resp.InvitationID)

	// JSON marshal must emit both keys so old and new clients both parse.
	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(raw, &parsed))
	assert.Contains(t, parsed, "id")
	assert.Contains(t, parsed, "invitation_id")
	assert.Equal(t, invID.String(), parsed["id"])
	assert.Equal(t, invID.String(), parsed["invitation_id"])
}

// ─────────────────────────────────────────────────────────────────────────
// N3: OperatorReassignOwnerRequest.EffectiveUserID prefers `user_id`
//     (LLD canonical) and falls back to `new_owner_user_id`.
// ─────────────────────────────────────────────────────────────────────────

func TestN3_EffectiveUserID_PrefersCanonicalUserID(t *testing.T) {
	canonical := uuid.New()
	legacy := uuid.New()
	req := OperatorReassignOwnerRequest{UserID: canonical, NewOwnerUserID: legacy}
	assert.Equal(t, canonical, req.EffectiveUserID(),
		"canonical user_id must win when both keys sent")
}

func TestN3_EffectiveUserID_FallsBackToLegacy(t *testing.T) {
	legacy := uuid.New()
	req := OperatorReassignOwnerRequest{NewOwnerUserID: legacy} // no UserID
	assert.Equal(t, legacy, req.EffectiveUserID(),
		"pre-rev-1.51 clients still supported for one release")
}

func TestN3_EffectiveUserID_BothMissingReturnsNil(t *testing.T) {
	req := OperatorReassignOwnerRequest{}
	assert.Equal(t, uuid.Nil, req.EffectiveUserID())
}

// ─────────────────────────────────────────────────────────────────────────
// B9/B10/B19: P-31 DTO carries record_version in body; handler-side
// fallback to query string covered in service_regression_test.go.
// ─────────────────────────────────────────────────────────────────────────

func TestB9_InvitationRevokeRequest_DTOShape(t *testing.T) {
	// Body binding must accept `{"record_version": N}` per LLD §5.4 P-31.
	body := `{"record_version": 42}`
	var req InvitationRevokeRequest
	require.NoError(t, json.NewDecoder(strings.NewReader(body)).Decode(&req))
	assert.Equal(t, int64(42), req.RecordVersion)
}

// ─────────────────────────────────────────────────────────────────────────
// G2: existing error catalog stays wired — regression guard so someone
// removing an old sentinel doesn't silently break the middleware map.
// ─────────────────────────────────────────────────────────────────────────

func TestG2_ExistingSentinelsStillMapped(t *testing.T) {
	// Sample the important pre-existing 5xx / 422 mappings to verify none
	// were disturbed when new sentinels were added.
	cases := []struct {
		name         string
		err          error
		expectStatus int
	}{
		{"validation → 400", domain.ErrValidation, http.StatusBadRequest},
		{"missing_identity → 401", domain.ErrMissingIdentity, http.StatusUnauthorized},
		{"insufficient_role → 403", domain.ErrInsufficientRole, http.StatusForbidden},
		{"member_not_found → 404", domain.ErrMemberNotFound, http.StatusNotFound},
		{"optimistic_lock_conflict → 409", domain.ErrOptimisticLockConflict, http.StatusConflict},
		{"seat_limit_reached → 409", domain.ErrSeatLimitReached, http.StatusConflict},
		{"last_owner_removal → 422", domain.ErrLastOwnerRemoval, http.StatusUnprocessableEntity},
		{"workflow_service_unavailable → 503", domain.ErrWorkflowServiceUnavailable, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, w := newTestContext(nil)
			HandleError(c, domain.NewError(tc.err, "test"))
			assert.Equal(t, tc.expectStatus, w.Code)
		})
	}
}
