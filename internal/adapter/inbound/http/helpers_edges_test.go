// Phase 19 — helper coverage for errors.go / middleware.go / handler
// helper edges (parseTenantIDParam / parseUUIDParam / errorResponseWithDetails
// / domainErrorStatus / newErrorResponse).
package http

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── parseTenantIDParam edge cases ────────────────────────────────────

func TestP19Helpers_ParseTenantIDParam_MissingParam_ReturnsInvalidUUID(t *testing.T) {
	c, _ := buildCtx(http.MethodGet, "/", ``, nil)
	// no `id` param set at all
	_, err := parseTenantIDParam(c)
	require.Error(t, err)
}

func TestP19Helpers_ParseUUIDParam_MissingParam_ReturnsInvalidUUID(t *testing.T) {
	c, _ := buildCtx(http.MethodGet, "/", ``, nil)
	_, err := parseUUIDParam(c, "user_id")
	require.Error(t, err)
}

func TestP19Helpers_ParseUUIDParam_ValidUUID_ReturnsID(t *testing.T) {
	want := uuid.New()
	c, _ := buildCtx(http.MethodGet, "/", ``, nil)
	setParams(c, "user_id", want.String())
	got, err := parseUUIDParam(c, "user_id")
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// ── domainErrorStatus enum coverage ──────────────────────────────────

func TestP19Helpers_DomainErrorStatus_EnumCoverage(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{domain.ErrValidation, http.StatusBadRequest},
		{domain.ErrNoMutableField, http.StatusBadRequest},
		{domain.ErrMissingIdentity, http.StatusUnauthorized},
		{domain.ErrInsufficientRole, http.StatusForbidden},
		{domain.ErrTenantNotFound, http.StatusNotFound},
		{domain.ErrMemberNotFound, http.StatusNotFound},
		{domain.ErrDepartmentNotFound, http.StatusNotFound},
		{domain.ErrInvitationNotFound, http.StatusNotFound},
		{domain.ErrOptimisticLockConflict, http.StatusConflict},
		{domain.ErrConflict, http.StatusConflict},
		{domain.ErrSlugAlreadyTaken, http.StatusConflict},
		{domain.ErrMemberAlreadyExists, http.StatusConflict},
		{domain.ErrDeptMembershipAlreadyExists, http.StatusConflict},
		{domain.ErrWorkflowResolutionRequired, http.StatusConflict},
		{domain.ErrSeatLimitReached, http.StatusConflict},
		{domain.ErrInvitationAlreadyExists, http.StatusConflict},
		{domain.ErrRoleAlreadyGranted, http.StatusConflict},
		{domain.ErrDepartmentAlreadyActivated, http.StatusConflict},
	}
	for _, tc := range cases {
		de := domain.NewError(tc.err, "test")
		assert.Equal(t, tc.want, domainErrorStatus(de), "err=%v", tc.err)
	}
}

// ── errorResponseWithDetails: with & without details ────────────────

func TestP19Helpers_ErrorResponseWithDetails_NoDetails(t *testing.T) {
	er := ErrorResponse{Error: "conflict", Code: "conflict", Message: "boom", Status: 409, TraceID: "t1", RequestID: "r1"}
	out := errorResponseWithDetails(er, nil)
	assert.Equal(t, "conflict", out["error"])
	assert.Equal(t, "conflict", out["code"])
	assert.Equal(t, "boom", out["message"])
	assert.Equal(t, 409, out["status"])
	assert.Equal(t, "t1", out["trace_id"])
	assert.Equal(t, "r1", out["request_id"])
}

func TestP19Helpers_ErrorResponseWithDetails_WithDetailsMerges(t *testing.T) {
	er := ErrorResponse{Error: "conflict", Code: "conflict", Status: 409}
	details := map[string]any{
		"record_version":   int64(5),
		"active_workflows": 3,
		"workflow_ids":     []string{"a", "b"},
	}
	out := errorResponseWithDetails(er, details)
	assert.EqualValues(t, 5, out["record_version"])
	assert.Equal(t, 3, out["active_workflows"])
	assert.NotNil(t, out["workflow_ids"])
}

// ── newErrorResponse: nil context returns bare envelope ──────────────

func TestP19Helpers_NewErrorResponse_NilContextReturnsBareEnvelope(t *testing.T) {
	er := newErrorResponse(nil, "code", "msg", nil)
	assert.Equal(t, "code", er.Error)
	assert.Equal(t, "code", er.Code)
	assert.Equal(t, "msg", er.Message)
	assert.Empty(t, er.TraceID)
	assert.Empty(t, er.RequestID)
}

// ── HandleError: DomainError translation + generic 500 ───────────────

func TestP19Helpers_HandleError_DomainErrorReturnsMappedStatus(t *testing.T) {
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	HandleError(c, domain.NewError(domain.ErrOptimisticLockConflict, "conflict"))
	assert.Equal(t, http.StatusConflict, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "optimistic_lock_conflict", body["code"])
}

func TestP19Helpers_HandleError_GenericErrorReturns500(t *testing.T) {
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	HandleError(c, assertPlainErr("db down"))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "internal_error", body["code"])
}

// assertPlainErr constructs a non-DomainError so HandleError's fallback
// 500 branch runs.
func assertPlainErr(msg string) error { return &plainErr{msg} }

type plainErr struct{ m string }

func (e *plainErr) Error() string { return e.m }
