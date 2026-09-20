// Package tokenservice is the outbound HTTP client for the standalone
// Token Service (iam-token-service), which owns the `service_account_principals`
// table — the registry of each tenant's `platform-automation` automation
// principal (HLD §5.8/§11.7, Realm Provisioner §2.5/RP-17).
//
// One read-only, mesh-only call:
//
//   - IsServiceAccount — GET /internal/tenants/:id/service-accounts?principal_sub=<uuid> (TS-5)
//
// TS-5 is AUTH-9's defense-in-depth check on the membership-create (P-6/I-3)
// and role-grant (P-10/P-28) paths (iam-lld-org-membership-service.md §10,
// AUTH-9) — not a hot path (invite/reconcile-roles frequency, not per-request
// auth), so a generous default timeout is fine.
//
// TS-5 404s (principal_not_found) for the overwhelming majority of calls —
// almost every subject checked is a real human user, not the tenant's
// automation principal. That 404 is the expected "not a service account"
// answer, not an error. Any transport error or non-2xx-non-404 response is
// returned as a plain error; per AUTH-9's framing as defense-in-depth on top
// of the structural composite-FK bar (TR-8/DM-4), a Token Service outage
// degrades to allowing the operation (fail-open) rather than blocking a
// legitimate invite/grant — that fallback is each caller service's
// responsibility (log a warning, proceed), not this client's.
package tokenservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

// xsvcService is this client's platform_dependency_* target_service label
// value (LLD §11.2) — the downstream peer being called, distinct from the
// metric's "service" const label (this service's own identity).
const xsvcService = "token_service"

// Logger is the structured logging interface this client uses (Warn only).
// *slog.Logger satisfies it directly; so does port.SlogStyleLogger, which
// New() passes in from main.go so these warnings flow through the same
// gincommon-backed sink as the rest of the service instead of slog.Default().
type Logger interface {
	Warn(msg string, args ...any)
}

type HTTPClient struct {
	baseURL string
	client  *http.Client
	logger  Logger
}

var _ port.TokenServiceClient = (*HTTPClient)(nil)

func NewHTTPClient(baseURL string, timeout time.Duration, logger Logger) *HTTPClient {
	if logger == nil {
		logger = slog.Default()
	}
	if timeout <= 0 {
		timeout = 1000 * time.Millisecond
	}
	return &HTTPClient{
		baseURL: baseURL,
		client:  httpx.NewClient(timeout),
		logger:  logger,
	}
}

// New preserves the sibling clients' factory-name convention so main.go's
// wiring reads the same way for every outbound client. log is the shared
// gincommon-backed Logger (may be nil — see port.SlogStyleLogger).
func New(log port.Logger) *HTTPClient {
	baseURL := envOr("TOKEN_SERVICE_BASE_URL", "")
	timeout := envDurationMs("TOKEN_SERVICE_TIMEOUT_MS", 1000*time.Millisecond)
	if baseURL == "" {
		// Warn at construction time so a missing TOKEN_SERVICE_BASE_URL is
		// visible in startup logs, not silently discovered on first call.
		slog.Default().Warn("tokenservice: TOKEN_SERVICE_BASE_URL is not set — AUTH-9 service-account check will be unavailable; every Invite/Assign/ReconcileRoles call degrades to allowing the operation (the structural composite-FK bar still holds)")
	}
	return NewHTTPClient(baseURL, timeout, port.NewSlogStyleLogger(log))
}

// principalResponseBody mirrors iam-token-service's principalResponseBody
// DTO (internal/adapter/inbound/http/principal_handler.go) — only the
// fields this client actually needs.
type principalResponseBody struct {
	PrincipalID uuid.UUID `json:"principal_id"`
}

func (c *HTTPClient) IsServiceAccount(ctx context.Context, tenantID, userID uuid.UUID) (bool, error) {
	const endpoint = "service-accounts"
	if c.baseURL == "" {
		return false, errors.New("tokenservice: baseURL not configured — TOKEN_SERVICE_BASE_URL must be set")
	}
	q := url.Values{}
	q.Set("principal_sub", userID.String())
	reqURL := c.baseURL + "/internal/tenants/" + tenantID.String() + "/service-accounts?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return false, fmt.Errorf("tokenservice: build request: %w", err)
	}
	c.setInternalHeaders(req, tenantID)

	start := time.Now()
	resp, err := c.client.Do(req)
	metrics.ObserveDependencyLatency(xsvcService, endpoint, time.Since(start).Seconds())
	if err != nil {
		metrics.IncDependencyError(xsvcService, endpoint, metrics.DependencyOutcome(err))
		c.logger.Warn("tokenservice: IsServiceAccount transport error", "tenant_id", tenantID, "user_id", userID, "error", err.Error())
		return false, err
	}
	defer resp.Body.Close() //nolint:errcheck

	// 404 principal_not_found is the expected, common answer — not an error.
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode >= 500 {
			metrics.IncDependencyError(xsvcService, endpoint, "5xx")
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		c.logger.Warn("tokenservice: IsServiceAccount non-2xx", "tenant_id", tenantID, "status", resp.StatusCode, "body", string(msg))
		return false, fmt.Errorf("tokenservice: IsServiceAccount returned %d: %s", resp.StatusCode, string(msg))
	}
	var out principalResponseBody
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return false, fmt.Errorf("tokenservice: decode response: %w", err)
	}
	return true, nil
}

// setInternalHeaders authenticates as the reserved iam-system principal —
// the same "/internal/*" trust-boundary convention every other IAM internal
// route in this stack uses (mesh-only/mTLS; RequireSystemRole is
// defense-in-depth on Token Service's side too).
func (c *HTTPClient) setInternalHeaders(req *http.Request, tenantID uuid.UUID) {
	req.Header.Set("x-user-id", "iam-system")
	req.Header.Set("x-tenant-id", tenantID.String())
	req.Header.Set("x-tenant-roles", "iam-system")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDurationMs(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}
