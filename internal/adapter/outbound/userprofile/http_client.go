// Package userprofile is the outbound HTTP client for the iam-user-profile
// service (LLD §18, UP LLD §5.3 row 18). Delegation coordination uses
// SetAvailability with two variants:
//
//   - Create: {status:"ooo", ooo_from, ooo_until, delegate_id}
//   - End:    {delegate_id: null} — pointer-clear only (DEL-6);
//     NEVER {status:"available"} (UP owns that transition via
//     its own OOO sweep).
//
// Timeout via USER_PROFILE_TIMEOUT_MS (default 3000). Propagates OTel
// traceparent so UP-side spans link to O&M's originating span.
//
// Availability-first ordering (§8.6 CONS-2): the caller invokes this
// BEFORE inserting the delegations row + emitting DelegationStarted. On
// non-2xx return the caller returns 422 invalid_delegate to the user;
// the delegation row is never written and no outbox event is enqueued.
package userprofile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

// HTTPClient is the real Phase 6 implementation. Constructed with a base
// URL + timeout; safe to share across goroutines.
type HTTPClient struct {
	baseURL string
	client  *http.Client
	logger  *slog.Logger
}

var _ port.UserProfileClient = (*HTTPClient)(nil)

// NewHTTPClient constructs a real HTTP client. Passing baseURL="" yields
// a client that returns transport errors on every call — used by the
// composition root when USER_PROFILE_SERVICE_BASE_URL is unset in dev
// (fail-safe: dev delegation flow still returns 422 invalid_delegate).
func NewHTTPClient(baseURL string, timeout time.Duration, logger *slog.Logger) *HTTPClient {
	if logger == nil {
		logger = slog.Default()
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &HTTPClient{
		baseURL: baseURL,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				IdleConnTimeout:       30 * time.Second,
				MaxIdleConnsPerHost:   8,
				ResponseHeaderTimeout: timeout,
			},
		},
		logger: logger,
	}
}

// SetAvailability calls PUT /api/v1/internal/users/:id/availability on UP.
//
// Two payload shapes (per UP LLD §5.3 row 18):
//   - ClearDelegate=true → {delegate_id: null} literal (§8.7 pointer-clear)
//   - Otherwise → {status, ooo_from, ooo_until, delegate_id} (create/refresh)
//
// Returns nil on 2xx. On any non-2xx or transport error, caller interprets
// per its own flow (delegation create → 422; expiry cron → defer per DEL-6).
func (c *HTTPClient) SetAvailability(ctx context.Context, req port.SetAvailabilityRequest) error {
	if c.baseURL == "" {
		return fmt.Errorf("userprofile: baseURL not configured")
	}
	body := buildBody(req)
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal availability request: %w", err)
	}
	url := fmt.Sprintf("%s/api/v1/internal/users/%s/availability", c.baseURL, req.UserID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build availability request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-tenant-id", req.TenantID.String())
	httpReq.Header.Set("x-user-id", "iam-system")
	httpReq.Header.Set("x-tenant-roles", "iam-system")
	propagateTraceparent(ctx, httpReq)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		c.logger.Warn("userprofile: SetAvailability transport error", "user_id", req.UserID, "error", err.Error())
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	c.logger.Warn("userprofile: SetAvailability non-2xx",
		"user_id", req.UserID, "status", resp.StatusCode, "body", string(msg))
	if resp.StatusCode == 422 {
		// Check if the body contains delegate_unavailable (§16 A65)
		if bytes.Contains(msg, []byte("delegate_unavailable")) {
			return &upError{code: "delegate_unavailable"}
		}
	}
	return fmt.Errorf("userprofile: SetAvailability returned %d", resp.StatusCode)
}

// upError carries a machine-readable error code from the User Profile service.
type upError struct{ code string }

func (e *upError) Error() string { return "userprofile: " + e.code }

// buildBody constructs the wire JSON. delegate_id carries three states:
//
//	{ "delegate_id": null } — explicit clear (ClearDelegate=true)
//	{ "delegate_id": "<uuid>" } — set to specific user (DelegateID != nil)
//	// (omitted) — DelegateID nil && !ClearDelegate → don't touch pointer
func buildBody(req port.SetAvailabilityRequest) map[string]any {
	m := map[string]any{}
	if req.Status != nil {
		m["status"] = *req.Status
	}
	if req.OOOFrom != nil {
		m["ooo_from"] = req.OOOFrom.UTC().Format(time.RFC3339)
	}
	if req.OOOUntil != nil {
		m["ooo_until"] = req.OOOUntil.UTC().Format(time.RFC3339)
	}
	if req.Note != "" {
		m["note"] = req.Note
	}
	switch {
	case req.ClearDelegate:
		m["delegate_id"] = nil
	case req.DelegateID != nil && *req.DelegateID != uuid.Nil:
		m["delegate_id"] = req.DelegateID.String()
	}
	return m
}

// New retains the Phase 2 stub name so existing callers compile. Deprecated
// in favor of NewHTTPClient(baseURL, timeout, logger) at composition time.
// Kept as a thin alias that reads the env var and constructs the real client.
func New() *HTTPClient {
	baseURL := getenv("USER_PROFILE_SERVICE_BASE_URL", "")
	timeout := getenvDuration("USER_PROFILE_TIMEOUT_MS", 3*time.Second)
	return NewHTTPClient(baseURL, timeout, slog.Default())
}
