//go:build integration

package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resolvePublic(t *testing.T, e *apiTestEnv, identifier string, hdrs map[string]string) (int, map[string]any) {
	t.Helper()
	resp := e.do(t, http.MethodGet, "/api/v1/public/tenants/resolve?identifier="+identifier, "", hdrs)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body), string(raw))
	return resp.StatusCode, body
}

// TestAPIScenarios_PUB1_PublicRouteWiring runs PUB-1 through the full
// production router composition (same NewRouter call as cmd/server) on
// real Postgres, across the other layers that shape its answer.
func TestAPIScenarios_PUB1_PublicRouteWiring(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)
	slug := "test-" + tt.TenantID[:8]

	t.Run("PUB1-W-01 zero headers reach the handler, a protected route 401s", func(t *testing.T) {
		code, body := resolvePublic(t, e, slug, nil)
		require.Equal(t, http.StatusOK, code, "PUB-1 must need no headers at all (PUBRES-1)")
		assert.Equal(t, true, body["found"])

		// Contrast: the same bare request to a protected /api/v1 route. If
		// registerPublicRoutes were ever moved under the protected group,
		// PUB-1 would 401 exactly like this.
		resp := e.do(t, http.MethodGet, "/api/v1/tenants/"+tt.TenantID, "", nil)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("PUB1-W-02 identity headers are ignored, not required", func(t *testing.T) {
		code, _ := resolvePublic(t, e, slug, isSys(tt.TenantID))
		assert.Equal(t, http.StatusOK, code)
	})

	t.Run("PUB1-W-03 a provisioned trial tenant resolves to the shared realm", func(t *testing.T) {
		_, body := resolvePublic(t, e, slug, nil)
		assert.Equal(t, map[string]any{"found": true, "realm": "trial", "strategy": "shared", "tenant_id": tt.TenantID}, body)
	})

	t.Run("PUB1-W-04 an active member's email is not resolvable (LLD §16 OQ-11)", func(t *testing.T) {
		// newTestTenant added member@test.com through I-3; O&M keeps no
		// email for an active member, so there is nothing to match.
		code, body := resolvePublic(t, e, "member@test.com", nil)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, map[string]any{"found": false}, body)
	})

	t.Run("PUB1-W-05 after I-2 converts it, the tenant resolves to its dedicated realm", func(t *testing.T) {
		rvT := e.getTenantRV(t, tt.TenantID, tt.OwnerID)
		resp := e.do(t, http.MethodPatch, "/api/v1/internal/tenants/"+tt.TenantID, toJSON(map[string]any{
			"realm_id": "tenant-" + slug, "realm_type": "dedicated", "keycloak_shard": "shard-0", "record_version": rvT,
		}), isSys(tt.TenantID))
		defer func() { _ = resp.Body.Close() }()
		assertStatus(t, resp, http.StatusOK)

		_, body := resolvePublic(t, e, slug, nil)
		assert.Equal(t, map[string]any{"found": true, "realm": "tenant-" + slug, "strategy": "dedicated"}, body,
			"a dedicated tenant never exposes tenant_id pre-login (PUBRES-4)")
	})
}
