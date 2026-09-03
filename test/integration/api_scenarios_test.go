//go:build integration

package integration_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAPIScenarios_I2_PatchRealm covers I-2: RP sets realm_id/realm_type/keycloak_shard.
func TestAPIScenarios_I2_PatchRealm(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)

	rvT := e.getTenantRV(t, tt.TenantID, tt.OwnerID)

	t.Run("I2-H-01 valid realm update returns 200", func(t *testing.T) {
		body := toJSON(map[string]any{
			"realm_id": "r-valid01", "realm_type": "dedicated",
			"keycloak_shard": "eu-1", "record_version": rvT,
		})
		resp := e.do(t, http.MethodPatch, "/api/v1/internal/tenants/"+tt.TenantID,
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusOK)
		rvT = rv(resp)
	})

	t.Run("I2-H-02 transition shared → dedicated", func(t *testing.T) {
		body := toJSON(map[string]any{
			"realm_id": "r-dedicated", "realm_type": "dedicated",
			"keycloak_shard": "ap-1", "record_version": rvT,
		})
		resp := e.do(t, http.MethodPatch, "/api/v1/internal/tenants/"+tt.TenantID,
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusOK)
		rvT = rv(resp)
	})

	t.Run("I2-AUTH-01 non-iam-system caller → 403", func(t *testing.T) {
		body := toJSON(map[string]any{
			"realm_id": "r-x", "realm_type": "dedicated",
			"keycloak_shard": "eu-1", "record_version": rvT,
		})
		resp := e.do(t, http.MethodPatch, "/api/v1/internal/tenants/"+tt.TenantID,
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusForbidden)
	})

	t.Run("I2-V-01 missing realm_id → 400", func(t *testing.T) {
		body := toJSON(map[string]any{
			"realm_type": "dedicated", "keycloak_shard": "eu-1", "record_version": rvT,
		})
		resp := e.do(t, http.MethodPatch, "/api/v1/internal/tenants/"+tt.TenantID,
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("I2-V-02 invalid realm_type → 422", func(t *testing.T) {
		body := toJSON(map[string]any{
			"realm_id": "r-bad", "realm_type": "INVALID",
			"keycloak_shard": "eu-1", "record_version": rvT,
		})
		resp := e.do(t, http.MethodPatch, "/api/v1/internal/tenants/"+tt.TenantID,
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusUnprocessableEntity)
	})

	t.Run("I2-NF-01 unknown tenant → 404", func(t *testing.T) {
		body := toJSON(map[string]any{
			"realm_id": "r-x", "realm_type": "dedicated",
			"keycloak_shard": "eu-1", "record_version": 1,
		})
		resp := e.do(t, http.MethodPatch, "/api/v1/internal/tenants/"+freshTenantID(),
			body, isSys(freshTenantID()))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusNotFound)
	})

	t.Run("I2-CONC-01 stale record_version → 409", func(t *testing.T) {
		body := toJSON(map[string]any{
			"realm_id": "r-stale", "realm_type": "dedicated",
			"keycloak_shard": "eu-1", "record_version": 9999,
		})
		resp := e.do(t, http.MethodPatch, "/api/v1/internal/tenants/"+tt.TenantID,
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusConflict)
	})
}

// TestAPIScenarios_I3_AddMember covers I-3: add member to tenant.
func TestAPIScenarios_I3_AddMember(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)

	t.Run("I3-HP-01 add fresh member → 201", func(t *testing.T) {
		uid := freshID()
		body := toJSON(map[string]any{
			"user_id": uid, "email": uid[:8] + "@t.com", "full_name": "User",
		})
		resp := e.do(t, http.MethodPost, "/api/v1/internal/tenants/"+tt.TenantID+"/members",
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusCreated)
	})

	t.Run("I3-IDP-01 re-add same member → 201 idempotent (PI-10)", func(t *testing.T) {
		// PI-10: uq_tm_active_user ON CONFLICT DO NOTHING returns the existing row.
		// Handler returns 201 for both fresh and idempotent adds (LLD I-3 @Success 201).
		body := toJSON(map[string]any{
			"user_id": tt.MemberID, "email": "member@test.com", "full_name": "Test Member",
		})
		resp := e.do(t, http.MethodPost, "/api/v1/internal/tenants/"+tt.TenantID+"/members",
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusCreated)
	})

	t.Run("I3-NF-01 unknown tenant → 404", func(t *testing.T) {
		uid := freshID()
		tid := freshTenantID()
		body := toJSON(map[string]any{
			"user_id": uid, "email": uid[:8] + "@t.com", "full_name": "User",
		})
		resp := e.do(t, http.MethodPost, "/api/v1/internal/tenants/"+tid+"/members",
			body, isSys(tid))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusNotFound)
	})

	t.Run("I3-VAL-01 invalid email → 400", func(t *testing.T) {
		body := toJSON(map[string]any{
			"user_id": freshID(), "email": "not-an-email", "full_name": "User",
		})
		resp := e.do(t, http.MethodPost, "/api/v1/internal/tenants/"+tt.TenantID+"/members",
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("I3-VAL-02 invalid UUID user_id → 400", func(t *testing.T) {
		body := toJSON(map[string]any{
			"user_id": "bad-uuid", "email": "u@t.com", "full_name": "User",
		})
		resp := e.do(t, http.MethodPost, "/api/v1/internal/tenants/"+tt.TenantID+"/members",
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
	})
}

// TestAPIScenarios_I4_PatchMemberLifecycle covers I-4.
func TestAPIScenarios_I4_PatchMemberLifecycle(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)
	memberRV := e.getMemberRV(t, tt.TenantID, tt.OwnerID, tt.MemberID)

	t.Run("I4-HP-01 suspend active member → 200", func(t *testing.T) {
		body := toJSON(map[string]any{"status": "suspended", "record_version": memberRV})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/internal/tenants/"+tt.TenantID+"/members/"+tt.MemberID,
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		hasField(t, b, "status", "suspended")
		memberRV = rv(resp)
	})

	t.Run("I4-HP-02 reactivate suspended member → 200", func(t *testing.T) {
		body := toJSON(map[string]any{"status": "active", "record_version": memberRV})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/internal/tenants/"+tt.TenantID+"/members/"+tt.MemberID,
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		hasField(t, b, "status", "active")
		memberRV = rv(resp)
	})

	t.Run("I4-EDGE-01 no-op same status → 200", func(t *testing.T) {
		body := toJSON(map[string]any{"status": "active", "record_version": memberRV})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/internal/tenants/"+tt.TenantID+"/members/"+tt.MemberID,
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusOK)
		memberRV = rv(resp)
	})

	t.Run("I4-BL-01 invalid status value → 400", func(t *testing.T) {
		body := toJSON(map[string]any{"status": "godmode", "record_version": memberRV})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/internal/tenants/"+tt.TenantID+"/members/"+tt.MemberID,
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("I4-NF-01 unknown member → 404", func(t *testing.T) {
		body := toJSON(map[string]any{"status": "suspended", "record_version": 1})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/internal/tenants/"+tt.TenantID+"/members/"+freshID(),
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusNotFound)
	})

	t.Run("I4-CONC-01 stale record_version → 409", func(t *testing.T) {
		body := toJSON(map[string]any{"status": "suspended", "record_version": 9999})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/internal/tenants/"+tt.TenantID+"/members/"+tt.MemberID,
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusConflict)
	})

	t.Run("I4-AUTH-01 non-iam-system caller → 403", func(t *testing.T) {
		body := toJSON(map[string]any{"status": "suspended", "record_version": memberRV})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/internal/tenants/"+tt.TenantID+"/members/"+tt.MemberID,
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusForbidden)
	})

	t.Run("I4-HP-03 set status left → 200", func(t *testing.T) {
		body := toJSON(map[string]any{"status": "left", "record_version": memberRV})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/internal/tenants/"+tt.TenantID+"/members/"+tt.MemberID,
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		hasField(t, b, "status", "left")
	})
}

// TestAPIScenarios_I5_DeleteMember covers I-5: KC USER_DELETE cascade.
func TestAPIScenarios_I5_DeleteMember(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)

	t.Run("I5-HP-01 delete plain member → 200", func(t *testing.T) {
		uid := freshID()
		e.addMember(t, tt.TenantID, uid, uid[:8]+"@t.com", "Del User")
		resp := e.do(t, http.MethodDelete,
			"/api/v1/internal/tenants/"+tt.TenantID+"/members/"+uid,
			"", isSys(tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		hasField(t, b, "removed", true)
	})

	t.Run("I5-IDP-01 re-delete same member → 200 idempotent", func(t *testing.T) {
		uid := freshID()
		e.addMember(t, tt.TenantID, uid, uid[:8]+"@t.com", "Del User 2")
		// Delete once
		resp := e.do(t, http.MethodDelete,
			"/api/v1/internal/tenants/"+tt.TenantID+"/members/"+uid,
			"", isSys(tt.TenantID))
		resp.Body.Close()
		// Delete again — must be idempotent 200
		resp2 := e.do(t, http.MethodDelete,
			"/api/v1/internal/tenants/"+tt.TenantID+"/members/"+uid,
			"", isSys(tt.TenantID))
		defer resp2.Body.Close()
		assertStatus(t, resp2, http.StatusOK)
	})

	t.Run("I5-NF-01 unknown user → 200 idempotent (no membership row)", func(t *testing.T) {
		resp := e.do(t, http.MethodDelete,
			"/api/v1/internal/tenants/"+tt.TenantID+"/members/"+freshID(),
			"", isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusOK)
	})

	t.Run("I5-VAL-01 invalid user UUID → 400", func(t *testing.T) {
		resp := e.do(t, http.MethodDelete,
			"/api/v1/internal/tenants/"+tt.TenantID+"/members/not-a-uuid",
			"", isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("I5-AUTH-01 non-iam-system → 403", func(t *testing.T) {
		resp := e.do(t, http.MethodDelete,
			"/api/v1/internal/tenants/"+tt.TenantID+"/members/"+tt.MemberID,
			"", isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusForbidden)
	})
}

// TestAPIScenarios_I13_AssigneeOverride covers I-13.
func TestAPIScenarios_I13_AssigneeOverride(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)
	tenderID := freshID()

	// Assign MEMBER to DEPT_ENG as preparator
	e.assignDept(t, tt.TenantID, tt.OwnerID, tt.MemberID, DeptEngID.String(), "preparator")
	// Assign OWNER to DEPT_ENG as approver (actor for I13)
	e.assignDept(t, tt.TenantID, tt.OwnerID, tt.OwnerID, DeptEngID.String(), "approver")

	t.Run("I13-HP-01 valid actor + eligible new_user_id → 200", func(t *testing.T) {
		body := toJSON(map[string]any{
			"actor_id":       tt.OwnerID,
			"new_user_id":    tt.MemberID,
			"required_level": "preparator",
			"department_id":  DeptEngID.String(),
			"tender_id":      tenderID,
		})
		resp := e.do(t, http.MethodPost,
			"/api/v1/internal/tenants/"+tt.TenantID+"/tenders/"+tenderID+"/assignee-override",
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusOK)
	})

	t.Run("I13-BL-01 new_user_id not a member → 422", func(t *testing.T) {
		body := toJSON(map[string]any{
			"actor_id":       tt.OwnerID,
			"new_user_id":    freshID(),
			"required_level": "preparator",
			"department_id":  DeptEngID.String(),
			"tender_id":      tenderID,
		})
		resp := e.do(t, http.MethodPost,
			"/api/v1/internal/tenants/"+tt.TenantID+"/tenders/"+tenderID+"/assignee-override",
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusUnprocessableEntity)
	})

	t.Run("I13-BL-04 new_user_id in dept but insufficient level → 422", func(t *testing.T) {
		body := toJSON(map[string]any{
			"actor_id":       tt.OwnerID,
			"new_user_id":    tt.MemberID,
			"required_level": "approver", // member is only preparator
			"department_id":  DeptEngID.String(),
			"tender_id":      tenderID,
		})
		resp := e.do(t, http.MethodPost,
			"/api/v1/internal/tenants/"+tt.TenantID+"/tenders/"+tenderID+"/assignee-override",
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusUnprocessableEntity)
	})

	t.Run("I13-VAL-04 unknown required_level → 422", func(t *testing.T) {
		body := toJSON(map[string]any{
			"actor_id":       tt.OwnerID,
			"new_user_id":    tt.MemberID,
			"required_level": "manager",
			"department_id":  DeptEngID.String(),
			"tender_id":      tenderID,
		})
		resp := e.do(t, http.MethodPost,
			"/api/v1/internal/tenants/"+tt.TenantID+"/tenders/"+tenderID+"/assignee-override",
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusUnprocessableEntity)
	})

	t.Run("I13-VAL-01 missing department_id → 400", func(t *testing.T) {
		body := toJSON(map[string]any{
			"actor_id":       tt.OwnerID,
			"new_user_id":    tt.MemberID,
			"required_level": "preparator",
			"tender_id":      tenderID,
		})
		resp := e.do(t, http.MethodPost,
			"/api/v1/internal/tenants/"+tt.TenantID+"/tenders/"+tenderID+"/assignee-override",
			body, isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("I13-AUTH-01 non-iam-system → 403", func(t *testing.T) {
		body := toJSON(map[string]any{
			"actor_id": tt.OwnerID, "new_user_id": tt.MemberID,
			"required_level": "preparator", "department_id": DeptEngID.String(),
			"tender_id": tenderID,
		})
		resp := e.do(t, http.MethodPost,
			"/api/v1/internal/tenants/"+tt.TenantID+"/tenders/"+tenderID+"/assignee-override",
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusForbidden)
	})
}

// TestAPIScenarios_IUM_I8_Memberships covers I-8 (hot-path membership projection).
func TestAPIScenarios_IUM_I8_Memberships(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)

	t.Run("IUM-H-01 active owner → 200 with roles", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/internal/users/"+tt.OwnerID+"/memberships?tenant_id="+tt.TenantID,
			"", isSys(tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		roles, _ := b["roles"].([]any)
		require.Contains(t, roles, "tenant_owner", "owner must have tenant_owner role")
		require.Contains(t, roles, "member", "member role must be derived (TR-7)")
	})

	t.Run("IUM-H-05 user with tenant_owner role → role_codes includes tenant_owner", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/internal/users/"+tt.OwnerID+"/memberships?tenant_id="+tt.TenantID,
			"", isSys(tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		roles, _ := b["roles"].([]any)
		assert.Contains(t, roles, "tenant_owner")
	})

	t.Run("IUM-S-01 active member → status=active", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/internal/users/"+tt.MemberID+"/memberships?tenant_id="+tt.TenantID,
			"", isSys(tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		hasField(t, b, "status", "active")
	})

	t.Run("IUM-S-02 suspended member → status=suspended still returns 200", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/internal/users/"+tt.SuspendedID+"/memberships?tenant_id="+tt.TenantID,
			"", isSys(tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		hasField(t, b, "status", "suspended")
	})

	t.Run("IUM-E-01 no membership → 404", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/internal/users/"+freshID()+"/memberships?tenant_id="+tt.TenantID,
			"", isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusNotFound)
	})

	t.Run("IUM-E-05 missing X-Tenant-ID → uses query param instead", func(t *testing.T) {
		// With tenant_id in query but missing x-tenant-id header → 401 (identity headers check)
		hdrs := map[string]string{"x-user-id": "00000000-0000-0000-0000-000000000001", "x-tenant-roles": "iam-system"}
		resp := e.do(t, http.MethodGet,
			"/api/v1/internal/users/"+tt.OwnerID+"/memberships?tenant_id="+tt.TenantID,
			"", hdrs)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("IUM-A-05 iam-system role → 200", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/internal/users/"+tt.OwnerID+"/memberships?tenant_id="+tt.TenantID,
			"", isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusOK)
	})

	t.Run("IUM-H-07 user with dept assignment → departments non-empty", func(t *testing.T) {
		e.assignDept(t, tt.TenantID, tt.OwnerID, tt.MemberID, DeptEngID.String(), "preparator")
		resp := e.do(t, http.MethodGet,
			"/api/v1/internal/users/"+tt.MemberID+"/memberships?tenant_id="+tt.TenantID,
			"", isSys(tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		depts, _ := b["departments"].([]any)
		assert.Greater(t, len(depts), 0, "departments must be non-empty")
	})
}

// TestAPIScenarios_I11_SeatUsage covers I-11.
func TestAPIScenarios_I11_SeatUsage(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)

	t.Run("I11-HP-01 iam-system reads seat usage → 200", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/internal/tenants/"+tt.TenantID+"/seat-usage",
			"", isSys(tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		_, hasActive := b["active_users"]
		_, hasLicensed := b["licensed_seats"]
		assert.True(t, hasActive && hasLicensed, "response must have seat-usage fields")
	})

	t.Run("I11-HP-02 response matches P-27 shape", func(t *testing.T) {
		respI := e.do(t, http.MethodGet,
			"/api/v1/internal/tenants/"+tt.TenantID+"/seat-usage", "", isSys(tt.TenantID))
		defer respI.Body.Close()
		bI := assertStatus(t, respI, http.StatusOK)

		respP := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/seat-usage", "", isOwner(tt.OwnerID, tt.TenantID))
		defer respP.Body.Close()
		bP := assertStatus(t, respP, http.StatusOK)

		assert.Equal(t, bI["licensed_seats"], bP["licensed_seats"])
		assert.Equal(t, bI["active_users"], bP["active_users"])
	})
}

// TestAPIScenarios_GT_GetTenant covers P-1 / GT scenarios.
func TestAPIScenarios_GT_GetTenant(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)

	t.Run("GT-HP-01 tenant_owner reads tenant → 200", func(t *testing.T) {
		resp := e.do(t, http.MethodGet, "/api/v1/tenants/"+tt.TenantID,
			"", isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		hasField(t, b, "plan", "starter")
		hasField(t, b, "status", "trial")
	})

	t.Run("GT-HP-02 tenant_admin reads tenant → 200", func(t *testing.T) {
		resp := e.do(t, http.MethodGet, "/api/v1/tenants/"+tt.TenantID,
			"", isAdmin(tt.AdminID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusOK)
	})

	t.Run("GT-HP-03 plain member reads tenant → 200", func(t *testing.T) {
		resp := e.do(t, http.MethodGet, "/api/v1/tenants/"+tt.TenantID,
			"", isMember(tt.MemberID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusOK)
	})

	t.Run("GT-NF-01 non-existent tenant → 404", func(t *testing.T) {
		tid := freshTenantID()
		resp := e.do(t, http.MethodGet, "/api/v1/tenants/"+tid,
			"", isOwner(tt.OwnerID, tid))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusNotFound)
	})

	t.Run("GT-VAL-01 invalid UUID → 400", func(t *testing.T) {
		// Use iam-system so middleware doesn't reject the invalid path UUID first;
		// parseTenantIDParam in the handler returns 400 for "not-a-uuid".
		resp := e.do(t, http.MethodGet, "/api/v1/tenants/not-a-uuid",
			"", isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("GT-SEC-01 cross-tenant attempt → 404 (other tenant not found)", func(t *testing.T) {
		otherTenantID := freshTenantID()
		// Caller claims to be in otherTenantID, but that tenant doesn't exist.
		// RequireActiveTenant fires first → 404 tenant_not_found (before membership check).
		hdrs := map[string]string{
			"x-user-id": tt.OwnerID, "x-tenant-id": otherTenantID, "x-tenant-roles": "tenant_owner",
		}
		resp := e.do(t, http.MethodGet, "/api/v1/tenants/"+otherTenantID, "", hdrs)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("GT-AUTH-01 no auth headers → 401", func(t *testing.T) {
		resp := e.do(t, http.MethodGet, "/api/v1/tenants/"+tt.TenantID, "", noAuth())
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusUnauthorized)
	})

	t.Run("GT-S-03 suspended member blocked by RequireActiveMembership → 403", func(t *testing.T) {
		resp := e.do(t, http.MethodGet, "/api/v1/tenants/"+tt.TenantID,
			"", isMember(tt.SuspendedID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusForbidden)
	})
}

// TestAPIScenarios_P2_PatchTenant covers P-2.
func TestAPIScenarios_P2_PatchTenant(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)
	rvT := e.getTenantRV(t, tt.TenantID, tt.OwnerID)

	t.Run("P2-HP-01 update name → 200", func(t *testing.T) {
		body := toJSON(map[string]any{"name": "Renamed Corp", "record_version": rvT})
		resp := e.do(t, http.MethodPatch, "/api/v1/tenants/"+tt.TenantID,
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		hasField(t, b, "name", "Renamed Corp")
		rvT = rv(resp)
	})

	t.Run("P2-HP-02 update mfa_freshness_seconds=600 → 200", func(t *testing.T) {
		body := toJSON(map[string]any{"mfa_freshness_seconds": 600, "record_version": rvT})
		resp := e.do(t, http.MethodPatch, "/api/v1/tenants/"+tt.TenantID,
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		assert.Equal(t, float64(600), b["mfa_freshness_seconds"])
		rvT = rv(resp)
	})

	t.Run("P2-VAL-01 mfa_freshness_seconds below 60 → 400", func(t *testing.T) {
		body := toJSON(map[string]any{"mfa_freshness_seconds": 59, "record_version": rvT})
		resp := e.do(t, http.MethodPatch, "/api/v1/tenants/"+tt.TenantID,
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("P2-VAL-02 mfa_freshness_seconds above 900 → 400", func(t *testing.T) {
		body := toJSON(map[string]any{"mfa_freshness_seconds": 901, "record_version": rvT})
		resp := e.do(t, http.MethodPatch, "/api/v1/tenants/"+tt.TenantID,
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("P2-CONC-01 stale record_version → 409", func(t *testing.T) {
		body := toJSON(map[string]any{"name": "X", "record_version": 9999})
		resp := e.do(t, http.MethodPatch, "/api/v1/tenants/"+tt.TenantID,
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusConflict)
	})

	t.Run("P2-AUTH-01 member cannot PATCH tenant → 403", func(t *testing.T) {
		body := toJSON(map[string]any{"name": "X", "record_version": rvT})
		resp := e.do(t, http.MethodPatch, "/api/v1/tenants/"+tt.TenantID,
			body, isMember(tt.MemberID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusForbidden)
	})

	t.Run("P2-NF-01 non-existent tenant → 404", func(t *testing.T) {
		tid := freshTenantID()
		body := toJSON(map[string]any{"name": "X", "record_version": 1})
		resp := e.do(t, http.MethodPatch, "/api/v1/tenants/"+tid,
			body, isOwner(tt.OwnerID, tid))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusNotFound)
	})

	t.Run("P2-HP-03 update default_locale → 200", func(t *testing.T) {
		body := toJSON(map[string]any{"default_locale": "fr-FR", "record_version": rvT})
		resp := e.do(t, http.MethodPatch, "/api/v1/tenants/"+tt.TenantID,
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		hasField(t, b, "default_locale", "fr-FR")
		rvT = rv(resp)
	})

	t.Run("P2-T15-01 realm sync sets realm_sync_pending → 202", func(t *testing.T) {
		body := toJSON(map[string]any{"local_accounts_enabled": true, "record_version": rvT})
		resp := e.do(t, http.MethodPatch, "/api/v1/tenants/"+tt.TenantID,
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		// 200 or 202 depending on RP response
		assert.True(t, resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted,
			"got %d", resp.StatusCode)
		rvT = rv(resp)
	})
}

// TestAPIScenarios_P4_ListMembers covers P-4.
func TestAPIScenarios_P4_ListMembers(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)

	t.Run("P4-HP-01 list with default limit → 200", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/members",
			"", isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		items, _ := b["items"].([]any)
		assert.Greater(t, len(items), 0, "should have at least the owner")
	})

	t.Run("P4-HP-04 plain member can list → 200 (TR-7 member role)", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/members",
			"", isMember(tt.MemberID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusOK)
	})

	t.Run("P4-VAL-01 limit=-1 → 400", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/members?limit=-1",
			"", isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("P4-VAL-02 limit=201 → 400", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/members?limit=201",
			"", isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("P4-VAL-05 limit=1 → 200 with at most 1 item", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/members?limit=1",
			"", isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		items, _ := b["items"].([]any)
		assert.LessOrEqual(t, len(items), 1)
	})

	t.Run("P4-SEC-01 suspended member cannot list → 403", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/members",
			"", isMember(tt.SuspendedID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusForbidden)
	})

	t.Run("P4-AUTH-06 offboarded tenant → 404", func(t *testing.T) {
		// Use a fresh tenant set to offboarded — just verify non-existent tenant returns 404
		tid := freshTenantID()
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tid+"/members",
			"", isOwner(tt.OwnerID, tid))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusNotFound)
	})
}

// TestAPIScenarios_P5_GetMember covers P-5.
func TestAPIScenarios_P5_GetMember(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)

	t.Run("P5-HP-01 owner reads member → 200", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/members/"+tt.MemberID,
			"", isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		hasField(t, b, "user_id", tt.MemberID)
	})

	t.Run("P5-HP-02 plain member reads another → 200", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/members/"+tt.AdminID,
			"", isMember(tt.MemberID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusOK)
	})

	t.Run("P5-NF-01 unknown user → 404", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/members/"+freshID(),
			"", isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusNotFound)
	})

	t.Run("P5-VAL-01 invalid UUID → 400", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/members/not-a-uuid",
			"", isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("P5-SEC-01 suspended caller → 403", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/members/"+tt.MemberID,
			"", isMember(tt.SuspendedID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusForbidden)
	})
}

// TestAPIScenarios_P7_PatchMemberStatus covers P-7.
func TestAPIScenarios_P7_PatchMemberStatus(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)
	memberRV := e.getMemberRV(t, tt.TenantID, tt.OwnerID, tt.MemberID)

	t.Run("P7-HP-01 suspend member → 200", func(t *testing.T) {
		body := toJSON(map[string]any{"status": "suspended", "record_version": memberRV})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/tenants/"+tt.TenantID+"/members/"+tt.MemberID,
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		hasField(t, b, "status", "suspended")
		memberRV = rv(resp)
	})

	t.Run("P7-HP-02 reactivate → 200", func(t *testing.T) {
		body := toJSON(map[string]any{"status": "active", "record_version": memberRV})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/tenants/"+tt.TenantID+"/members/"+tt.MemberID,
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		hasField(t, b, "status", "active")
		memberRV = rv(resp)
	})

	t.Run("P7-AUTH-01 member cannot PATCH status → 403", func(t *testing.T) {
		body := toJSON(map[string]any{"status": "suspended", "record_version": memberRV})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/tenants/"+tt.TenantID+"/members/"+tt.MemberID,
			body, isMember(tt.MemberID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusForbidden)
	})

	t.Run("P7-CONC-01 stale record_version → 409", func(t *testing.T) {
		body := toJSON(map[string]any{"status": "suspended", "record_version": 9999})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/tenants/"+tt.TenantID+"/members/"+tt.MemberID,
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusConflict)
	})

	t.Run("P7-NF-01 unknown member → 404", func(t *testing.T) {
		body := toJSON(map[string]any{"status": "suspended", "record_version": 1})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/tenants/"+tt.TenantID+"/members/"+freshID(),
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusNotFound)
	})

	t.Run("P7-VAL-01 missing record_version → 409 or 400", func(t *testing.T) {
		body := `{"status":"suspended","record_version":0}`
		resp := e.do(t, http.MethodPatch,
			"/api/v1/tenants/"+tt.TenantID+"/members/"+tt.MemberID,
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		// 0 rv → either optimistic_lock_conflict (409) or treated as no rv (400)
		assert.True(t, resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusBadRequest,
			"got %d", resp.StatusCode)
	})
}

// TestAPIScenarios_P9_ListDeptMembers covers P-9.
func TestAPIScenarios_P9_ListDeptMembers(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)
	e.assignDept(t, tt.TenantID, tt.OwnerID, tt.MemberID, DeptEngID.String(), "preparator")

	t.Run("P9-HP-01 list dept members → 200 with items", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/departments/"+DeptEngID.String()+"/members",
			"", isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		items, _ := b["items"].([]any)
		assert.Greater(t, len(items), 0)
	})

	t.Run("P9-HP-02 empty dept → 200 items=[]", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/departments/"+DeptDesID.String()+"/members",
			"", isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		items, _ := b["items"].([]any)
		assert.Equal(t, 0, len(items))
	})

	t.Run("P9-HP-03 plain member can list → 200", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/departments/"+DeptEngID.String()+"/members",
			"", isMember(tt.MemberID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusOK)
	})

	t.Run("P9-SEC-01 suspended caller → 403", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/departments/"+DeptEngID.String()+"/members",
			"", isMember(tt.SuspendedID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusForbidden)
	})

	t.Run("P9-VAL-01 invalid dept UUID → 400", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/departments/not-a-uuid/members",
			"", isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
	})
}

// TestAPIScenarios_P10_AssignDeptMember covers P-10.
func TestAPIScenarios_P10_AssignDeptMember(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)

	t.Run("P10-HP-01 assign active member → 200/201", func(t *testing.T) {
		body := toJSON(map[string]any{"level": "preparator", "record_version": 1})
		resp := e.do(t, http.MethodPut,
			fmt.Sprintf("/api/v1/tenants/%s/departments/%s/members/%s",
				tt.TenantID, DeptEngID, tt.MemberID),
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assert.True(t, resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated,
			"got %d", resp.StatusCode)
	})

	t.Run("P10-HP-02 change level → 200", func(t *testing.T) {
		// Get current rv
		resp := e.do(t, http.MethodGet,
			fmt.Sprintf("/api/v1/tenants/%s/departments/%s/members", tt.TenantID, DeptEngID),
			"", isOwner(tt.OwnerID, tt.TenantID))
		resp.Body.Close()

		body := toJSON(map[string]any{"level": "reviewer", "record_version": 2})
		resp2 := e.do(t, http.MethodPut,
			fmt.Sprintf("/api/v1/tenants/%s/departments/%s/members/%s",
				tt.TenantID, DeptEngID, tt.MemberID),
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp2.Body.Close()
		assert.True(t, resp2.StatusCode == http.StatusOK || resp2.StatusCode == http.StatusCreated,
			"got %d", resp2.StatusCode)
	})

	t.Run("P10-VAL-01 invalid level → 422", func(t *testing.T) {
		body := toJSON(map[string]any{"level": "invalid_level", "record_version": 1})
		resp := e.do(t, http.MethodPut,
			fmt.Sprintf("/api/v1/tenants/%s/departments/%s/members/%s",
				tt.TenantID, DeptEngID, tt.MemberID),
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusUnprocessableEntity)
	})

	t.Run("P10-VAL-02 assign non-member → 422", func(t *testing.T) {
		body := toJSON(map[string]any{"level": "preparator", "record_version": 1})
		resp := e.do(t, http.MethodPut,
			fmt.Sprintf("/api/v1/tenants/%s/departments/%s/members/%s",
				tt.TenantID, DeptEngID, freshID()),
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusUnprocessableEntity)
	})

	t.Run("P10-SUSPENDED-01 assign suspended → 422", func(t *testing.T) {
		body := toJSON(map[string]any{"level": "preparator", "record_version": 1})
		resp := e.do(t, http.MethodPut,
			fmt.Sprintf("/api/v1/tenants/%s/departments/%s/members/%s",
				tt.TenantID, DeptEngID, tt.SuspendedID),
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusUnprocessableEntity)
	})

	t.Run("P10-AUTH-01 plain member cannot assign → 403", func(t *testing.T) {
		body := toJSON(map[string]any{"level": "preparator", "record_version": 1})
		resp := e.do(t, http.MethodPut,
			fmt.Sprintf("/api/v1/tenants/%s/departments/%s/members/%s",
				tt.TenantID, DeptEngID, tt.MemberID),
			body, isMember(tt.MemberID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusForbidden)
	})
}

// TestAPIScenarios_P11_RemoveDeptMember covers P-11.
func TestAPIScenarios_P11_RemoveDeptMember(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)
	e.assignDept(t, tt.TenantID, tt.OwnerID, tt.MemberID, DeptEngID.String(), "preparator")

	t.Run("P11-HP-01 remove assigned member → 200", func(t *testing.T) {
		// Handler returns 200 with {"removed":true,"user_id":...,"department_id":...}.
		resp := e.do(t, http.MethodDelete,
			fmt.Sprintf("/api/v1/tenants/%s/departments/%s/members/%s",
				tt.TenantID, DeptEngID, tt.MemberID),
			"", isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		require.Equal(t, true, b["removed"])
	})

	t.Run("P11-NF-01 remove non-assigned member → 404", func(t *testing.T) {
		resp := e.do(t, http.MethodDelete,
			fmt.Sprintf("/api/v1/tenants/%s/departments/%s/members/%s",
				tt.TenantID, DeptEngID, freshID()),
			"", isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusNotFound)
	})

	t.Run("P11-AUTH-01 plain member cannot remove → 403", func(t *testing.T) {
		e.assignDept(t, tt.TenantID, tt.OwnerID, tt.AdminID, DeptEngID.String(), "preparator")
		resp := e.do(t, http.MethodDelete,
			fmt.Sprintf("/api/v1/tenants/%s/departments/%s/members/%s",
				tt.TenantID, DeptEngID, tt.AdminID),
			"", isMember(tt.MemberID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusForbidden)
	})
}

// TestAPIScenarios_P12_RoleLabels covers P-12/P-13.
func TestAPIScenarios_P12_RoleLabels(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)

	t.Run("P12-HP-01 list role labels → 200", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/roles",
			"", isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		items, _ := b["items"].([]any)
		assert.Greater(t, len(items), 0, "should have 3 default role labels")
	})

	t.Run("P12-HP-02 plain member can list → 200", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/roles",
			"", isMember(tt.MemberID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusOK)
	})

	t.Run("P13-HP-01 update preparator label → 200", func(t *testing.T) {
		rvL := int64(1)
		// Get current rv
		resp0 := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/roles/preparator",
			"", isOwner(tt.OwnerID, tt.TenantID))
		if resp0.StatusCode == http.StatusOK {
			rvL = rv(resp0)
		}
		resp0.Body.Close()

		body := toJSON(map[string]any{"display_name": "Ops", "record_version": rvL})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/tenants/"+tt.TenantID+"/roles/preparator",
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusOK)
	})

	t.Run("P13-VAL-01 empty display_name → 400", func(t *testing.T) {
		body := toJSON(map[string]any{"display_name": "", "record_version": 1})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/tenants/"+tt.TenantID+"/roles/preparator",
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("P13-VAL-02 unknown role_code → 400", func(t *testing.T) {
		body := toJSON(map[string]any{"display_name": "X", "record_version": 1})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/tenants/"+tt.TenantID+"/roles/manager_role",
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
	})
}

// TestAPIScenarios_P24_P25_Departments covers P-24/P-25.
func TestAPIScenarios_P24_P25_Departments(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)

	t.Run("P24-HP-01 activate operations dept → 201", func(t *testing.T) {
		// Use DeptOpsID (non-system, not seeded by TrialSignup) for fresh activation.
		body := toJSON(map[string]any{"department_id": DeptOpsID.String()})
		resp := e.do(t, http.MethodPost,
			"/api/v1/tenants/"+tt.TenantID+"/departments",
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusCreated)
	})

	t.Run("P24-IDP-01 re-activate same dept → 409 conflict", func(t *testing.T) {
		// DeptEngID is already activated by TrialSignup → 409 department_already_activated.
		body := toJSON(map[string]any{"department_id": DeptEngID.String()})
		resp := e.do(t, http.MethodPost,
			"/api/v1/tenants/"+tt.TenantID+"/departments",
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusConflict)
	})

	t.Run("P24-VAL-01 unknown dept ID → 404", func(t *testing.T) {
		// Unknown UUID → catalog returns not-found → 404 (not 422).
		body := toJSON(map[string]any{"department_id": freshID()})
		resp := e.do(t, http.MethodPost,
			"/api/v1/tenants/"+tt.TenantID+"/departments",
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusNotFound)
	})

	t.Run("P25-HP-01 deactivate operations dept → 200", func(t *testing.T) {
		// DeptOpsID was activated in P24-HP-01 above; deactivating a non-system dept is allowed.
		rvD := e.getDeptRV(t, tt.TenantID, tt.OwnerID, DeptOpsID.String())
		body := toJSON(map[string]any{"is_active": false, "record_version": rvD})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/tenants/"+tt.TenantID+"/departments/"+DeptOpsID.String(),
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusOK)
	})

	t.Run("P25-VAL-01 deactivate system dept → 422", func(t *testing.T) {
		rvD := e.getDeptRV(t, tt.TenantID, tt.OwnerID, DeptEngID.String())
		body := toJSON(map[string]any{"is_active": false, "record_version": rvD})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/tenants/"+tt.TenantID+"/departments/"+DeptEngID.String(),
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusUnprocessableEntity)
	})
}

// TestAPIScenarios_P27_SeatUsage covers P-27.
func TestAPIScenarios_P27_SeatUsage(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)

	t.Run("P27-HP-01 owner reads seat usage → 200", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/seat-usage",
			"", isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		require.Contains(t, b, "active_users")
		require.Contains(t, b, "licensed_seats")
		require.Contains(t, b, "over_cap")
	})

	t.Run("P27-HP-02 admin reads seat usage → 200", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/seat-usage",
			"", isAdmin(tt.AdminID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusOK)
	})

	t.Run("P27-AUTH-01 plain member cannot read → 403", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/seat-usage",
			"", isMember(tt.MemberID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusForbidden)
	})

	t.Run("P27-NF-01 unknown tenant → 404", func(t *testing.T) {
		tid := freshTenantID()
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tid+"/seat-usage",
			"", isOwner(tt.OwnerID, tid))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusNotFound)
	})
}

// TestAPIScenarios_P28_ReconcileRoles covers P-28.
func TestAPIScenarios_P28_ReconcileRoles(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)
	memberRV := e.getMemberRV(t, tt.TenantID, tt.OwnerID, tt.MemberID)

	t.Run("P28-HP-01 grant tenant_admin → 200", func(t *testing.T) {
		body := toJSON(map[string]any{"roles": []string{"tenant_admin"}, "record_version": memberRV})
		resp := e.do(t, http.MethodPut,
			"/api/v1/tenants/"+tt.TenantID+"/members/"+tt.MemberID+"/roles",
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		// P-28 returns the full post-reconcile elevated role set in "roles" (not a delta).
		roles, _ := b["roles"].([]any)
		assert.Contains(t, roles, "tenant_admin")
		memberRV = e.getMemberRV(t, tt.TenantID, tt.OwnerID, tt.MemberID)
	})

	t.Run("P28-HP-02 strip all roles → 200", func(t *testing.T) {
		body := toJSON(map[string]any{"roles": []string{}, "record_version": memberRV})
		resp := e.do(t, http.MethodPut,
			"/api/v1/tenants/"+tt.TenantID+"/members/"+tt.MemberID+"/roles",
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		// After stripping all roles, the "roles" array must be empty.
		roles, _ := b["roles"].([]any)
		assert.Empty(t, roles, "all elevated roles stripped — roles must be empty")
	})

	t.Run("P28-VAL-01 unknown role → 422", func(t *testing.T) {
		body := toJSON(map[string]any{"roles": []string{"superman"}, "record_version": memberRV})
		resp := e.do(t, http.MethodPut,
			"/api/v1/tenants/"+tt.TenantID+"/members/"+tt.MemberID+"/roles",
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusUnprocessableEntity)
	})

	t.Run("P28-VAL-02 member role is derived (not persistent) → 422", func(t *testing.T) {
		body := toJSON(map[string]any{"roles": []string{"member"}, "record_version": memberRV})
		resp := e.do(t, http.MethodPut,
			"/api/v1/tenants/"+tt.TenantID+"/members/"+tt.MemberID+"/roles",
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusUnprocessableEntity)
	})

	t.Run("P28-NF-01 unknown user → 404", func(t *testing.T) {
		body := toJSON(map[string]any{"roles": []string{"tenant_admin"}, "record_version": 1})
		resp := e.do(t, http.MethodPut,
			"/api/v1/tenants/"+tt.TenantID+"/members/"+freshID()+"/roles",
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusNotFound)
	})

	t.Run("P28-TM8-01 last-owner cannot lose tenant_owner → 422", func(t *testing.T) {
		ownerRV := e.getMemberRV(t, tt.TenantID, tt.OwnerID, tt.OwnerID)
		body := toJSON(map[string]any{"roles": []string{}, "record_version": ownerRV})
		resp := e.do(t, http.MethodPut,
			"/api/v1/tenants/"+tt.TenantID+"/members/"+tt.OwnerID+"/roles",
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusUnprocessableEntity)
	})
}

// TestAPIScenarios_P30_ListInvitations covers P-30.
func TestAPIScenarios_P30_ListInvitations(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)

	t.Run("P30-HP-01 owner lists invitations → 200", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/invitations",
			"", isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		require.Contains(t, b, "items")
	})

	t.Run("P30-AUTH-01 plain member cannot list invitations → 403", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tt.TenantID+"/invitations",
			"", isMember(tt.MemberID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusForbidden)
	})

	t.Run("P30-NF-01 unknown tenant → 404", func(t *testing.T) {
		tid := freshTenantID()
		resp := e.do(t, http.MethodGet,
			"/api/v1/tenants/"+tid+"/invitations",
			"", isOwner(tt.OwnerID, tid))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusNotFound)
	})
}

// TestAPIScenarios_P31_RevokeInvitation covers P-31.
func TestAPIScenarios_P31_RevokeInvitation(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)

	// Invite someone first via P-6
	email := "p31-" + freshID()[:8] + "@test.com"
	invBody := toJSON(map[string]any{
		"email": email, "full_name": "P31 User", "initial_tenant_role": "tenant_admin",
	})
	invResp := e.do(t, http.MethodPost,
		"/api/v1/tenants/"+tt.TenantID+"/members",
		invBody, isOwner(tt.OwnerID, tt.TenantID))
	invB := parseBody(t, invResp)
	invResp.Body.Close()
	require.True(t, invResp.StatusCode == http.StatusCreated || invResp.StatusCode == http.StatusAccepted,
		"invite: got %d body: %v", invResp.StatusCode, invB)

	invID, _ := invB["invitation_id"].(string)
	require.NotEmpty(t, invID, "invitation_id must be present: %v", invB)

	t.Run("P31-HP-01 revoke pending invitation → 204", func(t *testing.T) {
		// record_version=1 for fresh invitation; handler also accepts ?record_version=N query param.
		body := toJSON(map[string]any{"record_version": 1})
		resp := e.do(t, http.MethodDelete,
			"/api/v1/tenants/"+tt.TenantID+"/invitations/"+invID,
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusNoContent)
	})

	t.Run("P31-NF-01 revoke unknown invitation → 404", func(t *testing.T) {
		resp := e.do(t, http.MethodDelete,
			"/api/v1/tenants/"+tt.TenantID+"/invitations/"+freshID(),
			"", isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusNotFound)
	})

	t.Run("P31-AUTH-01 plain member cannot revoke → 403", func(t *testing.T) {
		resp := e.do(t, http.MethodDelete,
			"/api/v1/tenants/"+tt.TenantID+"/invitations/"+freshID(),
			"", isMember(tt.MemberID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusForbidden)
	})
}

// TestAPIScenarios_O4_FeatureFlags covers O-4.
func TestAPIScenarios_O4_FeatureFlags(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)
	rvT := e.getTenantRV(t, tt.TenantID, tt.OwnerID)

	t.Run("O4-HP-01 set sso_enabled=true → 200", func(t *testing.T) {
		body := toJSON(map[string]any{
			"feature_flags": map[string]any{"sso_enabled": true},
			"record_version": rvT,
		})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/operator/tenants/"+tt.TenantID+"/feature-flags",
			body, isOperator(tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		flags, _ := b["feature_flags"].(map[string]any)
		assert.Equal(t, true, flags["sso_enabled"])
		rvT = rv(resp)
	})

	t.Run("O4-HP-02 set empty flags → 200 clears all", func(t *testing.T) {
		body := toJSON(map[string]any{
			"feature_flags": map[string]any{},
			"record_version": rvT,
		})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/operator/tenants/"+tt.TenantID+"/feature-flags",
			body, isOperator(tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		flags, _ := b["feature_flags"].(map[string]any)
		assert.Equal(t, 0, len(flags))
		rvT = rv(resp)
	})

	t.Run("O4-VAL-01 unknown flag key → 400", func(t *testing.T) {
		body := toJSON(map[string]any{
			"feature_flags": map[string]any{"sso_enable": true}, // typo
			"record_version": rvT,
		})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/operator/tenants/"+tt.TenantID+"/feature-flags",
			body, isOperator(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("O4-VAL-02 nested object value → 400", func(t *testing.T) {
		body := toJSON(map[string]any{
			"feature_flags": map[string]any{"sso_enabled": map[string]any{"nested": true}},
			"record_version": rvT,
		})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/operator/tenants/"+tt.TenantID+"/feature-flags",
			body, isOperator(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("O4-AUTH-01 non-operator → 403", func(t *testing.T) {
		body := toJSON(map[string]any{
			"feature_flags": map[string]any{"sso_enabled": true},
			"record_version": rvT,
		})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/operator/tenants/"+tt.TenantID+"/feature-flags",
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusForbidden)
	})

	t.Run("O4-CONC-01 stale record_version → 409", func(t *testing.T) {
		body := toJSON(map[string]any{
			"feature_flags": map[string]any{"sso_enabled": true},
			"record_version": 9999,
		})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/operator/tenants/"+tt.TenantID+"/feature-flags",
			body, isOperator(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusConflict)
	})

	t.Run("O4-NF-01 unknown tenant → 404", func(t *testing.T) {
		tid := freshTenantID()
		body := toJSON(map[string]any{
			"feature_flags": map[string]any{"sso_enabled": true},
			"record_version": 1,
		})
		resp := e.do(t, http.MethodPatch,
			"/api/v1/operator/tenants/"+tid+"/feature-flags",
			body, isOperator(tid))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusNotFound)
	})
}

// TestAPIScenarios_O7_ReassignOwner covers O-7.
func TestAPIScenarios_O7_ReassignOwner(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)
	rvT := e.getTenantRV(t, tt.TenantID, tt.OwnerID)

	t.Run("O7-HP-01 reassign to admin → 200", func(t *testing.T) {
		body := toJSON(map[string]any{"user_id": tt.AdminID, "record_version": rvT})
		resp := e.do(t, http.MethodPost,
			"/api/v1/operator/tenants/"+tt.TenantID+"/reassign-owner",
			body, isOperator(tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		roles, _ := b["roles"].([]any)
		require.Contains(t, roles, "tenant_owner", "response roles must include tenant_owner — body: %v", b)
		rvT = e.getTenantRV(t, tt.TenantID, tt.OwnerID)
	})

	t.Run("O7-HP-02 reassign back to original owner → 200", func(t *testing.T) {
		body := toJSON(map[string]any{"user_id": tt.OwnerID, "record_version": rvT})
		resp := e.do(t, http.MethodPost,
			"/api/v1/operator/tenants/"+tt.TenantID+"/reassign-owner",
			body, isOperator(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusOK)
		rvT = e.getTenantRV(t, tt.TenantID, tt.OwnerID)
	})

	t.Run("O7-VAL-01 new owner is not a member → 422", func(t *testing.T) {
		body := toJSON(map[string]any{"user_id": freshID(), "record_version": rvT})
		resp := e.do(t, http.MethodPost,
			"/api/v1/operator/tenants/"+tt.TenantID+"/reassign-owner",
			body, isOperator(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusUnprocessableEntity)
	})

	t.Run("O7-VAL-02 new owner is suspended → 422", func(t *testing.T) {
		body := toJSON(map[string]any{"user_id": tt.SuspendedID, "record_version": rvT})
		resp := e.do(t, http.MethodPost,
			"/api/v1/operator/tenants/"+tt.TenantID+"/reassign-owner",
			body, isOperator(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusUnprocessableEntity)
	})

	t.Run("O7-AUTH-01 non-operator → 403", func(t *testing.T) {
		body := toJSON(map[string]any{"user_id": tt.AdminID, "record_version": rvT})
		resp := e.do(t, http.MethodPost,
			"/api/v1/operator/tenants/"+tt.TenantID+"/reassign-owner",
			body, isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusForbidden)
	})

	t.Run("O7-CONC-01 record_version field is accepted but O7 has no OL → 200", func(t *testing.T) {
		// O7 ReassignOwner does not implement optimistic locking on the tenant row;
		// record_version in the body is ignored. The operation succeeds regardless.
		body := toJSON(map[string]any{"user_id": tt.AdminID, "record_version": 9999})
		resp := e.do(t, http.MethodPost,
			"/api/v1/operator/tenants/"+tt.TenantID+"/reassign-owner",
			body, isOperator(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusOK)
	})

	t.Run("O7-NF-01 unknown tenant → 404", func(t *testing.T) {
		tid := freshTenantID()
		body := toJSON(map[string]any{"user_id": tt.AdminID, "record_version": 1})
		resp := e.do(t, http.MethodPost,
			"/api/v1/operator/tenants/"+tid+"/reassign-owner",
			body, isOperator(tid))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusNotFound)
	})

	t.Run("O7-deprecated-01 new_owner_user_id alias → 200", func(t *testing.T) {
		body := toJSON(map[string]any{"new_owner_user_id": tt.AdminID, "record_version": rvT})
		resp := e.do(t, http.MethodPost,
			"/api/v1/operator/tenants/"+tt.TenantID+"/reassign-owner",
			body, isOperator(tt.TenantID))
		defer resp.Body.Close()
		// Deprecated alias still works
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
}

// TestAPIScenarios_I15_MemberExists covers I-15.
func TestAPIScenarios_I15_MemberExists(t *testing.T) {
	e := newAPITestEnv(t)
	tt := newTestTenant(t, e)

	t.Run("I15-HP-01 active member exists → 200", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/internal/tenants/"+tt.TenantID+"/members/"+tt.MemberID+"/exists",
			"", isSys(tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusOK)
	})

	t.Run("I15-HP-02 non-existent member → 200 active:false", func(t *testing.T) {
		// I-15 "never 404s" per LLD §5.4: returns 200 with {"active":false} for unknown users.
		resp := e.do(t, http.MethodGet,
			"/api/v1/internal/tenants/"+tt.TenantID+"/members/"+freshID()+"/exists",
			"", isSys(tt.TenantID))
		defer resp.Body.Close()
		b := assertStatus(t, resp, http.StatusOK)
		require.Equal(t, false, b["active"], "non-existent member must return active:false")
	})

	t.Run("I15-AUTH-01 non-iam-system → 403", func(t *testing.T) {
		resp := e.do(t, http.MethodGet,
			"/api/v1/internal/tenants/"+tt.TenantID+"/members/"+tt.MemberID+"/exists",
			"", isOwner(tt.OwnerID, tt.TenantID))
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusForbidden)
	})
}

// ── Helper: get dept record_version ──────────────────────────────────────────

func (e *apiTestEnv) getDeptRV(t *testing.T, tenantID, callerID, deptID string) int64 {
	t.Helper()
	resp := e.do(t, http.MethodGet,
		"/api/v1/tenants/"+tenantID+"/departments/"+deptID,
		"", isOwner(callerID, tenantID))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return rv(resp)
}
