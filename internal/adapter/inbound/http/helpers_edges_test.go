// Phase 19 — helper coverage for errors.go / middleware.go / handler
// helper edges (parseTenantIDParam / parseUUIDParam / HandleError).
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

// ── HandleError: DomainError translation + generic 500 ───────────────

func TestP19Helpers_HandleError_DomainErrorReturnsMappedStatus(t *testing.T) {
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	HandleError(c, domain.NewError(domain.ErrOptimisticLockConflict, "conflict"))
	assert.Equal(t, http.StatusConflict, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "OPTIMISTIC_LOCK_CONFLICT", body["errorCode"])
}

func TestP19Helpers_HandleError_GenericErrorReturns500(t *testing.T) {
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	HandleError(c, assertPlainErr("db down"))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "INTERNAL_ERROR", body["errorCode"])
	assert.Empty(t, body["detail"], "RFC 9457 suppresses detail for internal errors")
}

// assertPlainErr constructs a non-DomainError so HandleError's fallback
// 500 branch runs.
func assertPlainErr(msg string) error { return &plainErr{msg} }

type plainErr struct{ m string }

func (e *plainErr) Error() string { return e.m }
