// http_client_transport_test.go — covers the transport-error and nil-metric
// branches that remain uncovered after the existing test suite.
//
// Uncovered branches targeted (from coverage profile):
//
//	Line 143-146  CreateInvitedUser: c.client.Do returns error (transport fail)
//	Line 174-177  DeleteUser:        c.client.Do returns error
//	Line 209-212  PatchRealmConfig:  c.client.Do returns error
//	Lines 237-243 RevokeUserSessions: transport error branch
//	               — metrics.AuthSessionRevokeFailed.WithLabelValues("transport").Inc()
//	               — logger.Warn + return err
//	Lines 249-251 RevokeUserSessions: non-2xx, metrics.AuthSessionRevokeFailed IS nil
//	               (the `if metrics.AuthSessionRevokeFailed != nil` false arm)
//	Line 271-273  ResetMFA: bad-URL request build error
//
// Strategy: all transport errors use a server that is closed before the call
// is made. The metrics.AuthSessionRevokeFailed nil branch is tested without
// registering the metric — the package-level var defaults to nil.
package realmprovisioner

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── helpers ────────────────────────────────────────────────────────────────

// closedServer creates and immediately closes an httptest.Server so any
// connection attempt against its URL will fail with a transport error.
func closedServer() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Close()
	return srv
}

// ─── CreateInvitedUser — transport error ────────────────────────────────────

// Test Case ID: P20-RP-T01
// Feature:      CreateInvitedUser transport error (line 143-146)
// Scenario:     c.client.Do fails → logger.Warn emitted, error returned.
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP20RPT01_CreateInvitedUser_TransportError_LogsAndReturnsError(t *testing.T) {
	srv := closedServer()
	c := NewHTTPClient(srv.URL, 0, slog.Default())
	_, err := c.CreateInvitedUser(context.Background(), port.CreateInvitedUserRequest{
		TenantID: uuid.New(),
		Email:    "a@example.com",
		FullName: "Alice",
	})
	require.Error(t, err, "transport error from c.client.Do must propagate to caller")
}

// ─── DeleteUser — transport error ──────────────────────────────────────────

// Test Case ID: P20-RP-T02
// Feature:      DeleteUser transport error (line 174-177)
// Scenario:     c.client.Do fails → logger.Warn emitted, error returned.
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP20RPT02_DeleteUser_TransportError_LogsAndReturnsError(t *testing.T) {
	srv := closedServer()
	c := NewHTTPClient(srv.URL, 0, slog.Default())
	err := c.DeleteUser(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err, "transport error from DeleteUser c.client.Do must propagate")
}

// ─── PatchRealmConfig — transport error ─────────────────────────────────────

// Test Case ID: P20-RP-T03
// Feature:      PatchRealmConfig transport error (line 209-212)
// Scenario:     c.client.Do fails → logger.Warn emitted, error returned.
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP20RPT03_PatchRealmConfig_TransportError_LogsAndReturnsError(t *testing.T) {
	srv := closedServer()
	enabled := true
	c := NewHTTPClient(srv.URL, 0, slog.Default())
	err := c.PatchRealmConfig(context.Background(), uuid.New(), port.RealmConfigPatch{
		LocalAccountsEnabled: &enabled,
	})
	require.Error(t, err, "transport error from PatchRealmConfig c.client.Do must propagate")
}

// ─── RevokeUserSessions — transport error (with metric increment) ────────────

// Test Case ID: P20-RP-T04
// Feature:      RevokeUserSessions transport error branch (lines 237-243)
//   - metrics.AuthSessionRevokeFailed must be incremented when non-nil
//   - logger.Warn emitted
//   - error returned
//
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP20RPT04_RevokeUserSessions_TransportError_IncrementsMetricAndReturnsError(t *testing.T) {
	// Register a real counter for this test to exercise the non-nil metric branch.
	reg := prometheus.NewRegistry()
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iam_session_revoke_failed_total_transport_test",
		Help: "test counter",
	}, []string{"reason"})
	require.NoError(t, reg.Register(counter))

	original := metrics.AuthSessionRevokeFailed
	metrics.AuthSessionRevokeFailed = counter
	defer func() { metrics.AuthSessionRevokeFailed = original }()

	srv := closedServer()
	c := NewHTTPClient(srv.URL, 0, slog.Default())
	err := c.RevokeUserSessions(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err, "transport error from RevokeUserSessions must propagate (AUTH-8 surface for fail-count)")

	// Verify the metric was incremented with label "transport".
	mfs, metricErr := reg.Gather()
	require.NoError(t, metricErr)
	require.Len(t, mfs, 1)
	require.Len(t, mfs[0].GetMetric(), 1)
	assert.Equal(t, float64(1), mfs[0].GetMetric()[0].GetCounter().GetValue(),
		"transport error must increment SessionRevokeFailed{reason=transport} exactly once")
}

// ─── RevokeUserSessions — non-2xx, nil SessionRevokeFailed metric ────────────

// Test Case ID: P20-RP-T05
// Feature:      RevokeUserSessions non-2xx response when metrics.AuthSessionRevokeFailed
//
//	is nil (lines 249-251: the `if metrics.AuthSessionRevokeFailed != nil`
//	false arm).
//
// When the metric counter is nil (no Prometheus registry bootstrapped),
// the code skips the Inc() call. This branch is separate from the transport
// error branch (T04 above) and only reachable on a successful HTTP
// round-trip that returns a non-2xx status.
//
// Priority: P2 · Severity: Minor · Automation Status: Automated
func TestP20RPT05_RevokeUserSessions_Non2xx_NilMetric_ReturnsError(t *testing.T) {
	// Ensure the package-level var is nil for this test — no Prometheus
	// registration needed. We save/restore to avoid contaminating parallel tests.
	original := metrics.AuthSessionRevokeFailed
	metrics.AuthSessionRevokeFailed = nil
	defer func() { metrics.AuthSessionRevokeFailed = original }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "realm not ready", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0, slog.Default())
	err := c.RevokeUserSessions(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err,
		"non-2xx RevokeUserSessions must surface the error even when the metric counter is nil")
	assert.Contains(t, err.Error(), "503",
		"error message must include the HTTP status code")
}

// ─── RevokeUserSessions — non-2xx WITH non-nil metric ─────────────────────────

// Test Case ID: P20-RP-T05b
// Feature:      RevokeUserSessions non-2xx response when metrics.AuthSessionRevokeFailed
//
//	IS non-nil (lines 249-251: the `if metrics.AuthSessionRevokeFailed != nil`
//	TRUE arm — increments with the HTTP status code as label).
//
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP20RPT05b_RevokeUserSessions_Non2xx_NonNilMetric_IncrementsStatusLabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iam_session_revoke_failed_total_non2xx_test",
		Help: "test counter",
	}, []string{"reason"})
	require.NoError(t, reg.Register(counter))

	original := metrics.AuthSessionRevokeFailed
	metrics.AuthSessionRevokeFailed = counter
	defer func() { metrics.AuthSessionRevokeFailed = original }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0, slog.Default())
	err := c.RevokeUserSessions(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "502")

	// Verify the metric was incremented with the status code as label ("502").
	mfs, metricErr := reg.Gather()
	require.NoError(t, metricErr)
	require.Len(t, mfs, 1, "exactly one metric family expected")
	require.Len(t, mfs[0].GetMetric(), 1, "exactly one label series expected")
	assert.Equal(t, float64(1), mfs[0].GetMetric()[0].GetCounter().GetValue(),
		"non-2xx must increment SessionRevokeFailed{reason=<status>} exactly once")
}

// ─── ResetMFA — request build error (bad base URL) ───────────────────────────

// Test Case ID: P20-RP-T06
// Feature:      ResetMFA request build error (line 271-273)
//
//	http.NewRequestWithContext returns an error when the URL
//	contains a control character.
//
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP20RPT06_ResetMFA_BadBaseURL_RequestBuildError(t *testing.T) {
	// "\x7f" (DEL) is an invalid URL character that makes http.NewRequestWithContext
	// return an error without hitting the network — same pattern used in
	// TestP19RPEdges_CreateInvitedUser_BadBaseURL_RequestBuildError.
	c := NewHTTPClient("http://\x7f", 0, slog.Default())
	err := c.ResetMFA(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err,
		"invalid base URL must cause http.NewRequestWithContext to fail before any network I/O")
}
