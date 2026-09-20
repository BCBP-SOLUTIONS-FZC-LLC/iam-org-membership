// Package delegationcheck is the outbound HTTP client for the standalone
// Delegation Service (iam-delegation), which owns the `delegations` table
// as of ADR-0008 v2 (Option C — this repo drops the table entirely).
//
// One read-only, mesh-only call, replacing the local
// DelegationRepository.FindActiveDeptDelegateForUser lookup
// DeptMembershipService used before the split:
//
//   - DeptDelegate — GET /internal/delegations/dept-delegate (DLG-I3)
//
// DLG-I3 is Core's §8.8.4 department-scope removal precision (WFI-11),
// called synchronously on the admin dept-membership Assign/Remove path
// (iam-lld-delegation-service.md §11.5) — not a hot path, so a generous
// default timeout is fine; DELEGATION_TIMEOUT_MS still lets an operator
// tighten it.
//
// DLG-I3 never 404s — `{"found": false}` is the valid "no active
// department delegation" answer (iam-delegation's internal_handler.go
// DeptDelegate/DeptDelegateResponse). Any transport error or non-2xx
// response is returned as a plain error; per LLD §11.5, "on a Delegation
// outage the gate degrades to tenant-wide impact (still correct, less
// precise)" — that fallback is DeptMembershipService's responsibility
// (log a warning, proceed with a nil delegation id), not this client's.
package delegationcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// xsvcService is this client's platform_dependency_* target_service
// label value (LLD §11.2) — the downstream peer being called, distinct
// from the metric's "service" const label (this service's own identity).
const xsvcService = "delegation"

type HTTPClient struct {
	baseURL string
	client  *http.Client
	logger  port.Logger
}

var _ port.DelegationCheckClient = (*HTTPClient)(nil)

func NewHTTPClient(baseURL string, timeout time.Duration, logger port.Logger) *HTTPClient {
	// Gap-8 fix: raised default from 300ms to 1000ms. 300ms was too tight —
	// under load or cold-start the Delegation Service timed out frequently,
	// causing org_membership to fall back to tenant-wide impact scoping more
	// often than intended. DELEGATION_TIMEOUT_MS can still override this.
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
// gincommon Zap logger (may be nil — warn() is then a no-op).
func New(log port.Logger) *HTTPClient {
	baseURL := envOr("DELEGATION_BASE_URL", "")
	// Gap-8 fix: raised default from 300ms to 1000ms — see NewHTTPClient.
	timeout := envDurationMs("DELEGATION_TIMEOUT_MS", 1000*time.Millisecond)
	if baseURL == "" {
		// Warn at construction time so a missing DELEGATION_BASE_URL is
		// visible in startup logs, not silently discovered on first call.
		warn(log, "delegationcheck: DELEGATION_BASE_URL is not set — dept-scope precision lookup (DLG-I3) will be unavailable; falling back to tenant-wide impact scoping on every removal")
	}
	return NewHTTPClient(baseURL, timeout, log)
}

func warn(log port.Logger, msg string, kv ...any) {
	if log != nil {
		log.Warn(msg, port.Fields(kv...))
	}
}

func (c *HTTPClient) warn(msg string, kv ...any) {
	warn(c.logger, msg, kv...)
}

// deptDelegateResponse mirrors iam-delegation's DeptDelegateResponse DTO
// (internal/adapter/inbound/http/dto.go) exactly: `found` is false (every
// other field omitted) when no active department-scope delegation exists.
type deptDelegateResponse struct {
	Found        bool       `json:"found"`
	DelegationID *uuid.UUID `json:"delegation_id,omitempty"`
	DelegatorID  *uuid.UUID `json:"delegator_id,omitempty"`
	DelegateID   *uuid.UUID `json:"delegate_id,omitempty"`
}

func (c *HTTPClient) DeptDelegate(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*uuid.UUID, error) {
	const endpoint = "dept-delegate"
	if c.baseURL == "" {
		return nil, errors.New("delegationcheck: baseURL not configured — DELEGATION_BASE_URL must be set")
	}
	q := url.Values{}
	q.Set("tenant_id", tenantID.String())
	q.Set("user_id", userID.String())
	q.Set("dept_id", deptID.String())
	reqURL := c.baseURL + "/internal/delegations/dept-delegate?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("delegationcheck: build request: %w", err)
	}
	c.setInternalHeaders(req, tenantID)

	start := time.Now()
	resp, err := c.client.Do(req)
	metrics.ObserveDependencyLatency(xsvcService, endpoint, time.Since(start).Seconds())
	if err != nil {
		metrics.IncDependencyError(xsvcService, endpoint, metrics.DependencyOutcome(err))
		c.warn("delegationcheck: DeptDelegate transport error", "tenant_id", tenantID, "user_id", userID, "dept_id", deptID, "error", err.Error())
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode >= 500 {
			metrics.IncDependencyError(xsvcService, endpoint, "5xx")
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		c.warn("delegationcheck: DeptDelegate non-2xx", "tenant_id", tenantID, "status", resp.StatusCode, "body", string(msg))
		return nil, fmt.Errorf("delegationcheck: DeptDelegate returned %d: %s", resp.StatusCode, string(msg))
	}
	var out deptDelegateResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("delegationcheck: decode response: %w", err)
	}
	if !out.Found || out.DelegationID == nil {
		return nil, nil
	}
	id := *out.DelegationID
	return &id, nil
}

// setInternalHeaders authenticates as the reserved iam-system principal —
// the same "/internal/*" trust-boundary convention every other IAM
// internal route in this stack uses (mesh-only/mTLS; RequireSystemRole is
// defense-in-depth on iam-delegation's side too).
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
