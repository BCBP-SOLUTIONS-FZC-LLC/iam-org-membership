//go:build e2e

// Phase 13 · realistic end-to-end flows via the running server.
//
//   - P13-FLOW-001: GET /api/v1/tenants/:id — happy path returns 200 with
//     the seeded tenant projection (JSON keys per swagger spec).
//   - P13-FLOW-002: invite → list-invitations two-hop flow (P-6, P-30).
//   - P13-FLOW-003: create → list → cancel delegation (P-19, P-18, P-20).
//   - P13-FLOW-004: seat-usage endpoint returns valid shape (P-27).
//   - P13-FLOW-005: internal GetMemberships hot path returns full projection.
//   - P13-FLOW-006: internal ProvisionTenant (I-1) creates tenant + 5 depts
//     + 3 role labels + owner + role grants — full trial-signup flow.
//   - P13-FLOW-007: PUT roles reconcile (P-28) applies exactly the target
//     set (add + remove semantics).
//
// These are the "does the whole thing actually work when you call it over
// HTTP" tests. Handler-layer validation tests live in Phase 3/9.
//
// Full test-case metadata (Module · Feature · Priority · Severity) lives in
// Reference_doc/Test_metadata_P12_P16.md.
package e2e_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── P13-FLOW-001 ────────────────────────────────────────────────────────────

func TestGetTenantHappyPath(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "flow-001")
	userID := e.seedOwner(t, tenantID)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodGet,
		path:    "/api/v1/tenants/" + tenantID.String(),
		headers: ownerHeaders(userID, tenantID),
	})
	require.Equal(t, http.StatusOK, code,
		"P13-FLOW-001: GET tenant must return 200 (got %d body=%s)", code, string(body))

	var resp map[string]any
	unmarshalBody(t, body, &resp)
	assert.Equal(t, tenantID.String(), resp["id"], "P13-FLOW-001: response id must match seeded tenant")
	assert.Equal(t, "flow-001", resp["slug"], "P13-FLOW-001: slug round-trips")
	assert.Equal(t, "starter", resp["plan"], "P13-FLOW-001: plan round-trips")
	assert.Equal(t, "trial", resp["status"], "P13-FLOW-001: status round-trips")
}

// ── P13-FLOW-002 ────────────────────────────────────────────────────────────

func TestInviteThenList(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "flow-002")
	userID := e.seedOwner(t, tenantID)

	// Step 1 — P-6 invite.
	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPost,
		path:    "/api/v1/tenants/" + tenantID.String() + "/members",
		headers: ownerHeaders(userID, tenantID),
		body: map[string]any{
			"email":     "flow-002@example.com",
			"full_name": "Flow Two Test",
		},
	})
	// P-6 returns 202 Accepted — the invite is queued (KC user creation is
	// async; the seat is held in `pending` state synchronously).
	require.Equal(t, http.StatusAccepted, code,
		"P13-FLOW-002: invite must return 202 (got %d body=%s)", code, string(body))
	var invite map[string]any
	unmarshalBody(t, body, &invite)
	inviteID, _ := invite["invitation_id"].(string)
	require.NotEmpty(t, inviteID, "invitation_id must be present in response")

	// Step 2 — P-30 list invitations.
	code, _, body = e.do(t, reqOpts{
		method:  http.MethodGet,
		path:    "/api/v1/tenants/" + tenantID.String() + "/invitations",
		headers: ownerHeaders(userID, tenantID),
	})
	require.Equal(t, http.StatusOK, code,
		"P13-FLOW-002: list invitations must return 200 (got %d)", code)
	var listResp map[string]any
	unmarshalBody(t, body, &listResp)
	items, _ := listResp["items"].([]any)
	require.Len(t, items, 1, "list must contain the one invitation just created")
	found, _ := items[0].(map[string]any)
	assert.Equal(t, inviteID, found["invitation_id"], "invitation_id round-trips through list")
	assert.Equal(t, "pending", found["status"], "newly invited status must be pending")
}

// ── P13-FLOW-003 ────────────────────────────────────────────────────────────
//
// TestDelegationCreateListCancel removed (ADR-0008 v2): P-18/P-19/P-20
// (list/create/cancel delegation) moved to the standalone Delegation
// Service's DLG-1..3; the routes this test called are retired here and
// return 404 (see retired_routes_test.go). ID never reused.

// ── P13-FLOW-004 ────────────────────────────────────────────────────────────

func TestSeatUsageShape(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "flow-004")
	userID := e.seedOwner(t, tenantID)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodGet,
		path:    "/api/v1/tenants/" + tenantID.String() + "/seat-usage",
		headers: ownerHeaders(userID, tenantID),
	})
	require.Equal(t, http.StatusOK, code,
		"P13-FLOW-004: seat-usage must return 200 (got %d body=%s)", code, string(body))

	var resp map[string]any
	unmarshalBody(t, body, &resp)
	// Owner counts as 1 active user.
	assert.EqualValues(t, 50, resp["licensed_seats"], "P13-FLOW-004: licensed_seats round-trips")
	assert.NotNil(t, resp["active_users"], "P13-FLOW-004: active_users key present")
	assert.NotNil(t, resp["pending_invitations"], "P13-FLOW-004: pending_invitations key present")
}

// ── P13-FLOW-005 ────────────────────────────────────────────────────────────

// TestInternalGetMemberships — I-8 hot path returns full
// projection for a user, callable only with iam-system role.
func TestInternalGetMemberships(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "flow-005")
	userID := e.seedOwner(t, tenantID)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodGet,
		path:    "/api/v1/internal/users/" + userID.String() + "/memberships?tenant_id=" + tenantID.String(),
		headers: systemHeaders(tenantID),
	})
	require.Equal(t, http.StatusOK, code,
		"P13-FLOW-005: I-8 hot path must return 200 (got %d body=%s)", code, string(body))

	var resp map[string]any
	unmarshalBody(t, body, &resp)
	assert.Equal(t, userID.String(), resp["user_id"], "user_id round-trips")
	assert.Equal(t, tenantID.String(), resp["tenant_id"], "tenant_id round-trips")
	assert.Equal(t, "active", resp["status"], "membership status projected")
	assert.Equal(t, "starter", resp["plan"], "plan projected from tenant")
}

// ── P13-FLOW-006 ────────────────────────────────────────────────────────────

// TestInternalProvisionTenant — full trial signup via I-1.
func TestInternalProvisionTenant(t *testing.T) {
	e := newE2EEnv(t)

	// The e2e harness's fakeCatalogDepartments/fakeCatalogPlans already seed
	// 3 plans + 5 system departments (§8.1) in place of the dropped
	// plans/departments tables (now owned by the Catalog Service). Nothing
	// to bootstrap here.

	tenantID := uuid.New()
	ownerID := uuid.New()

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPost,
		path:    "/api/v1/internal/tenants",
		headers: systemHeaders(tenantID),
		body: map[string]any{
			"tenant_id":     tenantID.String(),
			"slug":          "flow-006-corp",
			"name":          "Flow 006 Corp",
			"plan":          "starter",
			"owner_user_id": ownerID.String(),
		},
	})
	require.Equal(t, http.StatusCreated, code,
		"P13-FLOW-006: I-1 provision tenant must return 201 (got %d body=%s)", code, string(body))

	// Verify all §8.1 outputs materialised.
	var tCount, dmCount, trCount, tdCount, rlCount int
	require.NoError(t, e.rawPool.QueryRow(e.ctx,
		`SELECT count(*) FROM tenants WHERE id = $1`, tenantID).Scan(&tCount))
	assert.Equal(t, 1, tCount, "P13-FLOW-006: tenant row inserted")
	require.NoError(t, e.rawPool.QueryRow(e.ctx,
		`SELECT count(*) FROM tenant_departments WHERE tenant_id = $1`, tenantID).Scan(&tdCount))
	assert.Equal(t, 5, tdCount, "P13-FLOW-006: 5 default depts activated")
	require.NoError(t, e.rawPool.QueryRow(e.ctx,
		`SELECT count(*) FROM tenant_memberships WHERE tenant_id = $1 AND user_id = $2`,
		tenantID, ownerID).Scan(&dmCount))
	assert.Equal(t, 1, dmCount, "P13-FLOW-006: owner membership created")
	require.NoError(t, e.rawPool.QueryRow(e.ctx,
		`SELECT count(*) FROM tenant_roles WHERE tenant_id = $1 AND user_id = $2 AND role_code = 'tenant_owner'`,
		tenantID, ownerID).Scan(&trCount))
	assert.Equal(t, 1, trCount, "P13-FLOW-006: tenant_owner role granted")
	require.NoError(t, e.rawPool.QueryRow(e.ctx,
		`SELECT count(*) FROM dept_role_labels WHERE tenant_id = $1`, tenantID).Scan(&rlCount))
	assert.Equal(t, 3, rlCount, "P13-FLOW-006: 3 default role labels seeded")

	// Verify events queued to outbox (TenantCreated + TrialStarted at minimum).
	var outboxCount int
	require.NoError(t, e.rawPool.QueryRow(e.ctx,
		`SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, tenantID.String()).Scan(&outboxCount))
	assert.GreaterOrEqual(t, outboxCount, 2,
		"P13-FLOW-006: at least TenantCreated + TrialStarted must be queued")
}

// ── P13-FLOW-007 ────────────────────────────────────────────────────────────

// TestReconcileRoles — PUT roles is idempotent-declarative:
// send the desired set, get exactly that set (grants missing, revokes extras).
func TestReconcileRoles(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "flow-007")
	owner := e.seedOwner(t, tenantID)
	target := e.seedActiveMember(t, tenantID)

	// Target starts with zero elevated roles. Reconcile → [tender_admin].
	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPut,
		path:    "/api/v1/tenants/" + tenantID.String() + "/members/" + target.String() + "/roles",
		headers: ownerHeaders(owner, tenantID),
		body:    map[string]any{"roles": []string{"tender_admin"}},
	})
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, code,
		"P13-FLOW-007: reconcile must succeed (got %d body=%s)", code, string(body))

	var got []string
	rows, err := e.rawPool.Query(e.ctx,
		`SELECT role_code FROM tenant_roles WHERE tenant_id = $1 AND user_id = $2 ORDER BY role_code`,
		tenantID, target)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var rc string
		require.NoError(t, rows.Scan(&rc))
		got = append(got, rc)
	}
	assert.Equal(t, []string{"tender_admin"}, got,
		"P13-FLOW-007: tenant_roles must contain exactly the reconciled set")
}
