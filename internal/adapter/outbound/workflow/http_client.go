// Package workflow is the outbound HTTP client for the Workflow Service.
// Three methods per §18:
//
//   - GetDelegateImpact — "who's on this user's active workflows?" — gates
//     P-8/P-11/P-26 removal (WFI-3 → 409 workflow_resolution_required).
//   - ReassignDelegate — P-26 replace_delegate branch.
//   - CancelByDelegate — P-26 stop_workflows branch.
//
// Timeout via WORKFLOW_TIMEOUT_MS (default 3000). WFI-8: transport error
// or 5xx maps to 503 workflow_service_unavailable at the caller. WFI-13
// suspension-advisory is fail-open — an error means "we don't know, proceed".
package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

// Logger is the structured logging interface this client uses (Warn only).
// *slog.Logger satisfies it directly (existing tests keep working
// unchanged); so does port.SlogStyleLogger, which New() passes in from
// main.go so these warnings flow through the same gincommon-backed sink as
// the rest of the service instead of slog.Default().
type Logger interface {
	Warn(msg string, args ...any)
}

type HTTPClient struct {
	baseURL string
	client  *http.Client
	logger  Logger
}

var _ port.WorkflowClient = (*HTTPClient)(nil)

func NewHTTPClient(baseURL string, timeout time.Duration, logger Logger) *HTTPClient {
	if logger == nil {
		logger = slog.Default()
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &HTTPClient{
		baseURL: baseURL,
		client:  httpx.NewClient(timeout),
		logger:  logger,
	}
}

// New is the composition-root constructor reading env directly.
// Preserves the Phase 2 factory name so existing wiring compiles. log is
// the shared gincommon-backed Logger (may be nil — see port.SlogStyleLogger).
func New(log port.Logger) *HTTPClient {
	baseURL := envOr("WORKFLOW_SERVICE_BASE_URL", "")
	timeout := envDurationMs("WORKFLOW_TIMEOUT_MS", 3*time.Second)
	return NewHTTPClient(baseURL, timeout, port.NewSlogStyleLogger(log))
}

// zeroImpactWhenUnconfigured returns "no active workflows" when baseURL is
// blank. WFI-13 fail-open semantics — dev without a Workflow service must
// not block removal flows.
func (c *HTTPClient) zeroImpactWhenUnconfigured(operation string) *port.DelegateImpact {
	if c.baseURL == "" {
		c.logger.Warn("workflow: baseURL not configured — returning empty impact (WFI-13 fail-open)",
			"operation", operation)
		return &port.DelegateImpact{ActiveWorkflows: 0}
	}
	return nil
}

func (c *HTTPClient) GetDelegateImpact(ctx context.Context, tenantID, userID uuid.UUID, delegationID *uuid.UUID) (*port.DelegateImpact, error) {
	if fallback := c.zeroImpactWhenUnconfigured("GetDelegateImpact"); fallback != nil {
		return fallback, nil
	}
	q := url.Values{
		"tenant_id":        {tenantID.String()},
		"delegate_user_id": {userID.String()},
	}
	if delegationID != nil {
		q.Set("delegation_id", delegationID.String())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/internal/workflows/delegate-impact?%s", c.baseURL, q.Encode()), nil)
	if err != nil {
		return nil, err
	}
	c.setInternalHeaders(req, tenantID)

	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Warn("workflow: GetDelegateImpact transport error", "user_id", userID, "error", err.Error())
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("workflow: GetDelegateImpact returned %d: %s", resp.StatusCode, string(msg))
	}
	var body struct {
		ActiveWorkflows int         `json:"active_workflows"`
		WorkflowIDs     []uuid.UUID `json:"workflow_ids"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("workflow: decode response: %w", err)
	}
	return &port.DelegateImpact{
		ActiveWorkflows: body.ActiveWorkflows,
		WorkflowIDs:     body.WorkflowIDs,
	}, nil
}

func (c *HTTPClient) ReassignDelegate(ctx context.Context, tenantID, oldUserID, newUserID uuid.UUID, delegationID *uuid.UUID) error {
	if c.baseURL == "" {
		c.logger.Warn("workflow: baseURL not configured — no-op",
			"operation", "ReassignDelegate", "old_user_id", oldUserID, "new_user_id", newUserID)
		return nil
	}
	body := map[string]any{
		"tenant_id":       tenantID,
		"old_delegate_id": oldUserID,
		"new_delegate_id": newUserID,
	}
	if delegationID != nil {
		body["delegation_id"] = *delegationID
	}
	return c.postInternal(ctx, tenantID, "/api/v1/internal/workflows/reassign-delegate", body)
}

func (c *HTTPClient) CancelByDelegate(ctx context.Context, tenantID, userID uuid.UUID, delegationID *uuid.UUID) error {
	if c.baseURL == "" {
		c.logger.Warn("workflow: baseURL not configured — no-op",
			"operation", "CancelByDelegate", "user_id", userID)
		return nil
	}
	body := map[string]any{
		"tenant_id":        tenantID,
		"delegate_user_id": userID,
	}
	if delegationID != nil {
		body["delegation_id"] = *delegationID
	}
	return c.postInternal(ctx, tenantID, "/api/v1/internal/workflows/cancel-by-delegate", body)
}

func (c *HTTPClient) postInternal(ctx context.Context, tenantID uuid.UUID, path string, body any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal workflow request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setInternalHeaders(req, tenantID)

	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Warn("workflow: transport error", "path", path, "error", err.Error())
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("workflow: %s returned %d: %s", path, resp.StatusCode, string(msg))
}

func (c *HTTPClient) setInternalHeaders(req *http.Request, tenantID uuid.UUID) {
	req.Header.Set("x-tenant-id", tenantID.String())
	req.Header.Set("x-user-id", "iam-system")
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
