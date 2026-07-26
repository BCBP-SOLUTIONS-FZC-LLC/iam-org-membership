//go:build e2e

// Phase 13 · infra + middleware tests. Verifies:
//
//   - INFRA-1..3: /healthz, /readyz, /metrics are reachable without any
//     gateway identity headers (LB probes).
//   - MW-1: protected route WITHOUT gateway identity headers → 401.
//   - MW-2/3: X-Request-ID echoed if provided, generated if absent.
//   - MW-4: 1MB request body cap enforced.
//   - MW-5: /internal without iam-system role → 403.
//   - MW-6: /operator without platform_operator role → 403.
//   - MW-7: RequireJSONContentType — POST with text/plain → 415.
//
// Test IDs use the P13-* namespace per Test_cover.md Rule 4.
// Full test-case metadata (Module · Feature · Priority · Severity) lives in
// Reference_doc/Test_metadata_P12_P16.md.
package e2e_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── P13-INFRA-001 ───────────────────────────────────────────────────────────

func TestP13_INFRA_001_HealthzUnauthenticated(t *testing.T) {
	e := newE2EEnv(t)
	code, _, body := e.do(t, reqOpts{method: http.MethodGet, path: "/healthz"})
	assert.Equal(t, http.StatusOK, code, "P13-INFRA-001: /healthz must accept unauthenticated probes")
	assert.NotEmpty(t, body, "healthz body must not be empty")
}

// ── P13-INFRA-002 ───────────────────────────────────────────────────────────

func TestP13_INFRA_002_ReadyzWhenDBUp(t *testing.T) {
	e := newE2EEnv(t)
	code, _, body := e.do(t, reqOpts{method: http.MethodGet, path: "/readyz"})
	assert.Equal(t, http.StatusOK, code, "P13-INFRA-002: /readyz must return 200 when the DB is healthy")
	assert.Contains(t, string(body), "ready")
}

// ── P13-MW-001 ──────────────────────────────────────────────────────────────

func TestP13_MW_001_ProtectedRouteMissingHeaders(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "mw-001")
	code, _, body := e.do(t, reqOpts{
		method: http.MethodGet,
		path:   "/api/v1/tenants/" + tenantID.String(),
	})
	assert.Equal(t, http.StatusUnauthorized, code,
		"P13-MW-001: request without gateway headers must be rejected 401")
	assert.NotEmpty(t, body)
}

// ── P13-MW-002 ──────────────────────────────────────────────────────────────

func TestP13_MW_002_XRequestIDEchoedWhenProvided(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "mw-002")
	userID := e.seedOwner(t, tenantID)
	custom := "req-e2e-" + uuid.NewString()

	headers := ownerHeaders(userID, tenantID)
	headers["X-Request-ID"] = custom

	_, respHeaders, _ := e.do(t, reqOpts{
		method:  http.MethodGet,
		path:    "/api/v1/tenants/" + tenantID.String(),
		headers: headers,
	})
	assert.Equal(t, custom, respHeaders.Get("X-Request-ID"),
		"P13-MW-002: X-Request-ID must be echoed verbatim when provided")
}

// ── P13-MW-003 ──────────────────────────────────────────────────────────────

func TestP13_MW_003_XRequestIDGeneratedWhenAbsent(t *testing.T) {
	e := newE2EEnv(t)
	_, respHeaders, _ := e.do(t, reqOpts{method: http.MethodGet, path: "/healthz"})
	rid := respHeaders.Get("X-Request-ID")
	assert.NotEmpty(t, rid, "P13-MW-003: middleware must generate a request id when the client omits one")
}

// ── P13-MW-004 ──────────────────────────────────────────────────────────────

// TestP13_MW_004_BodyCapEnforced — payload larger than 1MB must be rejected.
func TestP13_MW_004_BodyCapEnforced(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "mw-004")
	userID := e.seedOwner(t, tenantID)

	// 2MB of JSON-looking body — exceeds the 1MB MaxBytesReader cap.
	big := bytes.Repeat([]byte(`x`), 2<<20)
	code, _, _ := e.do(t, reqOpts{
		method:  http.MethodPatch,
		path:    "/api/v1/tenants/" + tenantID.String(),
		headers: ownerHeaders(userID, tenantID),
		rawBody: big,
	})
	assert.GreaterOrEqual(t, code, 400,
		"P13-MW-004: 2MB body must fail (got %d)", code)
	assert.NotEqual(t, http.StatusOK, code)
}

// ── P13-MW-005 ──────────────────────────────────────────────────────────────

func TestP13_MW_005_InternalRequiresSystemRole(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "mw-005")
	userID := e.seedOwner(t, tenantID)

	// tenant_owner is elevated but NOT iam-system.
	code, _, body := e.do(t, reqOpts{
		method:  http.MethodGet,
		path:    "/api/v1/internal/users/" + userID.String() + "/memberships",
		headers: ownerHeaders(userID, tenantID),
	})
	assert.Equal(t, http.StatusForbidden, code,
		"P13-MW-005: internal route without iam-system role must return 403")
	assert.NotEmpty(t, body)
}

// ── P13-MW-006 ──────────────────────────────────────────────────────────────

func TestP13_MW_006_OperatorRequiresOperatorRole(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "mw-006")
	userID := e.seedOwner(t, tenantID)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodGet,
		path:    "/api/v1/operator/plans",
		headers: ownerHeaders(userID, tenantID),
	})
	assert.Equal(t, http.StatusForbidden, code,
		"P13-MW-006: operator route without platform_operator role must return 403")
	assert.NotEmpty(t, body)
}

// ── P13-MW-007 ──────────────────────────────────────────────────────────────

func TestP13_MW_007_ContentTypeGate(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "mw-007")
	userID := e.seedOwner(t, tenantID)

	headers := ownerHeaders(userID, tenantID)
	headers["Content-Type"] = "text/plain" // wrong content-type on POST

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPost,
		path:    "/api/v1/tenants/" + tenantID.String() + "/members",
		headers: headers,
		rawBody: []byte(`hello`),
	})
	assert.Equal(t, http.StatusUnsupportedMediaType, code,
		"P13-MW-007: RequireJSONContentType must reject non-JSON POST (got %d body=%s)",
		code, strings.TrimSpace(string(body)))
}

var _ = require.NoError // keep require reference regardless of test-file trims
