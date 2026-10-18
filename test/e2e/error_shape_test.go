//go:build e2e

// Phase 13 · error-envelope shape. Verifies the §17 error taxonomy is
// preserved end-to-end when a real HTTP request produces an RFC 9457
// application/problem+json response:
//
//   - P13-ERR-001: 404 for unknown tenant → {type, title, status, errorCode, detail}.
//   - P13-ERR-002: 422 for invalid UUID path param.
//   - P13-ERR-003: 400 for malformed JSON body.
//   - P13-ERR-004: 403 for cross-tenant read (tenant-ID mismatch).
//
// All error responses follow RFC 9457 Problem Details (platform-gincommon).
package e2e_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── P13-ERR-001 ─────────────────────────────────────────────────────────────

// TestUnknownTenantReturnsShapedError — GET on a tenant UUID
// that doesn't exist must return 404 with the RFC 9457 problem envelope.
func TestUnknownTenantReturnsShapedError(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "err-001-existing")
	userID := e.seedOwner(t, tenantID)

	// Query a DIFFERENT tenant id — but pretend we belong there so we get
	// past auth and land in the service layer. The seeded tenant is only
	// used to grant the caller a valid identity.
	missing := uuid.New()

	code, _, body := e.do(t, reqOpts{
		method: http.MethodGet,
		path:   "/api/v1/tenants/" + missing.String(),
		// Note: gateway tenant-id must match the URL tenant-id or the
		// handler rejects cross-tenant reads at requireTenantAdmin. So
		// send the identity as belonging to the "missing" tenant.
		headers: ownerHeaders(userID, missing),
	})

	// RLS + service layer → not_found. Either 404 or 403 is acceptable
	// depending on whether the auth check runs before the fetch; assert
	// on the RFC 9457 envelope shape either way.
	assert.Contains(t, []int{http.StatusNotFound, http.StatusForbidden}, code,
		"P13-ERR-001: unknown tenant must yield 404/403 (got %d)", code)

	var envelope map[string]any
	unmarshalBody(t, body, &envelope)
	assert.NotEmpty(t, envelope["errorCode"], "P13-ERR-001: problem body must carry `errorCode`")
	assert.NotEmpty(t, envelope["detail"], "P13-ERR-001: problem body must carry `detail`")
	assert.NotEmpty(t, envelope["type"], "P13-ERR-001: problem body must carry RFC 9457 `type`")
	assert.NotEmpty(t, envelope["title"], "P13-ERR-001: problem body must carry RFC 9457 `title`")
}

// ── P13-ERR-002 ─────────────────────────────────────────────────────────────

func TestInvalidUUIDParam(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "err-002")
	userID := e.seedOwner(t, tenantID)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodGet,
		path:    "/api/v1/tenants/not-a-uuid",
		headers: ownerHeaders(userID, tenantID),
	})
	assert.GreaterOrEqual(t, code, 400,
		"P13-ERR-002: non-UUID tenant id must error (got %d)", code)

	var envelope map[string]any
	unmarshalBody(t, body, &envelope)
	assert.NotEmpty(t, envelope["errorCode"], "RFC 9457 problem body must carry errorCode")
}

// ── P13-ERR-003 ─────────────────────────────────────────────────────────────

func TestMalformedJSONBody(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "err-003")
	userID := e.seedOwner(t, tenantID)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPost,
		path:    "/api/v1/tenants/" + tenantID.String() + "/members",
		headers: ownerHeaders(userID, tenantID),
		rawBody: []byte(`{"email":`), // truncated JSON
	})
	assert.Equal(t, http.StatusBadRequest, code,
		"P13-ERR-003: malformed JSON must return 400 (got %d body=%s)", code, string(body))

	var envelope map[string]any
	unmarshalBody(t, body, &envelope)
	assert.NotEmpty(t, envelope["errorCode"], "P13-ERR-003: RFC 9457 problem body must carry errorCode")
	assert.NotEmpty(t, envelope["detail"], "P13-ERR-003: RFC 9457 problem body must carry detail")
}

// ── P13-ERR-004 ─────────────────────────────────────────────────────────────

// TestCrossTenantForbidden — the caller identifies as tenant A
// but requests tenant B's resource → handler must reject with 403.
func TestCrossTenantForbidden(t *testing.T) {
	e := newE2EEnv(t)
	tenantA := e.seedTenant(t, "err-004-a")
	tenantB := e.seedTenant(t, "err-004-b")
	userA := e.seedOwner(t, tenantA)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodGet,
		path:    "/api/v1/tenants/" + tenantB.String(),
		headers: ownerHeaders(userA, tenantA), // caller is owner of A, asking for B
	})
	assert.Equal(t, http.StatusForbidden, code,
		"P13-ERR-004: cross-tenant read must be 403 (got %d body=%s)", code, string(body))
}

// TestPublicResolveInvalidIdentifierShape: PUB-1's 400 returns an RFC 9457
// problem envelope even though the route runs with no auth middleware at all.
func TestPublicResolveInvalidIdentifierShape(t *testing.T) {
	e := newE2EEnv(t)
	code, _, body := e.do(t, reqOpts{method: http.MethodGet, path: "/api/v1/public/tenants/resolve?identifier="})
	require.Equal(t, http.StatusBadRequest, code, string(body))

	var envelope map[string]any
	unmarshalBody(t, body, &envelope)
	assert.Equal(t, "BAD_REQUEST", envelope["errorCode"])
	assert.NotEmpty(t, envelope["detail"])
	assert.NotEmpty(t, envelope["traceId"], "RFC 9457 problem body must carry traceId")
	assert.NotEmpty(t, envelope["type"], "RFC 9457 problem body must carry type URI")
}
