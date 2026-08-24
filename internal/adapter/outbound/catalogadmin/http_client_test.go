// Read-cutover — outbound clients · CatalogAdmin.
//
// Module:   iam-org-membership
// Feature:  Catalog / Admin Config Service client — Departments (CAT-I1),
//
//	Plans (CAT-I2). Unlike sibling outbound clients, an unconfigured
//	base URL errors rather than fail-open (see package doc).
//
// File:     internal/adapter/outbound/catalogadmin/http_client.go
package catalogadmin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDepartments_Happy(t *testing.T) {
	deptID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/internal/departments", r.URL.Path)
		assert.Equal(t, "iam-system", r.Header.Get("x-user-id"))
		assert.Equal(t, "iam-system", r.Header.Get("x-tenant-roles"))
		assert.Equal(t, uuid.Nil.String(), r.Header.Get("x-tenant-id"))
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"departments": []map[string]any{
				{"id": deptID, "code": "ENGINEERING", "name": "Engineering", "is_system": true, "is_active": true, "record_version": 3},
			},
			"as_of": "2026-08-13T10:00:00Z",
		})
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	depts, err := c.Departments(context.Background())
	require.NoError(t, err)
	require.Len(t, depts, 1)
	assert.Equal(t, deptID, depts[0].ID)
	assert.Equal(t, "ENGINEERING", depts[0].Code)
	assert.True(t, depts[0].IsSystem)
	assert.EqualValues(t, 3, depts[0].RecordVersion)
}

func TestDepartments_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"insufficient_role"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	_, err := c.Departments(context.Background())
	require.Error(t, err)
}

func TestDepartments_EmptyBaseURL_ErrorsNotFailOpen(t *testing.T) {
	// Unlike realmprovisioner/userprofile/workflow, this client must NOT
	// fabricate a safe default when unconfigured — fabricating catalog
	// data is worse than an explicit error.
	c := New(nil)
	_, err := c.Departments(context.Background())
	require.Error(t, err)
}

func TestPlans_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/internal/plans", r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plans": []map[string]any{
				{"code": "starter", "display_name": "Starter", "workflow_template_limit": 5, "tender_limit": 10,
					"trial_duration_days": 30, "sso_enabled": false, "custom_branding": "none", "feature_set": map[string]any{}, "record_version": 1},
				{"code": "enterprise", "display_name": "Enterprise", "workflow_template_limit": nil, "tender_limit": nil,
					"trial_duration_days": 30, "sso_enabled": true, "custom_branding": "logo", "feature_set": map[string]any{}, "record_version": 5},
			},
			"record_versions": map[string]any{"starter": 1, "enterprise": 5},
		})
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	plans, err := c.Plans(context.Background())
	require.NoError(t, err)
	require.Len(t, plans, 2)
	assert.Equal(t, "starter", string(plans[0].Code))
	require.NotNil(t, plans[0].WorkflowTemplateLimit)
	assert.Equal(t, 5, *plans[0].WorkflowTemplateLimit)
	assert.Nil(t, plans[1].WorkflowTemplateLimit, "enterprise ships NULL = unlimited (CAT-D6)")
}

func TestPlans_EmptyBaseURL_ErrorsNotFailOpen(t *testing.T) {
	c := New(nil)
	_, err := c.Plans(context.Background())
	require.Error(t, err)
}

func TestPlans_TransportError(t *testing.T) {
	c := NewHTTPClient("http://127.0.0.1:1", 200*time.Millisecond, slog.Default())
	_, err := c.Plans(context.Background())
	require.Error(t, err)
}
