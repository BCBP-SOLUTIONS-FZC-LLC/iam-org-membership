//go:build integration

// Package postgres_test — I-15 and I-13 scope their reads to the path
// tenant, not the caller's x-tenant-id. Every repository query filters on
// the path tenant and RLS then applies the GUC tenant, so before the handlers
// bound the path tenant as the GUC tenant a mismatched header read zero rows:
// I-15 answered a false active:false and I-13 rejected a valid override.
package postgres_test

import (
	"bytes"
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

// mismatchedTenantCtx builds a request whose GUC tenant is headerTenant, as
// GUCBridgeMiddleware sets it from x-tenant-id, independent of the path.
func mismatchedTenantCtx(method, target string, body []byte, headerTenant uuid.UUID) (*httptest.ResponseRecorder, *gin.Context) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	rc := &requestctx.RequestContext{TenantID: headerTenant, UserID: uuid.Nil, Roles: []string{"iam-system"}}
	ctx := requestctx.WithContext(withTenant(context.Background(), headerTenant), rc)
	c.Request = httptest.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return w, c
}

func TestI15_PathTenantIsAuthoritative_MismatchedHeader(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantA, _ := seedTenantWithOwner(t, ctx, fx, "i15-path-a")
	tenantB, userB := seedTenantWithOwner(t, ctx, fx, "i15-path-b")

	w, c := mismatchedTenantCtx(http.MethodGet, "/", nil, tenantA)
	c.Params = gin.Params{{Key: "id", Value: tenantB.String()}, {Key: "user_id", Value: userB.String()}}
	httpadapter.NewInternalHandler(fx.Provisioning, fx.AuthZ, fx.Membership, nil, nil, nil, nil).CheckMemberExists(c)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, true, out["active"], "an active member of the path tenant must read active:true whatever x-tenant-id says")
}

// The reverse direction still answers for the path tenant only: a member of
// tenant B asked about under path tenant A is not active there, even when
// the header names B.
func TestI15_PathTenantIsAuthoritative_NoCrossTenantAnswer(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantA, _ := seedTenantWithOwner(t, ctx, fx, "i15-cross-a")
	tenantB, userB := seedTenantWithOwner(t, ctx, fx, "i15-cross-b")

	w, c := mismatchedTenantCtx(http.MethodGet, "/", nil, tenantB)
	c.Params = gin.Params{{Key: "id", Value: tenantA.String()}, {Key: "user_id", Value: userB.String()}}
	httpadapter.NewInternalHandler(fx.Provisioning, fx.AuthZ, fx.Membership, nil, nil, nil, nil).CheckMemberExists(c)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, false, out["active"])
}

func TestI13_PathTenantIsAuthoritative_MismatchedHeader(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantA, _ := seedTenantWithOwner(t, ctx, fx, "i13-path-a")
	tenantB, ownerB := seedTenantWithOwner(t, ctx, fx, "i13-path-b")

	// ownerB (tenant_owner — satisfies the actor check) is also the assignee,
	// holding approver in an active department of tenant B.
	deptID := uuid.New()
	_, err := fx.rawPool.Exec(ctx, `INSERT INTO tenant_departments (tenant_id, department_id) VALUES ($1, $2)`, tenantB, deptID)
	require.NoError(t, err)
	_, err = fx.rawPool.Exec(ctx, `
		INSERT INTO dept_memberships (tenant_id, user_id, tenant_membership_id, department_id, role_level, granted_by)
		SELECT $1, $2, tm.id, $3, 'approver', $2 FROM tenant_memberships tm WHERE tm.tenant_id = $1 AND tm.user_id = $2`,
		tenantB, ownerB, deptID)
	require.NoError(t, err)

	body, _ := json.Marshal(map[string]any{
		"new_user_id": ownerB, "department_id": deptID, "required_level": "reviewer", "actor_id": ownerB,
	})
	w, c := mismatchedTenantCtx(http.MethodPost, "/", body, tenantA)
	c.Params = gin.Params{{Key: "id", Value: tenantB.String()}, {Key: "tender_id", Value: uuid.New().String()}}
	httpadapter.NewInternalHandler(fx.Provisioning, fx.AuthZ, fx.Membership, nil, nil, nil, nil).AssigneeOverride(c)

	require.Equal(t, http.StatusOK, w.Code, "a valid override in the path tenant must pass whatever x-tenant-id says: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), `"eligible":true`)
}
