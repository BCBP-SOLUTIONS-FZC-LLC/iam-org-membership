//go:build e2e

// Phase 14 · security tests. Uses the same e2e harness (real Gin router +
// RLS-enforcing pgcommon.Pool + gateway-forwarded identity headers) to
// exercise the malicious-input and abuse-of-privilege boundary conditions
// named in Test_cover.md Phase 14.
//
// Coverage:
//
//   - SQL injection — proves parameterized queries store the raw string as
//     data (not interpreted); attackers can't smuggle DROP/UNION.
//   - XSS — payload strings round-trip verbatim, never HTML-escaped by the
//     server (safe because the API responds with JSON; escaping is the
//     rendering layer's job — but they must NOT execute server-side).
//   - AUTH edge cases — malformed / partial gateway headers rejected 401.
//   - Parameter tampering — record_version fabrications, body-actor spoof,
//     cross-tenant path IDs all rejected.
//   - Privilege escalation — non-elevated callers cannot grant themselves
//     tenant_owner / tenant_admin via P-28 role reconcile.
//   - Cross-tenant — every mutating entry point rejects when caller's
//     tenant differs from the URL tenant.
//   - Replay — same envelope submitted twice → second call is a business-
//     level 409, not a silent duplicate.
//
// Test IDs use the P14-* namespace per Test_cover.md Rule 4.
// Full test-case metadata (Module · Feature · Priority · Severity) lives in
// Reference_doc/Test_metadata_P12_P16.md.
package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// ── P14-SQLI-001 ────────────────────────────────────────────────────────────

// TestInviteEmailSQLInjection — a classic tautology payload in
// the invite email must be stored verbatim (parameterized query), not
// interpreted. The tenants row must NOT be dropped.
func TestInviteEmailSQLInjection(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "sqli-001")
	userID := e.seedOwner(t, tenantID)

	payload := `victim@example.com'; DROP TABLE tenants; --`
	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPost,
		path:    "/api/v1/tenants/" + tenantID.String() + "/members",
		headers: ownerHeaders(userID, tenantID),
		body: map[string]any{
			"email":     payload,
			"full_name": "SQLi Test",
		},
	})
	// The service may reject the email as invalid (422 validation) OR accept
	// it and store the literal string. Either way, the tenants table must
	// still exist afterwards.
	assert.NotEqual(t, http.StatusInternalServerError, code,
		"P14-SQLI-001: SQLi attempt must not 500 (got %d body=%s)", code, string(body))

	var count int
	require.NoError(t, e.rawPool.QueryRow(e.ctx,
		`SELECT count(*) FROM tenants WHERE id = $1`, tenantID).Scan(&count))
	assert.Equal(t, 1, count, "P14-SQLI-001: tenants row must survive the SQLi attempt")

	// Confirm the payload was stored verbatim (proof of parameterization) if
	// accepted. InvitationService.normalizeEmail lowercases every email
	// before storage (case-insensitive dedup, applies uniformly to all
	// invites) — that's the one intentional transformation; the tautology
	// payload itself must otherwise survive untouched.
	if code == http.StatusAccepted || code == http.StatusCreated {
		var stored string
		err := e.rawPool.QueryRow(e.ctx,
			`SELECT email FROM pending_invitations WHERE tenant_id = $1 LIMIT 1`, tenantID).Scan(&stored)
		if err == nil {
			assert.Equal(t, strings.ToLower(payload), stored, "SQLi payload stored verbatim modulo case-normalization")
		}
	}
}

// ── P14-SQLI-002 ────────────────────────────────────────────────────────────

// TestRoleLabelSQLInjection — patch a role label's display name
// with a classic UNION SELECT probe. Table must be intact after.
func TestRoleLabelSQLInjection(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "sqli-002")
	userID := e.seedOwner(t, tenantID)

	// Seed a role label first so PATCH has a target.
	_, err := e.rawPool.Exec(e.ctx, `
		INSERT INTO dept_role_labels (id, tenant_id, role_code, display_name)
		VALUES ($1, $2, 'preparator', 'Preparator')`,
		uuid.New(), tenantID)
	require.NoError(t, err)

	// Fetch the current record_version so the PATCH passes CONC-1.
	var ver int64
	require.NoError(t, e.rawPool.QueryRow(e.ctx,
		`SELECT record_version FROM dept_role_labels WHERE tenant_id = $1 AND role_code = 'preparator'`,
		tenantID).Scan(&ver))

	payload := `Boss' UNION SELECT password FROM users --`
	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPatch,
		path:    "/api/v1/tenants/" + tenantID.String() + "/roles/preparator",
		headers: ownerHeaders(userID, tenantID),
		body: map[string]any{
			"display_name":   payload,
			"record_version": ver,
		},
	})
	assert.NotEqual(t, http.StatusInternalServerError, code,
		"P14-SQLI-002: SQLi must not 500 (got %d body=%s)", code, string(body))

	// dept_role_labels row still exists, and if PATCH accepted, display_name
	// is stored verbatim (parameterized).
	if code == http.StatusOK {
		var stored string
		require.NoError(t, e.rawPool.QueryRow(e.ctx,
			`SELECT display_name FROM dept_role_labels WHERE tenant_id = $1 AND role_code = 'preparator'`,
			tenantID).Scan(&stored))
		assert.Equal(t, payload, stored, "P14-SQLI-002: SQLi payload stored verbatim (parameterization)")
	}
}

// ── P14-XSS-001 ─────────────────────────────────────────────────────────────

// TestScriptTagInInvitationName — a <script> payload in full_name
// must be stored verbatim server-side (no interpretation) AND the JSON
// response must Unicode-escape `<`/`>` per Go's encoding/json default
// (defense in depth — prevents accidental HTML execution if JSON is ever
// mis-served as text/html).
func TestScriptTagInInvitationName(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "xss-001")
	userID := e.seedOwner(t, tenantID)

	payload := `<script>alert(document.cookie)</script>`
	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPost,
		path:    "/api/v1/tenants/" + tenantID.String() + "/members",
		headers: ownerHeaders(userID, tenantID),
		body: map[string]any{
			"email":     "xss001@example.com",
			"full_name": payload,
		},
	})
	require.Equal(t, http.StatusAccepted, code,
		"P14-XSS-001: invite with script tag must succeed at API level (got %d body=%s)", code, string(body))

	// JSON response must NOT contain a raw `<script>` sequence — Go's
	// encoding/json HTML-escapes `<`/`>`/`&` by default. The escaped form
	// `<script>` (bytes: literal backslash u003c) is what
	// callers see. This is defense-in-depth against mis-serving JSON as
	// text/html downstream.
	assert.NotContains(t, string(body), "<script>",
		"P14-XSS-001: JSON response must Unicode-escape `<` (defense in depth)")
	assert.Contains(t, string(body), "\\u003cscript\\u003e",
		"P14-XSS-001: response must carry the Unicode-escaped form (bytes: backslash-u003c)")

	// DB stores the raw payload verbatim (parameterized query, no
	// server-side munging).
	var stored string
	require.NoError(t, e.rawPool.QueryRow(e.ctx,
		`SELECT full_name FROM pending_invitations WHERE tenant_id = $1 LIMIT 1`,
		tenantID).Scan(&stored))
	assert.Equal(t, payload, stored,
		"P14-XSS-001: DB must hold the raw string (proves no server-side interpretation)")
}

// ── P14-XSS-002 ─────────────────────────────────────────────────────────────

// TestHTMLInDelegationReason — same defense-in-depth check as
// XSS-001 but for delegation reason: DB stores raw, JSON escapes `<`.
func TestHTMLInDelegationReason(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "xss-002")
	delegator := e.seedOwner(t, tenantID)
	delegate := e.seedActiveMember(t, tenantID)

	payload := `<img src=x onerror="alert(1)">`
	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPost,
		path:    "/api/v1/delegations",
		headers: ownerHeaders(delegator, tenantID),
		body: map[string]any{
			"delegate_id": delegate.String(),
			"scope":       "all",
			"reason":      payload,
		},
	})
	require.Equal(t, http.StatusCreated, code,
		"P14-XSS-002: delegation create with HTML reason must succeed (got %d body=%s)", code, string(body))

	// JSON response must escape `<`.
	assert.NotContains(t, string(body), "<img",
		"P14-XSS-002: JSON response must Unicode-escape `<` (defense in depth)")

	// DB stores raw payload verbatim.
	var stored string
	require.NoError(t, e.rawPool.QueryRow(e.ctx,
		`SELECT reason FROM delegations WHERE delegator_id = $1`, delegator).Scan(&stored))
	assert.Equal(t, payload, stored,
		"P14-XSS-002: DB stores raw string (parameterized, no interpretation)")
}

// ── P14-AUTH-001 ────────────────────────────────────────────────────────────

// TestEmptyUserIDHeaderRejected — x-user-id present but empty
// must be treated as missing (401), not as an anonymous session.
func TestEmptyUserIDHeaderRejected(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "auth-001")

	code, _, body := e.do(t, reqOpts{
		method: http.MethodGet,
		path:   "/api/v1/tenants/" + tenantID.String(),
		headers: map[string]string{
			"x-user-id":      "",
			"x-tenant-id":    tenantID.String(),
			"x-tenant-roles": "tenant_owner",
		},
	})
	assert.Equal(t, http.StatusUnauthorized, code,
		"P14-AUTH-001: empty x-user-id must yield 401 (got %d body=%s)", code, string(body))
}

// ── P14-AUTH-002 ────────────────────────────────────────────────────────────

// TestEmptyTenantIDHeaderRejected — x-tenant-id present but
// empty must be treated as missing.
func TestEmptyTenantIDHeaderRejected(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "auth-002")

	code, _, body := e.do(t, reqOpts{
		method: http.MethodGet,
		path:   "/api/v1/tenants/" + tenantID.String(),
		headers: map[string]string{
			"x-user-id":      uuid.NewString(),
			"x-tenant-id":    "",
			"x-tenant-roles": "tenant_owner",
		},
	})
	assert.Equal(t, http.StatusUnauthorized, code,
		"P14-AUTH-002: empty x-tenant-id must yield 401 (got %d body=%s)", code, string(body))
}

// ── P14-AUTH-003 ────────────────────────────────────────────────────────────

// TestNonUUIDTenantIDHeaderRejected — a well-formed but non-UUID
// x-tenant-id must be rejected by the GUCBridge / handler layer.
func TestNonUUIDTenantIDHeaderRejected(t *testing.T) {
	e := newE2EEnv(t)
	code, _, body := e.do(t, reqOpts{
		method: http.MethodGet,
		path:   "/api/v1/tenants/" + uuid.NewString(),
		headers: map[string]string{
			"x-user-id":      uuid.NewString(),
			"x-tenant-id":    "not-a-uuid",
			"x-tenant-roles": "tenant_owner",
		},
	})
	assert.GreaterOrEqual(t, code, 400,
		"P14-AUTH-003: non-UUID x-tenant-id must be rejected (got %d body=%s)", code, string(body))
	assert.NotEqual(t, http.StatusOK, code)
}

// ── P14-AUTH-004 ────────────────────────────────────────────────────────────

// TestUnknownRoleCannotElevate — a role string the gateway
// didn't grant (e.g. "definitely_not_a_real_role") must not open protected
// routes — RequireOperatorRole / RequireSystemRole reject by exact match.
func TestUnknownRoleCannotElevate(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "auth-004")
	userID := uuid.New()

	// O-1/O-2/O-3/O-5/O-6 moved to the Catalog / Admin Config Service
	// (migration-runbook Phase 4); O-4 is the remaining RequireOperatorRole
	// route to exercise this gate against.
	code, _, body := e.do(t, reqOpts{
		method: http.MethodPatch,
		path:   "/api/v1/operator/tenants/" + tenantID.String() + "/feature-flags",
		headers: map[string]string{
			"x-user-id":      userID.String(),
			"x-tenant-id":    tenantID.String(),
			"x-tenant-roles": "definitely_not_a_real_role,another_fake_one",
		},
		body: map[string]any{"feature_flags": map[string]any{}},
	})
	assert.Equal(t, http.StatusForbidden, code,
		"P14-AUTH-004: bogus roles must not open operator lane (got %d body=%s)", code, string(body))
}

// ── P14-TAMPER-001 ──────────────────────────────────────────────────────────

// TestFabricatedRecordVersionOnPatch — attacker sends a huge
// record_version on PATCH tenant. Optimistic-lock rejects with 409.
func TestFabricatedRecordVersionOnPatch(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "tamper-001")
	userID := e.seedOwner(t, tenantID)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPatch,
		path:    "/api/v1/tenants/" + tenantID.String(),
		headers: ownerHeaders(userID, tenantID),
		body: map[string]any{
			"name":           "New Name",
			"record_version": int64(9999999),
		},
	})
	assert.Equal(t, http.StatusConflict, code,
		"P14-TAMPER-001: fabricated record_version must trigger optimistic-lock 409 (got %d body=%s)", code, string(body))
	assert.Contains(t, string(body), "optimistic_lock_conflict",
		"P14-TAMPER-001: error code must indicate optimistic-lock conflict")
}

// ── P14-TAMPER-002 ──────────────────────────────────────────────────────────

// TestInviteWithOwnerRoleFromNonOwner — a tender_admin (who
// can invite) tries to include `tenant_owner` in initial_tenant_roles.
// Handler must reject the elevation attempt.
func TestInviteWithOwnerRoleFromNonOwner(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "tamper-002")
	inviter := e.seedActiveMember(t, tenantID)

	// Grant tender_admin (may invite, cannot grant tenant_owner via §10.4).
	_, err := e.rawPool.Exec(e.ctx, `
		INSERT INTO tenant_roles (id, tenant_id, tenant_membership_id, user_id, role_code, granted_by)
		SELECT $1, $2, tm.id, $3, 'tender_admin', $3
		FROM tenant_memberships tm
		WHERE tm.tenant_id = $2 AND tm.user_id = $3`,
		uuid.New(), tenantID, inviter)
	require.NoError(t, err)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPost,
		path:    "/api/v1/tenants/" + tenantID.String() + "/members",
		headers: gatewayHeaders(inviter, tenantID, "tender_admin"),
		body: map[string]any{
			"email":                "tamper002@example.com",
			"full_name":            "Tamper Test",
			"initial_tenant_roles": []string{"tenant_owner"},
		},
	})
	// Must NOT succeed silently — either reject (403/422) or drop the
	// elevated role but never grant tenant_owner to the invitee.
	assert.NotEqual(t, http.StatusAccepted, code,
		"P14-TAMPER-002: non-owner must not be able to invite with tenant_owner role (got %d body=%s)", code, string(body))
}

// ── P14-TAMPER-003 ──────────────────────────────────────────────────────────

// TestBodyActorIgnored — a body-supplied actor_id must be
// ignored; the audit trail uses the gateway-supplied identity.
func TestBodyActorIgnored(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "tamper-003")
	realOwner := e.seedOwner(t, tenantID)
	target := e.seedActiveMember(t, tenantID)
	fakeActor := uuid.New()

	code, _, _ := e.do(t, reqOpts{
		method:  http.MethodPut,
		path:    "/api/v1/tenants/" + tenantID.String() + "/members/" + target.String() + "/roles",
		headers: ownerHeaders(realOwner, tenantID),
		body: map[string]any{
			"roles":    []string{"tender_admin"},
			"actor_id": fakeActor.String(), // stray field — must be ignored
		},
	})
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, code)

	// Verify the audit trail (granted_by column) recorded the REAL owner,
	// not the fake actor.
	var grantedBy uuid.UUID
	require.NoError(t, e.rawPool.QueryRow(e.ctx,
		`SELECT granted_by FROM tenant_roles WHERE tenant_id = $1 AND user_id = $2 AND role_code = 'tender_admin'`,
		tenantID, target).Scan(&grantedBy))
	assert.Equal(t, realOwner, grantedBy,
		"P14-TAMPER-003: granted_by must be the gateway identity, not the body-supplied actor")
	assert.NotEqual(t, fakeActor, grantedBy,
		"P14-TAMPER-003: fake actor from body must NOT appear as granter")
}

// ── P14-PRIV-001 ────────────────────────────────────────────────────────────

// TestTenderAdminCannotGrantOwnerViaReconcile — a tender_admin
// tries to grant a peer `tenant_owner` via P-28. Must be rejected.
func TestTenderAdminCannotGrantOwnerViaReconcile(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "priv-001")
	e.seedOwner(t, tenantID) // seed at least one owner so TM-13 doesn't reject
	elevator := e.seedActiveMember(t, tenantID)
	target := e.seedActiveMember(t, tenantID)

	_, err := e.rawPool.Exec(e.ctx, `
		INSERT INTO tenant_roles (id, tenant_id, tenant_membership_id, user_id, role_code, granted_by)
		SELECT $1, $2, tm.id, $3, 'tender_admin', $3
		FROM tenant_memberships tm WHERE tm.tenant_id = $2 AND tm.user_id = $3`,
		uuid.New(), tenantID, elevator)
	require.NoError(t, err)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPut,
		path:    "/api/v1/tenants/" + tenantID.String() + "/members/" + target.String() + "/roles",
		headers: gatewayHeaders(elevator, tenantID, "tender_admin"),
		body:    map[string]any{"roles": []string{"tenant_owner"}},
	})
	assert.Equal(t, http.StatusForbidden, code,
		"P14-PRIV-001: tender_admin must not grant tenant_owner via reconcile (got %d body=%s)", code, string(body))
}

// ── P14-PRIV-002 ────────────────────────────────────────────────────────────

// TestMemberCannotSelfElevate — a regular member (no elevated
// roles) tries to grant themselves tender_admin via P-28 → 403.
func TestMemberCannotSelfElevate(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "priv-002")
	e.seedOwner(t, tenantID)
	member := e.seedActiveMember(t, tenantID)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPut,
		path:    "/api/v1/tenants/" + tenantID.String() + "/members/" + member.String() + "/roles",
		headers: gatewayHeaders(member, tenantID, ""), // no elevated role
		body:    map[string]any{"roles": []string{"tender_admin"}},
	})
	assert.Equal(t, http.StatusForbidden, code,
		"P14-PRIV-002: unprivileged member must not self-elevate (got %d body=%s)", code, string(body))
}

// ── P14-CT-001 ──────────────────────────────────────────────────────────────

// TestCrossTenantInviteRejected — owner of tenant A tries to
// POST an invite into tenant B.
func TestCrossTenantInviteRejected(t *testing.T) {
	e := newE2EEnv(t)
	tenantA := e.seedTenant(t, "ct-001-a")
	tenantB := e.seedTenant(t, "ct-001-b")
	userA := e.seedOwner(t, tenantA)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPost,
		path:    "/api/v1/tenants/" + tenantB.String() + "/members",
		headers: ownerHeaders(userA, tenantA),
		body: map[string]any{
			"email":     "ct001@example.com",
			"full_name": "Cross Tenant",
		},
	})
	assert.Equal(t, http.StatusForbidden, code,
		"P14-CT-001: cross-tenant invite must 403 (got %d body=%s)", code, string(body))
}

// ── P14-CT-002 ──────────────────────────────────────────────────────────────

func TestCrossTenantListMembersRejected(t *testing.T) {
	e := newE2EEnv(t)
	tenantA := e.seedTenant(t, "ct-002-a")
	tenantB := e.seedTenant(t, "ct-002-b")
	userA := e.seedOwner(t, tenantA)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodGet,
		path:    "/api/v1/tenants/" + tenantB.String() + "/members",
		headers: ownerHeaders(userA, tenantA),
	})
	assert.Equal(t, http.StatusForbidden, code,
		"P14-CT-002: cross-tenant list members must 403 (got %d body=%s)", code, string(body))
}

// ── P14-CT-003 ──────────────────────────────────────────────────────────────

func TestCrossTenantDeptAssignRejected(t *testing.T) {
	e := newE2EEnv(t)
	tenantA := e.seedTenant(t, "ct-003-a")
	tenantB := e.seedTenant(t, "ct-003-b")
	userA := e.seedOwner(t, tenantA)

	// Body doesn't matter — the tenant guard fires before validation.
	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPut,
		path:    fmt.Sprintf("/api/v1/tenants/%s/departments/%s/members/%s", tenantB, uuid.New(), uuid.New()),
		headers: ownerHeaders(userA, tenantA),
		body:    map[string]any{"level": "preparator"},
	})
	assert.Equal(t, http.StatusForbidden, code,
		"P14-CT-003: cross-tenant dept-membership assign must 403 (got %d body=%s)", code, string(body))
}

// ── P14-CT-004 ──────────────────────────────────────────────────────────────

func TestCrossTenantRoleReconcileRejected(t *testing.T) {
	e := newE2EEnv(t)
	tenantA := e.seedTenant(t, "ct-004-a")
	tenantB := e.seedTenant(t, "ct-004-b")
	userA := e.seedOwner(t, tenantA)
	targetInB := e.seedActiveMember(t, tenantB)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPut,
		path:    "/api/v1/tenants/" + tenantB.String() + "/members/" + targetInB.String() + "/roles",
		headers: ownerHeaders(userA, tenantA),
		body:    map[string]any{"roles": []string{"tender_admin"}},
	})
	assert.Equal(t, http.StatusForbidden, code,
		"P14-CT-004: cross-tenant role reconcile must 403 (got %d body=%s)", code, string(body))
}

// ── P14-CT-005 ──────────────────────────────────────────────────────────────

// TestRLSIsolatesConcurrentTenantContext — the RLS-enforcing app
// pool must return ZERO rows for tenant B when the request context carries
// tenant A's GUC. Bypasses the HTTP layer entirely — directly proves the
// bottom-of-stack isolation.
func TestRLSIsolatesConcurrentTenantContext(t *testing.T) {
	e := newE2EEnv(t)
	tenantA := e.seedTenant(t, "ct-005-a")
	tenantB := e.seedTenant(t, "ct-005-b")
	_ = e.seedOwner(t, tenantB) // membership row in B

	// Sanity-seed a row in A so we can prove BOTH directions.
	_, err := e.rawPool.Exec(e.ctx,
		`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES ($1, $2, $3, 'active')`,
		uuid.New(), tenantA, uuid.New())
	require.NoError(t, err)

	// Query the RLS-enforcing pool under a context bound to A. tenant_memberships
	// for B must be invisible even though the row physically exists.
	ctx := withGUCTenant(e.ctx, tenantA)
	var countB, countA int
	require.NoError(t, e.appPool.WithConn(ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM tenant_memberships WHERE tenant_id = $1`, tenantB).Scan(&countB); err != nil {
			return err
		}
		return conn.QueryRow(ctx,
			`SELECT count(*) FROM tenant_memberships WHERE tenant_id = $1`, tenantA).Scan(&countA)
	}))
	assert.Equal(t, 0, countB,
		"P14-CT-005: RLS must hide tenant B rows from a session bound to tenant A")
	assert.GreaterOrEqual(t, countA, 1, "P14-CT-005: A's own row is visible under A's GUC")
}

// ── P14-REPLAY-001 ──────────────────────────────────────────────────────────

// TestDoubleInviteSameEmail — posting the same invite email
// twice must yield a business-level 409 on the second call (§8.10, PI-1
// pending-invitations unique-per-tenant partial index).
func TestDoubleInviteSameEmail(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "replay-001")
	userID := e.seedOwner(t, tenantID)

	body := map[string]any{"email": "replay001@example.com", "full_name": "Replay One"}

	code1, _, _ := e.do(t, reqOpts{
		method:  http.MethodPost,
		path:    "/api/v1/tenants/" + tenantID.String() + "/members",
		headers: ownerHeaders(userID, tenantID),
		body:    body,
	})
	require.Equal(t, http.StatusAccepted, code1, "first invite must succeed")

	code2, _, respBody := e.do(t, reqOpts{
		method:  http.MethodPost,
		path:    "/api/v1/tenants/" + tenantID.String() + "/members",
		headers: ownerHeaders(userID, tenantID),
		body:    body,
	})
	assert.Equal(t, http.StatusConflict, code2,
		"P14-REPLAY-001: duplicate invite must 409 (got %d body=%s)", code2, string(respBody))
	assert.Contains(t, string(respBody), "invitation",
		"P14-REPLAY-001: error must reference the invitation-conflict domain")
}

// ── P14-REPLAY-002 ──────────────────────────────────────────────────────────

// TestDoubleDelegationCancelWithSameVersion — cancelling
// a delegation twice with the same record_version must succeed once and
// fail the second time as an optimistic-lock or not-found conflict.
func TestDoubleDelegationCancelWithSameVersion(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "replay-002")
	delegator := e.seedOwner(t, tenantID)
	delegate := e.seedActiveMember(t, tenantID)

	// Create a delegation.
	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPost,
		path:    "/api/v1/delegations",
		headers: ownerHeaders(delegator, tenantID),
		body: map[string]any{
			"delegate_id": delegate.String(),
			"scope":       "all",
		},
	})
	require.Equal(t, http.StatusCreated, code)
	var created map[string]any
	unmarshalBody(t, body, &created)
	delegationID, _ := created["id"].(string)
	ver, _ := created["record_version"].(float64)

	// First cancel — should succeed.
	code1, _, _ := e.do(t, reqOpts{
		method:  http.MethodDelete,
		path:    fmt.Sprintf("/api/v1/delegations/%s?record_version=%d", delegationID, int64(ver)),
		headers: ownerHeaders(delegator, tenantID),
	})
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, code1, "first cancel must succeed")

	// Second cancel with the SAME (now-stale) version — must fail.
	code2, _, respBody := e.do(t, reqOpts{
		method:  http.MethodDelete,
		path:    fmt.Sprintf("/api/v1/delegations/%s?record_version=%d", delegationID, int64(ver)),
		headers: ownerHeaders(delegator, tenantID),
	})
	assert.Contains(t, []int{http.StatusConflict, http.StatusNotFound, http.StatusUnprocessableEntity}, code2,
		"P14-REPLAY-002: double-cancel must fail 4xx (got %d body=%s)", code2, string(respBody))
}

// ── helpers ─────────────────────────────────────────────────────────────────

// withGUCTenant binds the given tenant id to the pgcommon GUC-provider bag
// so the pool emits `SET LOCAL app.tenant_id = <uuid>` on every checkout
// (RLS-6). Mirrors test/postgres/rls_test.go withTenant.
func withGUCTenant(ctx context.Context, tenantID uuid.UUID) context.Context {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.TenantID = tenantID.String()
	return pgcommon.WithGUCSet(ctx, g)
}

// Silence unused-var warning if a helper is trimmed later.
var _ = strings.Contains
