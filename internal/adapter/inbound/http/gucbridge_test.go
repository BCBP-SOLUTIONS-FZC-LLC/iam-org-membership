package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseBridgedIdentity is the pure helper GUCBridgeMiddleware uses to turn
// primitive gateway header values into a typed requestctx.RequestContext.
// The middleware wrapper is exercised end-to-end in test/postgres; here we
// lock every code path of the parser without depending on gincommon's
// internal RequestContext type.

func TestParseBridgedIdentity_ValidHeadersReturnTypedRC(t *testing.T) {
	userID, tenantID := uuid.New(), uuid.New()
	rc, er := parseBridgedIdentity(bridgedIdentity{
		UserIDStr:   userID.String(),
		TenantIDStr: tenantID.String(),
		Roles:       []string{"tenant_owner"},
		ClientIP:    "10.0.0.1",
		UserAgent:   "curl/8",
	})
	require.Nil(t, er)
	require.NotNil(t, rc)
	assert.Equal(t, userID, rc.UserID)
	assert.Equal(t, tenantID, rc.TenantID)
	assert.Equal(t, []string{"tenant_owner"}, rc.Roles)
	assert.Equal(t, "10.0.0.1", rc.ClientIP)
	assert.Equal(t, "curl/8", rc.UserAgent)
}

func TestParseBridgedIdentity_IamSystemUserIDMapsToUUIDNil(t *testing.T) {
	tenantID := uuid.New()
	rc, er := parseBridgedIdentity(bridgedIdentity{
		UserIDStr:   "iam-system",
		TenantIDStr: tenantID.String(),
		Roles:       []string{"iam-system"},
	})
	require.Nil(t, er)
	assert.Equal(t, uuid.Nil, rc.UserID,
		"the reserved iam-system principal is stored as uuid.Nil per RLS-5/IAPI-2")
	assert.Equal(t, tenantID, rc.TenantID)
	assert.True(t, rc.HasRole("iam-system"))
}

func TestParseBridgedIdentity_InvalidUserIDReturns401(t *testing.T) {
	rc, er := parseBridgedIdentity(bridgedIdentity{
		UserIDStr:   "not-a-uuid",
		TenantIDStr: uuid.New().String(),
	})
	require.Nil(t, rc, "malformed user id yields no rc")
	require.NotNil(t, er)
	assert.Equal(t, http.StatusUnauthorized, er.Status)
	assert.Equal(t, "missing_identity_headers", er.Code)
	assert.Contains(t, er.Message, "x-user-id")
}

func TestParseBridgedIdentity_InvalidTenantIDReturns401(t *testing.T) {
	rc, er := parseBridgedIdentity(bridgedIdentity{
		UserIDStr:   uuid.New().String(),
		TenantIDStr: "not-a-uuid",
	})
	require.Nil(t, rc)
	require.NotNil(t, er)
	assert.Equal(t, http.StatusUnauthorized, er.Status)
	assert.Contains(t, er.Message, "x-tenant-id")
}

func TestParseBridgedIdentity_IamSystemStillRequiresValidTenantID(t *testing.T) {
	// Even for the system principal, tenant_id must be a real UUID —
	// internal endpoints still bind RLS on a target tenant.
	rc, er := parseBridgedIdentity(bridgedIdentity{
		UserIDStr:   "iam-system",
		TenantIDStr: "garbage",
	})
	assert.Nil(t, rc)
	require.NotNil(t, er)
	assert.Contains(t, er.Message, "x-tenant-id")
}

// ── RegisterValidators — idempotency guard ─────────────────────────────

func TestRegisterValidators_SetsFlagAndIsIdempotent(t *testing.T) {
	validatorsRegistered = false
	RegisterValidators()
	assert.True(t, validatorsRegistered)

	// Second call must be a safe no-op — flag stays set.
	RegisterValidators()
	assert.True(t, validatorsRegistered)
}

// ── GUCBridgeMiddleware factory + no-context path ──────────────────────
//
// The factory returns a gin.HandlerFunc closure. We can't unit-test the
// happy-path branches inside the closure (they depend on gincommon's
// internal *domain.RequestContext type — not constructible from outside
// the gincommon module), but we CAN:
//  1. Call the factory to cover the outer function.
//  2. Invoke the returned closure with a gin.Context that has NO upstream
//     platformRc — this covers the `if !ok { c.Next(); return }` early-
//     return branch inside the closure.
// The rest of the closure body is exercised end-to-end in test/postgres
// where the real gincommon ContextMiddleware fires before this one.

func TestGUCBridgeMiddleware_FactoryReturnsHandler(t *testing.T) {
	h := GUCBridgeMiddleware()
	require.NotNil(t, h, "factory must return a non-nil gin.HandlerFunc")
}

func TestGUCBridgeMiddleware_NoUpstreamContextFallsThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := GUCBridgeMiddleware()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "/",
		strings.NewReader(""))

	// With no gincommon.RequestContext on c, the closure hits the early-
	// return branch and passes control to the next handler.
	h(c)

	assert.Equal(t, http.StatusOK, w.Code, "no upstream ctx → pass through")
	assert.False(t, c.IsAborted(), "middleware must not abort when there's no upstream ctx")
}
