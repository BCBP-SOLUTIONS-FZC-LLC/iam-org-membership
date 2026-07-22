// Package realmprovisioner is the outbound HTTP client for the Realm
// Provisioner service (§18). Four methods:
//
//   - CreateInvitedUser  — recommend-and-confirm (§16, P-6 seat-hold flow)
//   - DeleteUser         — recommend-and-confirm (§16, PI-9 reconciler)
//   - PatchRealmConfig   — HLD-ratified (HLD §5.2, LLD §16 A7/A58, T-15)
//   - RevokeUserSessions — recommend-and-confirm (§16 A46, AUTH-8 fail-open)
//
// Timeout via REALM_PROVISIONER_TIMEOUT_MS (default 3000).
//
// PatchRealmConfig failure → caller sets tenants.realm_sync_pending=true
// (T-15 Option A local-first + reconcile) and returns 202 to the client.
// RevokeUserSessions is best-effort: any non-2xx increments
// iam_session_revoke_failed_total but does not fail the caller (AUTH-8
// TTL-backstop design).
package realmprovisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

type HTTPClient struct {
	baseURL string
	client  *http.Client
	logger  *slog.Logger
}

var _ port.RealmProvisionerClient = (*HTTPClient)(nil)

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

// New preserves the Phase 2 factory name so existing wiring compiles.
func New() *HTTPClient {
	baseURL := envOr("REALM_PROVISIONER_BASE_URL", "")
	timeout := envDurationMs("REALM_PROVISIONER_TIMEOUT_MS", 3*time.Second)
	return NewHTTPClient(baseURL, timeout, slog.Default())
}

func (c *HTTPClient) CreateInvitedUser(ctx context.Context, req port.CreateInvitedUserRequest) (*port.CreateInvitedUserResponse, error) {
	if c.baseURL == "" {
		// Dev fallback: return a random UUID so the invitation flow can
		// proceed end-to-end without a running RP. Prod deployments set
		// REALM_PROVISIONER_BASE_URL.
		c.logger.Warn("rp: baseURL not configured — returning random KC user id (dev fallback)",
			"email", req.Email)
		return &port.CreateInvitedUserResponse{KeycloakUserID: uuid.New()}, nil
	}
	body := map[string]any{
		"tenant_id": req.TenantID,
		"email":     req.Email,
		"full_name": req.FullName,
	}
	buf, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/v1/internal/users/invite", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.setInternalHeaders(httpReq, req.TenantID)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		c.logger.Warn("rp: CreateInvitedUser transport error", "email", req.Email, "error", err.Error())
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("rp: CreateInvitedUser returned %d: %s", resp.StatusCode, string(msg))
	}
	var out struct {
		KeycloakUserID uuid.UUID `json:"keycloak_user_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return &port.CreateInvitedUserResponse{KeycloakUserID: out.KeycloakUserID}, nil
}

func (c *HTTPClient) DeleteUser(ctx context.Context, tenantID, keycloakUserID uuid.UUID) error {
	if c.baseURL == "" {
		c.logger.Warn("rp: baseURL not configured — DeleteUser no-op (dev)")
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		fmt.Sprintf("%s/api/v1/internal/users/%s", c.baseURL, keycloakUserID), nil)
	if err != nil {
		return err
	}
	c.setInternalHeaders(req, tenantID)

	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Warn("rp: DeleteUser transport error", "keycloak_user_id", keycloakUserID, "error", err.Error())
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	// PI-9 idempotent: 404 treated as success.
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("rp: DeleteUser returned %d: %s", resp.StatusCode, string(msg))
}

func (c *HTTPClient) PatchRealmConfig(ctx context.Context, tenantID uuid.UUID, patch port.RealmConfigPatch) error {
	if c.baseURL == "" {
		c.logger.Warn("rp: baseURL not configured — PatchRealmConfig no-op (dev)")
		return nil
	}
	body := map[string]any{}
	if patch.LocalAccountsEnabled != nil {
		body["local_accounts_enabled"] = *patch.LocalAccountsEnabled
	}
	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		fmt.Sprintf("%s/api/v1/internal/tenants/%s/realm-config", c.baseURL, tenantID), bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setInternalHeaders(req, tenantID)

	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Warn("rp: PatchRealmConfig transport error", "tenant_id", tenantID, "error", err.Error())
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("rp: PatchRealmConfig returned %d: %s", resp.StatusCode, string(msg))
}

// RevokeUserSessions is AUTH-8 fail-open. Any non-2xx increments
// iam_session_revoke_failed_total; the caller relies on the access-token
// TTL (5 min default) + 300 s membership cache eviction as backstop.
func (c *HTTPClient) RevokeUserSessions(ctx context.Context, tenantID, keycloakUserID uuid.UUID) error {
	if c.baseURL == "" {
		c.logger.Warn("rp: baseURL not configured — RevokeUserSessions no-op (dev, TTL backstop)")
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/internal/users/%s/revoke-sessions", c.baseURL, keycloakUserID), nil)
	if err != nil {
		return err
	}
	c.setInternalHeaders(req, tenantID)

	resp, err := c.client.Do(req)
	if err != nil {
		if metrics.SessionRevokeFailed != nil {
			metrics.SessionRevokeFailed.WithLabelValues("transport").Inc()
		}
		c.logger.Warn("rp: RevokeUserSessions transport error — fail-open",
			"keycloak_user_id", keycloakUserID, "error", err.Error())
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if metrics.SessionRevokeFailed != nil {
		metrics.SessionRevokeFailed.WithLabelValues(fmt.Sprintf("%d", resp.StatusCode)).Inc()
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	c.logger.Warn("rp: RevokeUserSessions non-2xx — fail-open",
		"keycloak_user_id", keycloakUserID, "status", resp.StatusCode, "body", string(msg))
	return fmt.Errorf("rp: RevokeUserSessions returned %d", resp.StatusCode)
}

func (c *HTTPClient) setInternalHeaders(req *http.Request, tenantID uuid.UUID) {
	req.Header.Set("x-tenant-id", tenantID.String())
	req.Header.Set("x-user-id", "iam-system")
	req.Header.Set("x-tenant-roles", "iam-system")
	propagateTraceparent(req.Context(), req)
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
