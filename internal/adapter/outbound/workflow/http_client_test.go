// Phase 10 — outbound clients · Workflow.
//
// Module:   iam-org-membership
// Feature:  WorkflowClient — WFI-3 pre-check + WFI-13 fail-open + P-26 actions
// File:     internal/adapter/outbound/workflow/http_client.go
//
// Test IDs: P10-WF-NNN.
//
// Pure unit — uses httptest to stand up a fake Workflow endpoint, no
// testcontainers or real deps. Runs in the fast tier.
package workflow

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═════════════════════════════════════════════════════════════════════════
// GetDelegateImpact
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P10-WF-001
// Module:            iam-org-membership · WorkflowClient
// Feature:           WFI-3 · GetDelegateImpact happy path
// API:               GET /api/v1/internal/workflows/active-by-user
// Scenario:          Positive — server returns {active_workflows: 0, workflow_ids: []}
// Preconditions:     fake Workflow server up
// Test Steps:
//  1. Stand up httptest server returning valid JSON
//  2. Construct HTTPClient with the URL
//  3. Call GetDelegateImpact
//
// Expected Result:
//   - Returns non-nil DelegateImpact with ActiveWorkflows=0
//   - Server observed a GET with the correct query params
//
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestP10WF001_GetDelegateImpactHappy(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"active_workflows":0,"workflow_ids":[]}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	impact, err := c.GetDelegateImpact(context.Background(), uuid.New(), uuid.New(), nil)
	require.NoError(t, err)
	require.NotNil(t, impact)
	assert.Equal(t, 0, impact.ActiveWorkflows)
	assert.Equal(t, http.MethodGet, gotMethod)
	assert.Equal(t, "/api/v1/internal/workflows/delegate-impact", gotPath)
	assert.Contains(t, gotQuery, "tenant_id=")
	assert.Contains(t, gotQuery, "delegate_user_id=")
}

// Test Case ID:      P10-WF-002
// Module:            iam-org-membership · WorkflowClient
// Feature:           WFI-13 · fail-open when baseURL unconfigured
// API:               GET /api/v1/internal/workflows/delegate-impact
// Scenario:          Positive — Workflow client with empty baseURL returns 0-impact
// Preconditions:     none
// Test Steps:
//  1. Construct HTTPClient with baseURL=""
//  2. Call GetDelegateImpact
//
// Expected Result:
//   - Returns non-nil DelegateImpact with ActiveWorkflows=0
//   - No error (WFI-13: unconfigured Workflow must not block removal)
//
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestP10WF002_WFI13FailOpenWhenUnconfigured(t *testing.T) {
	c := NewHTTPClient("", 5*time.Second, nil)
	impact, err := c.GetDelegateImpact(context.Background(), uuid.New(), uuid.New(), nil)
	require.NoError(t, err, "WFI-13 fail-open: no error when unconfigured")
	require.NotNil(t, impact)
	assert.Equal(t, 0, impact.ActiveWorkflows)
}

// Test Case ID:      P10-WF-003
// Module:            iam-org-membership · WorkflowClient
// Feature:           GetDelegateImpact carries delegation_id when set
// API:               GET /api/v1/internal/workflows/active-by-user
// Scenario:          Positive — delegation_id present in query (§8.8.4 WFI-11)
// Preconditions:     fake server up
// Test Steps:
//  1. Call GetDelegateImpact with a non-nil delegationID
//
// Expected Result:
//   - Server sees delegation_id=<uuid> in query
//
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10WF003_GetDelegateImpactWithDelegationID(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"active_workflows":0}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	delID := uuid.New()
	_, err := c.GetDelegateImpact(context.Background(), uuid.New(), uuid.New(), &delID)
	require.NoError(t, err)
	assert.Contains(t, gotQuery, "delegation_id="+delID.String())
}

// Test Case ID:      P10-WF-004
// Module:            iam-org-membership · WorkflowClient
// Feature:           GetDelegateImpact non-2xx → error (removal blocks)
// Scenario:          Negative — server returns 500
// Preconditions:     fake server returning 500
// Test Steps:
//  1. Fake server returns 500
//  2. Call GetDelegateImpact
//
// Expected Result:
//   - Returns error (removal path will then 503 upstream)
//
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10WF004_GetDelegateImpactNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	_, err := c.GetDelegateImpact(context.Background(), uuid.New(), uuid.New(), nil)
	require.Error(t, err, "5xx must propagate as an error")
}

// Test Case ID:      P10-WF-005
// Module:            iam-org-membership · WorkflowClient
// Feature:           Timeout enforcement
// API:               GET /api/v1/internal/workflows/active-by-user
// Scenario:          Negative — server sleeps beyond client's timeout
// Preconditions:     server sleeps 300 ms; client timeout=50 ms
// Test Steps:
//  1. Fake server sleeps
//  2. Client with short timeout
//
// Expected Result:
//   - Returns transport error (context.DeadlineExceeded wrap)
//
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10WF005_TimeoutEnforced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"active_workflows":0}`))
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 50*time.Millisecond, nil)
	_, err := c.GetDelegateImpact(context.Background(), uuid.New(), uuid.New(), nil)
	require.Error(t, err, "server latency > timeout must produce transport error")
}

// ═════════════════════════════════════════════════════════════════════════
// ReassignDelegate + CancelByDelegate
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P10-WF-006
// Module:            iam-org-membership · WorkflowClient
// Feature:           ReassignDelegate happy — POST + body shape
// API:               POST /api/v1/internal/workflows/reassign-delegate
// Scenario:          Positive — server accepts, returns 200
// Test Steps:
//  1. Fake server captures method, path, body
//  2. Call ReassignDelegate with old/new user + delegation id
//
// Expected Result:
//   - No error
//   - Server sees POST with tenant_id / old_user_id / new_user_id / delegation_id in body
//
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestP10WF006_ReassignDelegateHappy(t *testing.T) {
	var gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	tenant, old, new := uuid.New(), uuid.New(), uuid.New()
	delID := uuid.New()
	err := c.ReassignDelegate(context.Background(), tenant, old, new, &delID)
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Contains(t, gotBody, tenant.String())
	assert.Contains(t, gotBody, old.String())
	assert.Contains(t, gotBody, new.String())
	assert.Contains(t, gotBody, delID.String())
}

// Test Case ID:      P10-WF-007
// Module:            iam-org-membership · WorkflowClient
// Feature:           WFI-13 · ReassignDelegate fail-open when unconfigured
// Scenario:          Positive — empty baseURL → no error, no call
// Test Steps:
//  1. Client with baseURL=""
//  2. Call ReassignDelegate
//
// Expected Result:
//   - Returns nil (fail-open)
//
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestP10WF007_ReassignDelegateWFI13Fail(t *testing.T) {
	c := NewHTTPClient("", 5*time.Second, nil)
	err := c.ReassignDelegate(context.Background(), uuid.New(), uuid.New(), uuid.New(), nil)
	require.NoError(t, err, "WFI-13: unconfigured Workflow client returns nil for POSTs")
}

// Test Case ID:      P10-WF-008
// Module:            iam-org-membership · WorkflowClient
// Feature:           CancelByDelegate happy path
// API:               POST /api/v1/internal/workflows/cancel-by-delegate
// Scenario:          Positive — 200 response
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10WF008_CancelByDelegateHappy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	err := c.CancelByDelegate(context.Background(), uuid.New(), uuid.New(), nil)
	require.NoError(t, err)
}

// Test Case ID:      P10-WF-009
// Module:            iam-org-membership · WorkflowClient
// Feature:           CancelByDelegate returns error on 5xx
// Scenario:          Negative — server 500
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10WF009_CancelByDelegateNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	err := c.CancelByDelegate(context.Background(), uuid.New(), uuid.New(), nil)
	require.Error(t, err)
}

// ═════════════════════════════════════════════════════════════════════════
// Header propagation — verify x-tenant-id / x-user-id / traceparent
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P10-WF-010
// Module:            iam-org-membership · WorkflowClient
// Feature:           Internal-lane headers set (§18)
// API:               Any workflow call
// Scenario:          Positive — GET forwards x-tenant-id / x-user-id
// Test Steps:
//  1. Fake server captures headers
//  2. Call GetDelegateImpact
//
// Expected Result:
//   - x-tenant-id: <tenant uuid>
//   - x-user-id: iam-system
//
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10WF010_HeadersPropagated(t *testing.T) {
	var gotTenant, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant = r.Header.Get("x-tenant-id")
		gotUser = r.Header.Get("x-user-id")
		_, _ = w.Write([]byte(`{"active_workflows":0}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	tenant := uuid.New()
	_, err := c.GetDelegateImpact(context.Background(), tenant, uuid.New(), nil)
	require.NoError(t, err)
	assert.Equal(t, tenant.String(), gotTenant)
	assert.Equal(t, "iam-system", gotUser)
}

// Test Case ID:      P10-WF-011
// Module:            iam-org-membership · WorkflowClient
// Feature:           JSON response shape parsing — workflow_ids array
// Scenario:          Positive — server returns 3 workflow_ids
// Priority: P2 · Severity: Major · Automation Status: Automated
func TestP10WF011_ParsesWorkflowIDsArray(t *testing.T) {
	ids := []string{uuid.New().String(), uuid.New().String(), uuid.New().String()}
	resp := map[string]any{
		"active_workflows": 3,
		"workflow_ids":     ids,
	}
	body, _ := json.Marshal(resp)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	impact, err := c.GetDelegateImpact(context.Background(), uuid.New(), uuid.New(), nil)
	require.NoError(t, err)
	assert.Equal(t, 3, impact.ActiveWorkflows)
	assert.Len(t, impact.WorkflowIDs, 3)
}

// Test Case ID:      P10-WF-012
// Module:            iam-org-membership · WorkflowClient
// Feature:           Content-Type header for POSTs
// Scenario:          Positive — ReassignDelegate sets Content-Type: application/json
// Priority: P2 · Severity: Minor · Automation Status: Automated
func TestP10WF012_PostSetsContentType(t *testing.T) {
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	err := c.ReassignDelegate(context.Background(), uuid.New(), uuid.New(), uuid.New(), nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(gotCT, "application/json"),
		"POST payload must set Content-Type: application/json (got %q)", gotCT)
}
