//go:build e2e

// ADR-0007 Wave 3 Phase 6 (cleanup) — P-21/P-22/P-23/I-12 have been fully
// removed from this service (not merely retired to 410 Gone as in Phase 5);
// the routes are unregistered and a request to them now falls through to
// gin's plain 404. Moved to iam-tender-acl's TAC-1/2/3/4. See
// iam-tender-acl/MIGRATION_RUNBOOK.md.
//
// ADR-0008 v2 — P-18/P-19/P-20/P-32/P-33 (list/create/cancel/extend/reassign
// delegation) are likewise fully removed, not retired to 410 Gone. Moved to
// the standalone Delegation Service's DLG-1..5.
package e2e_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRetiredTenderACL_List_Returns404(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "retired-001")
	userID := e.seedOwner(t, tenantID)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodGet,
		path:    "/api/v1/tenants/" + tenantID.String() + "/tenders/" + uuid.NewString() + "/acl",
		headers: ownerHeaders(userID, tenantID),
	})
	require.Equal(t, http.StatusNotFound, code,
		"P-21 (list) route must be fully unregistered post-Phase-6 cleanup (got %d body=%s)", code, string(body))
}

func TestRetiredTenderACL_Grant_Returns404(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "retired-002")
	userID := e.seedOwner(t, tenantID)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPost,
		path:    "/api/v1/tenants/" + tenantID.String() + "/tenders/" + uuid.NewString() + "/acl",
		headers: ownerHeaders(userID, tenantID),
		body: map[string]any{
			"user_id":      userID.String(),
			"access_level": "view",
		},
	})
	require.Equal(t, http.StatusNotFound, code,
		"P-22 (grant) route must be fully unregistered post-Phase-6 cleanup (got %d body=%s)", code, string(body))
}

func TestRetiredTenderACL_Revoke_Returns404(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "retired-003")
	userID := e.seedOwner(t, tenantID)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodDelete,
		path:    "/api/v1/tenants/" + tenantID.String() + "/tenders/" + uuid.NewString() + "/acl/" + userID.String(),
		headers: ownerHeaders(userID, tenantID),
	})
	require.Equal(t, http.StatusNotFound, code,
		"P-23 (revoke) route must be fully unregistered post-Phase-6 cleanup (got %d body=%s)", code, string(body))
}

func TestRetiredTenderACL_CheckAccess_Returns404(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "retired-004")
	userID := e.seedOwner(t, tenantID)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodGet,
		path:    "/api/v1/internal/tenants/" + tenantID.String() + "/tenders/" + uuid.NewString() + "/acl/" + userID.String(),
		headers: systemHeaders(tenantID),
	})
	require.Equal(t, http.StatusNotFound, code,
		"I-12 (check access) route must be fully unregistered post-Phase-6 cleanup (got %d body=%s)", code, string(body))
}

func TestRetiredDelegation_List_Returns404(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "retired-007")
	userID := e.seedOwner(t, tenantID)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodGet,
		path:    "/api/v1/delegations",
		headers: ownerHeaders(userID, tenantID),
	})
	require.Equal(t, http.StatusNotFound, code,
		"P-18 (list) route must be fully unregistered post-ADR-0008 cleanup (got %d body=%s)", code, string(body))
}

func TestRetiredDelegation_Create_Returns404(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "retired-008")
	delegator := e.seedOwner(t, tenantID)
	delegate := e.seedActiveMember(t, tenantID)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPost,
		path:    "/api/v1/delegations",
		headers: ownerHeaders(delegator, tenantID),
		body: map[string]any{
			"delegate_id": delegate.String(),
			"scope":       "all",
		},
	})
	require.Equal(t, http.StatusNotFound, code,
		"P-19 (create) route must be fully unregistered post-ADR-0008 cleanup (got %d body=%s)", code, string(body))
}

func TestRetiredDelegation_Cancel_Returns404(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "retired-009")
	userID := e.seedOwner(t, tenantID)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodDelete,
		path:    "/api/v1/delegations/" + uuid.NewString(),
		headers: ownerHeaders(userID, tenantID),
	})
	require.Equal(t, http.StatusNotFound, code,
		"P-20 (cancel) route must be fully unregistered post-ADR-0008 cleanup (got %d body=%s)", code, string(body))
}

func TestRetiredDelegation_Extend_Returns404(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "retired-010")
	userID := e.seedOwner(t, tenantID)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPost,
		path:    "/api/v1/delegations/" + uuid.NewString() + "/extend",
		headers: ownerHeaders(userID, tenantID),
		body:    map[string]any{"extend_days": 7},
	})
	require.Equal(t, http.StatusNotFound, code,
		"P-32 (extend) route must be fully unregistered post-ADR-0008 cleanup (got %d body=%s)", code, string(body))
}

func TestRetiredDelegation_Reassign_Returns404(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "retired-011")
	delegator := e.seedOwner(t, tenantID)
	newDelegate := e.seedActiveMember(t, tenantID)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodPost,
		path:    "/api/v1/delegations/" + uuid.NewString() + "/reassign",
		headers: ownerHeaders(delegator, tenantID),
		body:    map[string]any{"delegate_id": newDelegate.String()},
	})
	require.Equal(t, http.StatusNotFound, code,
		"P-33 (reassign) route must be fully unregistered post-ADR-0008 cleanup (got %d body=%s)", code, string(body))
}

// TestCheckMemberExists_ActiveMember_ADR0007Wave3Phase3 confirms the new
// provider endpoint (added Phase 3, missing from this harness until Phase
// 5's cleanup pass added it) actually works end-to-end via the real
// router — the existing test/unit coverage exercises the handler/service
// directly, not through HTTP.
func TestCheckMemberExists_ActiveMember_ADR0007Wave3Phase3(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "retired-005")
	userID := e.seedOwner(t, tenantID)

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodGet,
		path:    "/api/v1/internal/tenants/" + tenantID.String() + "/members/" + userID.String() + "/exists",
		headers: systemHeaders(tenantID),
	})
	require.Equal(t, http.StatusOK, code,
		"CheckMemberExists must return 200 for an active member (got %d body=%s)", code, string(body))

	var resp map[string]any
	unmarshalBody(t, body, &resp)
	require.Equal(t, true, resp["active"], "seeded owner must be active")
	require.NotEmpty(t, resp["tenant_membership_id"], "active member response must include tenant_membership_id")
}

func TestCheckMemberExists_UnknownUser_ReturnsInactive(t *testing.T) {
	e := newE2EEnv(t)
	tenantID := e.seedTenant(t, "retired-006")

	code, _, body := e.do(t, reqOpts{
		method:  http.MethodGet,
		path:    "/api/v1/internal/tenants/" + tenantID.String() + "/members/" + uuid.NewString() + "/exists",
		headers: systemHeaders(tenantID),
	})
	require.Equal(t, http.StatusOK, code,
		"CheckMemberExists must never 404 — absence is a valid active:false answer (got %d body=%s)", code, string(body))

	var resp map[string]any
	unmarshalBody(t, body, &resp)
	require.Equal(t, false, resp["active"], "unknown user must be inactive")
}
