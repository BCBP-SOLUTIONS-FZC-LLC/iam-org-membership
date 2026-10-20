//go:build integration

// Package postgres_test — I-19 assignee eligibility against real Postgres
// (RLS on, real repositories): the response contract, the deactivated-
// department and soft-deleted-membership cases the I-8 projection does not
// reflect, and path-tenant scoping across tenants.
package postgres_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/http"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
)

// seedDeptMember activates a department for the tenant and gives userID the
// level there. Returns the department ID.
func seedDeptMember(t *testing.T, ctx context.Context, fx *testFixtures, tenantID, userID uuid.UUID, level string) uuid.UUID {
	t.Helper()
	deptID := uuid.New()
	_, err := fx.rawPool.Exec(ctx, `INSERT INTO tenant_departments (tenant_id, department_id) VALUES ($1, $2)`, tenantID, deptID)
	require.NoError(t, err)
	_, err = fx.rawPool.Exec(ctx, `
		INSERT INTO dept_memberships (tenant_id, user_id, tenant_membership_id, department_id, role_level, granted_by)
		SELECT $1, $2, tm.id, $3, $4::dept_role, $2
		FROM tenant_memberships tm WHERE tm.tenant_id = $1 AND tm.user_id = $2`,
		tenantID, userID, deptID, level)
	require.NoError(t, err)
	return deptID
}

// callI19 invokes the handler with the caller's GUC/x-tenant-id bound to
// callerTenant and the path :id set to pathTenant.
func callI19(t *testing.T, fx *testFixtures, pathTenant, callerTenant, userID, deptID uuid.UUID, level string) map[string]any {
	t.Helper()
	rc := &requestctx.RequestContext{TenantID: callerTenant, UserID: uuid.Nil, Roles: []string{"iam-system"}}
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// The caller's GUC carries callerTenant, as GUCBridgeMiddleware sets it
	// from x-tenant-id; the handler must rebind to the path tenant.
	baseCtx := requestctx.WithContext(withTenant(context.Background(), callerTenant), rc)
	c.Request = httptest.NewRequestWithContext(baseCtx, http.MethodGet,
		"/?department_id="+deptID.String()+"&required_level="+level, nil)
	c.Params = gin.Params{
		{Key: "id", Value: pathTenant.String()},
		{Key: "user_id", Value: userID.String()},
	}
	h := httpadapter.NewInternalHandler(nil, nil, nil, nil, nil, nil, nil, fx.DeptMembership)
	h.CheckAssigneeEligibility(c)
	require.Equal(t, http.StatusOK, w.Code, "I-19 must answer 200: %s", w.Body.String())
	var out map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&out))
	return out
}

func TestI19_Eligibility_LevelOrderAgainstRealRows(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, userID := seedTenantWithOwner(t, ctx, fx, "i19-levels")
	deptID := seedDeptMember(t, ctx, fx, tenantID, userID, "reviewer")

	for level, want := range map[string]bool{"preparator": true, "reviewer": true, "approver": false} {
		out := callI19(t, fx, tenantID, tenantID, userID, deptID, level)
		assert.Equal(t, want, out["eligible"], "reviewer for required_level=%s", level)
		assert.Equal(t, "reviewer", out["role_level"])
		if !want {
			assert.Equal(t, "level_insufficient", out["reason"])
		}
	}
}

// Deactivating a department leaves dept_memberships in place (I-8 still
// lists it); I-19 must refuse.
func TestI19_Eligibility_DeactivatedDepartment(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, userID := seedTenantWithOwner(t, ctx, fx, "i19-deactivated")
	deptID := seedDeptMember(t, ctx, fx, tenantID, userID, "approver")

	_, err := fx.rawPool.Exec(ctx, `UPDATE tenant_departments SET is_active = false WHERE tenant_id = $1 AND department_id = $2`, tenantID, deptID)
	require.NoError(t, err)

	out := callI19(t, fx, tenantID, tenantID, userID, deptID, "preparator")
	assert.Equal(t, false, out["eligible"])
	assert.Equal(t, "department_not_active", out["reason"])
}

func TestI19_Eligibility_RemovedFromDepartment(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, userID := seedTenantWithOwner(t, ctx, fx, "i19-removed")
	deptID := seedDeptMember(t, ctx, fx, tenantID, userID, "approver")

	_, err := fx.rawPool.Exec(ctx, `UPDATE dept_memberships SET deleted_at = now() WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID)
	require.NoError(t, err)

	out := callI19(t, fx, tenantID, tenantID, userID, deptID, "preparator")
	assert.Equal(t, false, out["eligible"])
	assert.Equal(t, "not_in_department", out["reason"])
}

func TestI19_Eligibility_SuspendedMember(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, userID := seedTenantWithOwner(t, ctx, fx, "i19-suspended")
	deptID := seedDeptMember(t, ctx, fx, tenantID, userID, "approver")

	_, err := fx.rawPool.Exec(ctx, `UPDATE tenant_memberships SET status = 'suspended' WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID)
	require.NoError(t, err)

	out := callI19(t, fx, tenantID, tenantID, userID, deptID, "preparator")
	assert.Equal(t, false, out["eligible"])
	assert.Equal(t, "member_not_active", out["reason"])
}
