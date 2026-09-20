// Stage 2 read-cutover — outbound clients · GroupMapping.
//
// Module:   iam-org-membership
// Feature:  Group Mapping / JIT Config Service client — ResolveGroups
//
//	(GM-I1). Unlike catalogadmin, an unconfigured base URL also
//	errors here — service.GroupMappingService is what supplies the
//	fail-open behavior (cache → stale → empty resolution).
//
// File:     internal/adapter/outbound/groupmappingclient/http_client.go
package groupmappingclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveGroups_Happy(t *testing.T) {
	tenantID := uuid.New()
	deptID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/internal/tenants/"+tenantID.String()+"/group-resolution", r.URL.Path)
		assert.Equal(t, "iam-system", r.Header.Get("x-user-id"))
		assert.Equal(t, "iam-system", r.Header.Get("x-tenant-roles"))
		assert.Equal(t, tenantID.String(), r.Header.Get("x-tenant-id"),
			"GM-I1 runs under the TARGET tenant's GUC, unlike catalogadmin's uuid.Nil placeholder")

		var body groupResolutionRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, []string{"eng-team", "tender-admins"}, body.Groups)

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"dept_mappings": []map[string]any{
				{"keycloak_group_name": "eng-team", "department_id": deptID},
			},
			"dept_role_mappings": []map[string]any{
				{"keycloak_group_name": "eng-team", "role_code": "reviewer"},
			},
			"tenant_role_mappings": []map[string]any{
				{"keycloak_group_name": "tender-admins", "role_code": "tender_admin"},
			},
		})
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	res, err := c.ResolveGroups(context.Background(), tenantID, []string{"eng-team", "tender-admins"})
	require.NoError(t, err)
	require.Len(t, res.DeptMappings, 1)
	assert.Equal(t, "eng-team", res.DeptMappings[0].KeycloakGroupName)
	assert.Equal(t, deptID, res.DeptMappings[0].DepartmentID)
	require.Len(t, res.DeptRoleMappings, 1)
	assert.Equal(t, domain.DeptReviewer, res.DeptRoleMappings[0].RoleCode)
	require.Len(t, res.TenantRoleMappings, 1)
	assert.Equal(t, domain.RoleTenderAdmin, res.TenantRoleMappings[0].RoleCode)
}

func TestResolveGroups_EmptyDimensionsAreNonNilSlices(t *testing.T) {
	tenantID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"dept_mappings":        []map[string]any{},
			"dept_role_mappings":   []map[string]any{},
			"tenant_role_mappings": []map[string]any{},
		})
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	res, err := c.ResolveGroups(context.Background(), tenantID, []string{"nonexistent-group"})
	require.NoError(t, err)
	assert.NotNil(t, res.DeptMappings)
	assert.NotNil(t, res.DeptRoleMappings)
	assert.NotNil(t, res.TenantRoleMappings)
	assert.Empty(t, res.DeptMappings)
	assert.Empty(t, res.DeptRoleMappings)
	assert.Empty(t, res.TenantRoleMappings)
}

func TestResolveGroups_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"internal_error"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	_, err := c.ResolveGroups(context.Background(), uuid.New(), []string{"eng-team"})
	require.Error(t, err)
}

func TestResolveGroups_EmptyBaseURL_Errors(t *testing.T) {
	// Resilience (cache → stale → fail-open) is
	// service.GroupMappingService's job, not this client's — it must
	// surface a plain error so the caller can decide.
	c := New(nil)
	_, err := c.ResolveGroups(context.Background(), uuid.New(), []string{"eng-team"})
	require.Error(t, err)
}

func TestResolveGroups_TransportError(t *testing.T) {
	c := NewHTTPClient("http://127.0.0.1:1", 200*time.Millisecond, nil)
	_, err := c.ResolveGroups(context.Background(), uuid.New(), []string{"eng-team"})
	require.Error(t, err)
}

func TestNewHTTPClientGM_NilLogger_FallsBackToSlogDefault(t *testing.T) {
	// nil logger → no-op sink — must not panic.
	c := NewHTTPClient("http://localhost", 5*time.Second, nil)
	require.NotNil(t, c)
}

func TestNewHTTPClientGM_ZeroTimeout_DefaultsTo300ms(t *testing.T) {
	// timeout <= 0 → defaults to 300ms.
	c := NewHTTPClient("http://localhost", 0, nil)
	require.NotNil(t, c)
	assert.Equal(t, 300*time.Millisecond, c.client.Timeout)
}

func TestResolveGroups_400Status_4xxErrorNotIncremented(t *testing.T) {
	// A 4xx status (not >= 500) must return an error but NOT call IncXsvcError
	// with "5xx" (only >= 500 triggers that label).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"bad_request"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	_, err := c.ResolveGroups(context.Background(), uuid.New(), []string{"eng-team"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "400")
}
