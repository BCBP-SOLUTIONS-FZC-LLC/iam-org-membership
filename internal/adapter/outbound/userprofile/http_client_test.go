// Phase 10 — outbound clients · UserProfile.
//
// Module:   iam-org-membership
// Feature:  UserProfileClient — CONS-2 availability-first + DEL-6 pointer-clear
// File:     internal/adapter/outbound/userprofile/http_client.go
//
// Test IDs: P10-UP-NNN.
package userprofile

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═════════════════════════════════════════════════════════════════════════
// SetAvailability — happy path (create OOO)
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P10-UP-001
// Module:            iam-org-membership · UserProfileClient
// Feature:           SetAvailability create (CONS-2 §8.6)
// API:               PUT /api/v1/internal/users/{id}/availability
// Scenario:          Positive — status=ooo + delegate_id set → 200
// Preconditions:     fake UP server up
// Test Steps:
//  1. Fake UP captures method + path + body
//  2. Call SetAvailability(status=ooo, delegate_id=<uuid>)
//
// Expected Result:
//   - No error
//   - Method = PUT
//   - Body contains "status":"ooo", "delegate_id":"<uuid>"
//
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestP10UP001_SetAvailabilityCreate(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	status := "ooo"
	delegate := uuid.New()
	from := time.Now().UTC()
	err := c.SetAvailability(context.Background(), port.SetAvailabilityRequest{
		TenantID: uuid.New(), UserID: uuid.New(),
		Status: &status, OOOFrom: &from, DelegateID: &delegate,
	})
	require.NoError(t, err)
	assert.Equal(t, http.MethodPut, gotMethod)
	assert.Contains(t, gotPath, "/api/v1/internal/users/")
	assert.Contains(t, gotPath, "/availability")
	// Body contains the delegate_id and status.
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(gotBody), &body))
	assert.Equal(t, "ooo", body["status"])
	assert.Equal(t, delegate.String(), body["delegate_id"])
}

// Test Case ID:      P10-UP-002
// Module:            iam-org-membership · UserProfileClient
// Feature:           SetAvailability pointer-clear (§8.7 DEL-6)
// API:               PUT /api/v1/internal/users/{id}/availability
// Scenario:          Positive — ClearDelegate=true sends delegate_id:null literal
// Preconditions:     fake UP server up
// Test Steps:
//  1. Call SetAvailability with ClearDelegate=true (no other fields)
//
// Expected Result:
//   - Body contains "delegate_id":null literal (not omitted)
//
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestP10UP002_PointerClearSemantics(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	err := c.SetAvailability(context.Background(), port.SetAvailabilityRequest{
		TenantID: uuid.New(), UserID: uuid.New(),
		ClearDelegate: true,
	})
	require.NoError(t, err)
	// Raw JSON must contain "delegate_id":null literally, per DEL-6.
	assert.Contains(t, gotBody, `"delegate_id":null`,
		"DEL-6: ClearDelegate must serialise as explicit delegate_id:null (got %s)", gotBody)
}

// Test Case ID:      P10-UP-003
// Module:            iam-org-membership · UserProfileClient
// Feature:           baseURL unset → transport error
// API:               internal
// Scenario:          Negative — baseURL="" surfaces error immediately
// Preconditions:     none
// Test Steps:
//  1. Construct client with baseURL=""
//  2. Call SetAvailability
//
// Expected Result:
//   - Returns error (caller translates to 422 invalid_delegate)
//
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestP10UP003_BaseURLEmptyErrors(t *testing.T) {
	c := NewHTTPClient("", 5*time.Second, slog.Default())
	status := "ooo"
	err := c.SetAvailability(context.Background(), port.SetAvailabilityRequest{
		TenantID: uuid.New(), UserID: uuid.New(),
		Status: &status,
	})
	require.Error(t, err, "empty baseURL must produce an error")
}

// Test Case ID:      P10-UP-004
// Module:            iam-org-membership · UserProfileClient
// Feature:           Non-2xx propagates as error
// Scenario:          Negative — server returns 404 → error
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10UP004_Non2xxErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	status := "ooo"
	err := c.SetAvailability(context.Background(), port.SetAvailabilityRequest{
		TenantID: uuid.New(), UserID: uuid.New(), Status: &status,
	})
	require.Error(t, err)
}

// Test Case ID:      P10-UP-005
// Module:            iam-org-membership · UserProfileClient
// Feature:           Timeout enforcement
// Scenario:          Negative — server slower than client timeout
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10UP005_TimeoutEnforced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, 30*time.Millisecond, slog.Default())
	status := "ooo"
	err := c.SetAvailability(context.Background(), port.SetAvailabilityRequest{
		TenantID: uuid.New(), UserID: uuid.New(), Status: &status,
	})
	require.Error(t, err)
}

// Test Case ID:      P10-UP-006
// Module:            iam-org-membership · UserProfileClient
// Feature:           Internal-lane headers set
// Scenario:          Positive — x-tenant-id / x-user-id headers propagate
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10UP006_HeadersPropagated(t *testing.T) {
	var gotTenant, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant = r.Header.Get("x-tenant-id")
		gotUser = r.Header.Get("x-user-id")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	tenant := uuid.New()
	status := "ooo"
	err := c.SetAvailability(context.Background(), port.SetAvailabilityRequest{
		TenantID: tenant, UserID: uuid.New(), Status: &status,
	})
	require.NoError(t, err)
	assert.Equal(t, tenant.String(), gotTenant)
	assert.Equal(t, "iam-system", gotUser)
}

// Test Case ID:      P10-UP-007
// Module:            iam-org-membership · UserProfileClient
// Feature:           buildBody omits nil fields (three-state DelegateID)
// Scenario:          Positive — DelegateID unset AND ClearDelegate=false → key omitted
// Priority: P2 · Severity: Minor · Automation Status: Automated
func TestP10UP007_BuildBodyOmitsUnsetDelegate(t *testing.T) {
	status := "ooo"
	from := time.Now().UTC()
	body := buildBody(port.SetAvailabilityRequest{
		TenantID: uuid.New(), UserID: uuid.New(),
		Status: &status, OOOFrom: &from,
	})
	_, present := body["delegate_id"]
	assert.False(t, present, "delegate_id must be omitted when neither set nor cleared")
}
