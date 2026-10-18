// Unit tests for the HTTP adapter: role-gating middleware, per-handler
// role checks, and the §17 error → HTTP status mapping (HandleError).
//
// Pure unit tests — no DB, no testcontainers. Runnable with `make test-unit`
// and part of `go test ./...`.
package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() { gin.SetMode(gin.TestMode) }

// newTestContext returns a fresh gin.Context wrapping a httptest.ResponseRecorder,
// with the supplied RequestContext already stitched onto c.Request's context.
// Handlers under test see exactly the state the real middleware would have set.
func newTestContext(rc *requestctx.RequestContext) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", strings.NewReader("{}"))
	if rc != nil {
		req = req.WithContext(requestctx.WithContext(req.Context(), rc))
	}
	c.Request = req
	return c, w
}

// decodeBody parses the JSON error body into a generic map for assertion.
func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	return body
}

// ─────────────────────────────────────────────────────────────────────────
// RequireSystemRole — /api/v1/internal/* gate
// ─────────────────────────────────────────────────────────────────────────

func TestRequireSystemRole_AcceptsSystemPrincipal(t *testing.T) {
	rc := &requestctx.RequestContext{Roles: []string{"iam-system"}}
	c, w := newTestContext(rc)

	called := false
	handler := gin.HandlerFunc(func(c *gin.Context) { called = true; c.Status(http.StatusOK) })
	chain := []gin.HandlerFunc{RequireSystemRole(), handler}
	for _, h := range chain {
		if c.IsAborted() {
			break
		}
		h(c)
	}
	assert.True(t, called, "iam-system role must pass through")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRequireSystemRole_Rejects403WhenRoleMissing(t *testing.T) {
	// A tenant_admin — not iam-system — is still a paying customer, not an
	// internal service. RequireSystemRole must 403 them.
	rc := &requestctx.RequestContext{Roles: []string{"tenant_admin", "tenant_owner"}}
	c, w := newTestContext(rc)

	RequireSystemRole()(c)

	assert.True(t, c.IsAborted())
	assert.Equal(t, http.StatusForbidden, w.Code)
	body := decodeBody(t, w)
	assert.Equal(t, "FORBIDDEN", body["errorCode"])
}

func TestRequireSystemRole_Rejects403WhenNoIdentity(t *testing.T) {
	// No requestctx on the request at all — no gateway headers → 403.
	// (Middleware upstream would normally 401, but this specific gate
	// treats the absence as an insufficient-role condition.)
	c, w := newTestContext(nil)

	RequireSystemRole()(c)

	assert.True(t, c.IsAborted())
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────
// RequireOperatorRole — /api/v1/operator/* gate (AUTH-6)
// ─────────────────────────────────────────────────────────────────────────

func TestRequireOperatorRole_AcceptsPlatformOperator(t *testing.T) {
	rc := &requestctx.RequestContext{Roles: []string{"platform_operator"}}
	c, w := newTestContext(rc)

	RequireOperatorRole()(c)

	assert.False(t, c.IsAborted(), "platform_operator must pass through")
	assert.Equal(t, http.StatusOK, w.Code) // default status when handler doesn't set one
}

func TestRequireOperatorRole_RejectsTenantOwner(t *testing.T) {
	// Even the top tenant-level role (tenant_owner) is NOT platform_operator.
	// AUTH-6: only platform_operator may reach /operator/*.
	rc := &requestctx.RequestContext{Roles: []string{"tenant_owner", "tenant_admin", "tender_admin"}}
	c, w := newTestContext(rc)

	RequireOperatorRole()(c)

	assert.True(t, c.IsAborted())
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "FORBIDDEN", decodeBody(t, w)["errorCode"])
}

func TestRequireOperatorRole_RejectsSystemPrincipal(t *testing.T) {
	// iam-system is for internal-mesh service calls, NOT the operator API.
	// AUTH-6 gate must isolate operator surface from internal surface.
	rc := &requestctx.RequestContext{Roles: []string{"iam-system"}}
	c, w := newTestContext(rc)

	RequireOperatorRole()(c)

	assert.True(t, c.IsAborted())
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────
// requireSameTenantMember — cross-tenant defense-in-depth alongside RLS
// ─────────────────────────────────────────────────────────────────────────

func TestRequireSameTenantMember_AcceptsMatchingTenant(t *testing.T) {
	tenantA := uuid.New()
	rc := &requestctx.RequestContext{
		UserID:   uuid.New(),
		TenantID: tenantA,
		Roles:    []string{"tenant_admin"},
	}
	c, _ := newTestContext(rc)

	err := requireSameTenantMember(c, tenantA)
	require.NoError(t, err)
}

func TestRequireSameTenantMember_RejectsCrossTenant(t *testing.T) {
	tenantA, tenantB := uuid.New(), uuid.New()
	// Caller is authenticated for tenantB but reaches for tenantA.
	rc := &requestctx.RequestContext{
		UserID:   uuid.New(),
		TenantID: tenantB,
		Roles:    []string{"tenant_owner"}, // even owner of B can't touch A
	}
	c, _ := newTestContext(rc)

	err := requireSameTenantMember(c, tenantA)
	require.Error(t, err)
	de, ok := err.(*domain.DomainError)
	require.True(t, ok)
	assert.Equal(t, "insufficient_role", de.Code)
}

func TestRequireSameTenantMember_RejectsMissingIdentity(t *testing.T) {
	c, _ := newTestContext(nil)

	err := requireSameTenantMember(c, uuid.New())
	require.Error(t, err)
	de, ok := err.(*domain.DomainError)
	require.True(t, ok)
	assert.Equal(t, "missing_identity_headers", de.Code)
}

// ─────────────────────────────────────────────────────────────────────────
// requireTenantAdmin — AUTH-2 gate for tenant mutations
// ─────────────────────────────────────────────────────────────────────────

func TestRequireTenantAdmin_AcceptsOwner(t *testing.T) {
	tenantA := uuid.New()
	rc := &requestctx.RequestContext{TenantID: tenantA, Roles: []string{"tenant_owner"}}
	c, _ := newTestContext(rc)

	require.NoError(t, requireTenantAdmin(c, tenantA))
}

func TestRequireTenantAdmin_AcceptsAdmin(t *testing.T) {
	tenantA := uuid.New()
	rc := &requestctx.RequestContext{TenantID: tenantA, Roles: []string{"tenant_admin"}}
	c, _ := newTestContext(rc)

	require.NoError(t, requireTenantAdmin(c, tenantA))
}

func TestRequireTenantAdmin_RejectsTenderAdmin(t *testing.T) {
	// tender_admin gates ACL management, NOT membership. requireTenantAdmin
	// must NOT let it through.
	tenantA := uuid.New()
	rc := &requestctx.RequestContext{TenantID: tenantA, Roles: []string{"tender_admin"}}
	c, _ := newTestContext(rc)

	err := requireTenantAdmin(c, tenantA)
	require.Error(t, err)
	de, _ := err.(*domain.DomainError)
	assert.Equal(t, "insufficient_role", de.Code)
}

func TestRequireTenantAdmin_RejectsPlainMember(t *testing.T) {
	tenantA := uuid.New()
	// Empty roles slice — a plain member (derived TR-7, never persisted).
	rc := &requestctx.RequestContext{TenantID: tenantA, Roles: []string{}}
	c, _ := newTestContext(rc)

	err := requireTenantAdmin(c, tenantA)
	require.Error(t, err)
	assert.Equal(t, "insufficient_role", err.(*domain.DomainError).Code)
}

// ─────────────────────────────────────────────────────────────────────────
// requireTenderAdminOrHigher — AUTH-3 gate for ACL routes
// ─────────────────────────────────────────────────────────────────────────

func TestRequireTenderAdminOrHigher_AcceptsTenderAdmin(t *testing.T) {
	tenantA := uuid.New()
	rc := &requestctx.RequestContext{TenantID: tenantA, Roles: []string{"tender_admin"}}
	c, _ := newTestContext(rc)

	require.NoError(t, requireTenderAdminOrHigher(c, tenantA))
}

func TestRequireTenderAdminOrHigher_AcceptsTenantAdmin(t *testing.T) {
	tenantA := uuid.New()
	rc := &requestctx.RequestContext{TenantID: tenantA, Roles: []string{"tenant_admin"}}
	c, _ := newTestContext(rc)

	require.NoError(t, requireTenderAdminOrHigher(c, tenantA))
}

func TestRequireTenderAdminOrHigher_AcceptsTenantOwner(t *testing.T) {
	tenantA := uuid.New()
	rc := &requestctx.RequestContext{TenantID: tenantA, Roles: []string{"tenant_owner"}}
	c, _ := newTestContext(rc)

	require.NoError(t, requireTenderAdminOrHigher(c, tenantA))
}

func TestRequireTenderAdminOrHigher_RejectsPlainMember(t *testing.T) {
	tenantA := uuid.New()
	rc := &requestctx.RequestContext{TenantID: tenantA, Roles: []string{}}
	c, _ := newTestContext(rc)

	err := requireTenderAdminOrHigher(c, tenantA)
	require.Error(t, err)
	assert.Equal(t, "insufficient_role", err.(*domain.DomainError).Code)
}

// ─────────────────────────────────────────────────────────────────────────
// HandleError — §17 domain error → HTTP status mapping. Verifies the 409
// shape for optimistic-lock and the 422 shape for domain-rule violations.
// ─────────────────────────────────────────────────────────────────────────

func TestHandleError_OptimisticLockReturns409WithVersionField(t *testing.T) {
	// The CONC-4 contract: response body carries `record_version` so the
	// client can retry with the fresh value.
	c, w := newTestContext(nil)
	err := domain.NewError(domain.ErrOptimisticLockConflict, "row moved under you").
		WithDetails(map[string]any{
			"record_version": int64(7),
			"updated_at":     "2026-07-21T10:00:00Z",
		})

	HandleError(c, err)

	assert.Equal(t, http.StatusConflict, w.Code, "optimistic_lock_conflict must be 409")
	body := decodeBody(t, w)
	assert.Equal(t, "OPTIMISTIC_LOCK_CONFLICT", body["errorCode"])
	assert.Equal(t, "row moved under you", body["detail"])
	require.Contains(t, body, "record_version", "409 body MUST carry record_version for retry (CONC-4)")
	assert.Equal(t, float64(7), body["record_version"], "record_version echoes the CURRENT db value")
	assert.Contains(t, body, "updated_at")
}

func TestHandleError_LastOwnerRemovalReturns422(t *testing.T) {
	// TM-8: stripping the last tenant_owner MUST be 422 (domain-rule), not
	// 400 (validation) or 409 (state conflict).
	c, w := newTestContext(nil)
	err := domain.NewError(domain.ErrLastOwnerRemoval, "cannot remove the last tenant_owner")

	HandleError(c, err)

	assert.Equal(t, http.StatusUnprocessableEntity, w.Code, "TM-8 last_owner_removal must be 422")
	body := decodeBody(t, w)
	assert.Equal(t, "LAST_OWNER_REMOVAL", body["errorCode"])
}

func TestHandleError_WorkflowResolutionRequiredReturns409WithWorkflowFields(t *testing.T) {
	// §8.8: pre-check on removal returns 409 workflow_resolution_required
	// with an explicit list of workflow_ids and allowed_actions so the UI
	// can render the resolution dialog.
	c, w := newTestContext(nil)
	workflowIDs := []string{"wf-1", "wf-2"}
	err := domain.NewError(domain.ErrWorkflowResolutionRequired, "active workflows must be resolved").
		WithDetails(map[string]any{
			"active_workflows": 2,
			"workflow_ids":     workflowIDs,
			"allowed_actions":  []string{"replace_delegate", "stop_workflows"},
		})

	HandleError(c, err)

	assert.Equal(t, http.StatusConflict, w.Code)
	body := decodeBody(t, w)
	assert.Equal(t, "WORKFLOW_RESOLUTION_REQUIRED", body["errorCode"])
	assert.Equal(t, float64(2), body["active_workflows"])
	assert.Contains(t, body, "workflow_ids")
	assert.Contains(t, body, "allowed_actions")
}

func TestHandleError_SeatLimitReachedReturns409(t *testing.T) {
	// SEAT-1: cap hit at invite time.
	c, w := newTestContext(nil)
	err := domain.NewError(domain.ErrSeatLimitReached, "seat limit reached").
		WithDetails(map[string]any{"licensed_seats": 10})

	HandleError(c, err)

	assert.Equal(t, http.StatusConflict, w.Code)
	body := decodeBody(t, w)
	assert.Equal(t, "SEAT_LIMIT_REACHED", body["errorCode"])
	assert.Equal(t, float64(10), body["licensed_seats"])
}

func TestHandleError_InsufficientRoleReturns403(t *testing.T) {
	c, w := newTestContext(nil)
	err := domain.NewError(domain.ErrInsufficientRole, "tenant_admin required")

	HandleError(c, err)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "INSUFFICIENT_ROLE", decodeBody(t, w)["errorCode"])
}

func TestHandleError_MissingIdentityReturns401(t *testing.T) {
	c, w := newTestContext(nil)
	err := domain.NewError(domain.ErrMissingIdentity, "no identity")

	HandleError(c, err)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, "MISSING_IDENTITY_HEADERS", decodeBody(t, w)["errorCode"])
}

func TestHandleError_ValidationReturns400(t *testing.T) {
	c, w := newTestContext(nil)
	err := domain.NewError(domain.ErrValidation, "bad body")

	HandleError(c, err)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "VALIDATION_ERROR", decodeBody(t, w)["errorCode"])
}

func TestHandleError_TenantNotFoundReturns404(t *testing.T) {
	c, w := newTestContext(nil)
	err := domain.NewError(domain.ErrTenantNotFound, "not visible via RLS")

	HandleError(c, err)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestHandleError_ReinviteTooSoonReturns429(t *testing.T) {
	// PI-11: retry_after_seconds MUST reach the client body.
	c, w := newTestContext(nil)
	err := domain.NewError(domain.ErrReinviteTooSoon, "wait before reinviting").
		WithDetails(map[string]any{"retry_after_seconds": 300})

	HandleError(c, err)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	body := decodeBody(t, w)
	assert.Equal(t, "REINVITE_TOO_SOON", body["errorCode"])
	assert.Equal(t, float64(300), body["retry_after_seconds"])
}

func TestHandleError_DBUnavailableReturns503(t *testing.T) {
	c, w := newTestContext(nil)
	err := domain.NewError(domain.ErrDBUnavailable, "pool exhausted")

	HandleError(c, err)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "DB_UNAVAILABLE", decodeBody(t, w)["errorCode"])
}

func TestHandleError_UnknownDomainErrorFallsBackTo422(t *testing.T) {
	// §17: any domain-rule sentinel that isn't in the explicit switch
	// falls into the 422 default (documented behavior — 422 is the
	// domain-rule bucket).
	c, w := newTestContext(nil)
	err := domain.NewError(domain.ErrInvalidAction, "action not permitted in current state")

	HandleError(c, err)

	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Equal(t, "INVALID_ACTION", decodeBody(t, w)["errorCode"])
}

func TestHandleError_NonDomainErrorReturns500(t *testing.T) {
	// A raw stdlib error MUST NOT leak its Error() to the client. Handler
	// returns a generic "internal_error" body and 500.
	c, w := newTestContext(nil)

	HandleError(c, assert.AnError) // stretchr's sentinel — not a *DomainError

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	body := decodeBody(t, w)
	assert.Equal(t, "INTERNAL_ERROR", body["errorCode"])
	assert.NotContains(t, w.Body.String(), assert.AnError.Error(),
		"raw error message must NOT be surfaced to the client")
}

// B5 fix: raw pgconn.PgError with SQLSTATE class 08/53/57/58 (DB
// connectivity / resource-exhaustion) must surface as 503 db_unavailable
// per LLD §17 (line 2544). Previously all pgconn.PgErrors fell to the
// generic 500 bucket.
func TestHandleError_DBConnectivitySQLState_Returns503(t *testing.T) {
	for _, code := range []string{"08006", "08001", "08004"} { // connection_failure class
		t.Run(code, func(t *testing.T) {
			c, w := newTestContext(nil)
			HandleError(c, &pgconn.PgError{Code: code})
			assert.Equal(t, http.StatusServiceUnavailable, w.Code)
			assert.Contains(t, w.Body.String(), "SERVICE_UNAVAILABLE")
		})
	}
}

func TestHandleError_DBResourceExhaustionSQLState_Returns503(t *testing.T) {
	c, w := newTestContext(nil)
	HandleError(c, &pgconn.PgError{Code: "53300"}) // too_many_connections
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "SERVICE_UNAVAILABLE")
}

// A leaked class 57 (operator_intervention) or 58 (system_error) PgError is
// a database outage (pgcommon.IsOperatorIntervention / IsSystemError).
func TestHandleError_OperatorOrSystemErrorSQLState_Returns503(t *testing.T) {
	for _, code := range []string{"57P01", "58030"} { // admin_shutdown, io_error
		t.Run(code, func(t *testing.T) {
			c, w := newTestContext(nil)
			HandleError(c, &pgconn.PgError{Code: code})
			assert.Equal(t, http.StatusServiceUnavailable, w.Code)
			assert.Contains(t, w.Body.String(), "SERVICE_UNAVAILABLE")
		})
	}
}

// Constraint violations (class 23) are logic errors that should have been
// caught by the repository layer — if they leak here it is an internal bug,
// so they still return 500, not 503.
func TestHandleError_ConstraintViolationSQLState_Returns500(t *testing.T) {
	c, w := newTestContext(nil)
	HandleError(c, &pgconn.PgError{Code: "23505"}) // unique_violation
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "INTERNAL_ERROR")
}

// ─────────────────────────────────────────────────────────────────────────
// RequireJSONContentType — 415 gate on write methods
// ─────────────────────────────────────────────────────────────────────────

func TestRequireJSONContentType_AcceptsJSON(t *testing.T) {
	c, w := newTestContext(nil)
	c.Request.Header.Set("Content-Type", "application/json")

	RequireJSONContentType()(c)

	assert.False(t, c.IsAborted())
	_ = w
}

func TestRequireJSONContentType_Rejects415OnFormPost(t *testing.T) {
	c, w := newTestContext(nil)
	c.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c.Request.ContentLength = 5

	RequireJSONContentType()(c)

	assert.True(t, c.IsAborted())
	assert.Equal(t, http.StatusUnsupportedMediaType, w.Code)
	assert.Equal(t, "UNSUPPORTED_MEDIA_TYPE", decodeBody(t, w)["errorCode"])
}

func TestRequireJSONContentType_SkipsGET(t *testing.T) {
	// GET has no body — content-type is not enforced.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)

	RequireJSONContentType()(c)

	assert.False(t, c.IsAborted())
}

func TestRequireJSONContentType_SkipsEmptyBody(t *testing.T) {
	// A DELETE with Content-Length: 0 — permitted regardless of content-type.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/", nil)
	c.Request.ContentLength = 0

	RequireJSONContentType()(c)

	assert.False(t, c.IsAborted())
}

// A raw statement/lock timeout or exhausted deadlock that skipped
// wrapConnErr still gets its proper code, never a 500.
func TestHandleError_LeakedTimeoutAndConflictSQLStates(t *testing.T) {
	for code, want := range map[string]struct {
		status int
		body   string
	}{
		"57014": {http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE"},
		"55P03": {http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE"},
		"40P01": {http.StatusConflict, "CONFLICT"},
		"40001": {http.StatusConflict, "CONFLICT"},
	} {
		t.Run(code, func(t *testing.T) {
			c, w := newTestContext(nil)
			HandleError(c, &pgconn.PgError{Code: code})
			assert.Equal(t, want.status, w.Code)
			assert.Contains(t, w.Body.String(), want.body)
		})
	}
}
