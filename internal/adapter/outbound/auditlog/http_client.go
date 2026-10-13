// Package auditlog is the outbound HTTP adapter for iam-audit-log's AL-5
// (POST /api/v1/internal/audit-entries, D-6 direct-write subset).
//
// This service calls AL-5 for three D-6 entry types it is authorised to
// write: config.tenant_setting.changed (P-2 PATCH), invitation.created
// (P-6), invitation.revoked (P-31), and invitation.expired (reconciler
// invitation-expiry CronJob).
//
// All calls are best-effort and fail-open: the caller logs a warning and
// continues on any error. AUDIT_LOG_TIMEOUT_MS defaults to 3000 ms —
// generous enough to survive a momentary Audit Log blip without holding up
// the calling request.
package auditlog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

const defaultTimeout = 3000 * time.Millisecond

// HTTPClient implements port.AuditLogClient against iam-audit-log's AL-5.
type HTTPClient struct {
	baseURL string
	client  *http.Client
	logger  port.Logger
}

var _ port.AuditLogClient = (*HTTPClient)(nil)

// New reads AUDIT_LOG_BASE_URL and AUDIT_LOG_TIMEOUT_MS from the
// environment, matching every other outbound client's factory convention.
func New(log port.Logger) *HTTPClient {
	baseURL := envOr("AUDIT_LOG_BASE_URL", "")
	timeout := envDurationMs("AUDIT_LOG_TIMEOUT_MS", defaultTimeout)
	if baseURL == "" {
		warn(log, "auditlog: AUDIT_LOG_BASE_URL is not set — AL-5 direct-write calls will be skipped")
	}
	return &HTTPClient{
		baseURL: baseURL,
		client:  httpx.NewClient(timeout),
		logger:  log,
	}
}

// NewHTTPClient constructs an HTTPClient directly (used in tests).
func NewHTTPClient(baseURL string, timeout time.Duration, log port.Logger) *HTTPClient {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &HTTPClient{
		baseURL: baseURL,
		client:  httpx.NewClient(timeout),
		logger:  log,
	}
}

// wireActor mirrors AL-5's actor wire shape.
type wireActor struct {
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	Display string `json:"display,omitempty"`
}

// wireTarget mirrors AL-5's optional target wire shape.
type wireTarget struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

// directWriteRequest is the AL-5 DirectWriteRequest body.
type directWriteRequest struct {
	TenantID        string         `json:"tenant_id"`
	EntryType       string         `json:"entry_type"`
	Action          string         `json:"action"`
	Actor           wireActor      `json:"actor"`
	Target          *wireTarget    `json:"target,omitempty"`
	OccurredAt      time.Time      `json:"occurred_at"`
	SourceService   string         `json:"source_service"`
	SourceEventType string         `json:"source_event_type"`
	IPAddress       string         `json:"ip_address,omitempty"`
	UserAgent       string         `json:"user_agent,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
}

// WriteEntry posts a D-6 entry to AL-5. Returns nil on 200 or 201; any
// other outcome is returned as a non-nil error. Callers are expected to
// discard the error and log a warning (fail-open contract).
func (c *HTTPClient) WriteEntry(ctx context.Context, req port.AuditWriteRequest) error {
	if c.baseURL == "" {
		return nil // no AL-5 configured — silently skip (AUDIT_LOG_BASE_URL unset)
	}

	body := directWriteRequest{
		TenantID:        req.TenantID.String(),
		EntryType:       req.EntryType,
		Action:          req.Action,
		Actor:           wireActor{Type: req.Actor.Type, ID: req.Actor.ID, Display: req.Actor.Display},
		OccurredAt:      req.OccurredAt.UTC(),
		SourceService:   req.SourceService,
		SourceEventType: req.SourceEventType,
		IPAddress:       req.IPAddress,
		UserAgent:       req.UserAgent,
		Metadata:        req.Metadata,
	}
	if req.Target != nil {
		body.Target = &wireTarget{Type: req.Target.Type, ID: req.Target.ID}
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("auditlog: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/v1/internal/audit-entries", bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("auditlog: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", req.IdempotencyKey)
	setInternalHeaders(httpReq, req.TenantID)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("auditlog: WriteEntry transport error: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("auditlog: WriteEntry returned %d for entry_type %s", resp.StatusCode, req.EntryType)
	}
	return nil
}

// setInternalHeaders authenticates as the iam-system internal principal —
// the same trust-boundary convention every other IAM internal route uses.
func setInternalHeaders(req *http.Request, tenantID uuid.UUID) {
	req.Header.Set("x-user-id", "iam-system")
	req.Header.Set("x-tenant-id", tenantID.String())
	req.Header.Set("x-tenant-roles", "iam-system")
}

func warn(log port.Logger, msg string, kv ...any) {
	if log != nil {
		log.Warn(msg, port.Fields(kv...))
	}
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
