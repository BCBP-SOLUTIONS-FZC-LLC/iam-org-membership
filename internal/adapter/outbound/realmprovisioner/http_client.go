// Package realmprovisioner is the outbound HTTP client for the Realm
// Provisioner service (§18). Five methods:
//
//   - CreateInvitedUser  — recommend-and-confirm (§16, P-6 seat-hold flow)
//   - DeleteUser         — recommend-and-confirm (§16, PI-9 reconciler)
//   - PatchRealmConfig   — HLD-ratified (HLD §5.2, LLD §16 A7/A58, T-15)
//   - RevokeUserSessions — recommend-and-confirm (§16 A46, AUTH-8 fail-open)
//   - ResetMFA           — RP-9, LLD §16 OQ-8 (F6 of the RP↔O&M alignment
//     review): confirmed O&M-initiated, but not yet wired to a public
//     endpoint or added to port.RealmProvisionerClient — deliberately a
//     method on *HTTPClient only, not the interface, until the "Reset MFA"
//     admin feature is actually scheduled (RP has agreed it's fine to hold).
//
// F8 of the RP↔O&M alignment review: every route below is versioned under
// internalAPIBase ("/api/v1/internal"), built from one central helper
// (url()) rather than repeating the prefix per method. REALM_PROVISIONER_
// BASE_URL is host-only (e.g. http://iam-realm-provisioner.iam.svc.cluster.
// local) — the version+internal-API segment is never part of the env var.
//
// Timeout via REALM_PROVISIONER_TIMEOUT_MS (default 3000).
//
// PatchRealmConfig failure → caller sets tenants.realm_sync_pending=true
// (T-15 Option A local-first + reconcile) and returns 202 to the client.
// RevokeUserSessions is best-effort: any non-2xx increments
// iam_auth_session_revoke_failed_total but does not fail the
// caller (AUTH-8 TTL-backstop design).
package realmprovisioner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

// internalAPIBase is the single source of truth for RP's versioned internal
// API prefix (F8, RP↔O&M alignment review — RP LLD v0.22 aligned all
// internal business endpoints under this prefix). Every RealmProvisionerClient
// route is built via (*HTTPClient).url(path), never by hand-appending this
// string per method, so a future prefix change (or a future RP client
// method) only ever touches one place.
const internalAPIBase = "/api/v1/internal"

// url builds a fully-qualified RP request URL: baseURL + internalAPIBase +
// path. path must start with "/" (e.g. "/tenants/"+tenantID.String()+"/users").
func (c *HTTPClient) url(path string) string {
	return c.baseURL + internalAPIBase + path
}

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

var _ port.RealmProvisionerClient = (*HTTPClient)(nil)

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

// New preserves the Phase 2 factory name so existing wiring compiles. log
// is the shared gincommon-backed Logger (may be nil — see
// port.SlogStyleLogger).
func New(log port.Logger) *HTTPClient {
	baseURL := envOr("REALM_PROVISIONER_BASE_URL", "")
	timeout := envDurationMs("REALM_PROVISIONER_TIMEOUT_MS", 3*time.Second)
	return NewHTTPClient(baseURL, timeout, port.NewSlogStyleLogger(log))
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
		"email":     req.Email,
		"full_name": req.FullName,
	}
	if len(req.RequiredActions) > 0 {
		// RP-5, F5: applied verbatim — O&M is the one side that knows the
		// invite's initial_tenant_roles/dept_mappings, so it decides here,
		// not RP.
		body["required_actions"] = req.RequiredActions
	}
	buf, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.url("/tenants/"+req.TenantID.String()+"/users"), bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.setInternalHeaders(httpReq, req.TenantID)
	// RP-5 requires Idempotency-Key (RequireIdempotencyKey — 400
	// missing_idempotency_key otherwise). Derived deterministically from
	// (tenant_id, email) rather than a fresh UUID per call, so a network-
	// level retry of the SAME invite reuses RP's stored result (IDEMP-3)
	// instead of risking a second Keycloak user create.
	httpReq.Header.Set("Idempotency-Key", createInvitedUserIdempotencyKey(req.TenantID, req.Email))

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
		c.url("/tenants/"+tenantID.String()+"/users/"+keycloakUserID.String()), nil)
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
		c.url("/tenants/"+tenantID.String()+"/realm-config"), bytes.NewReader(buf))
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
// iam_auth_session_revoke_failed_total; the caller relies on the
// access-token TTL (5 min default) + 300 s membership cache eviction as
// backstop.
func (c *HTTPClient) RevokeUserSessions(ctx context.Context, tenantID, keycloakUserID uuid.UUID) error {
	if c.baseURL == "" {
		c.logger.Warn("rp: baseURL not configured — RevokeUserSessions no-op (dev, TTL backstop)")
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.url("/tenants/"+tenantID.String()+"/users/"+keycloakUserID.String()+"/revoke-sessions"), nil)
	if err != nil {
		return err
	}
	c.setInternalHeaders(req, tenantID)

	resp, err := c.client.Do(req)
	if err != nil {
		if metrics.AuthSessionRevokeFailed != nil {
			metrics.AuthSessionRevokeFailed.WithLabelValues("transport").Inc()
		}
		c.logger.Warn("rp: RevokeUserSessions transport error — fail-open",
			"keycloak_user_id", keycloakUserID, "error", err.Error())
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if metrics.AuthSessionRevokeFailed != nil {
		metrics.AuthSessionRevokeFailed.WithLabelValues(fmt.Sprintf("%d", resp.StatusCode)).Inc()
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	c.logger.Warn("rp: RevokeUserSessions non-2xx — fail-open",
		"keycloak_user_id", keycloakUserID, "status", resp.StatusCode, "body", string(msg))
	return fmt.Errorf("rp: RevokeUserSessions returned %d", resp.StatusCode)
}

// ResetMFA calls RP-9 (LLD §16 OQ-8, F6 of the RP↔O&M alignment review).
// RP confirmed this is O&M-initiated (a tenant-admin user-management
// action requiring O&M's authorization + audit, neither of which RP can
// decide) — P-34 is that trigger. Fail-closed: a non-2xx or transport
// error is returned to the caller (service.MembershipService.ResetUserMFA
// maps it to realm_provisioner_unavailable, 503), never swallowed.
func (c *HTTPClient) ResetMFA(ctx context.Context, tenantID, keycloakUserID uuid.UUID) error {
	if c.baseURL == "" {
		c.logger.Warn("rp: baseURL not configured — ResetMFA no-op (dev)")
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.url("/tenants/"+tenantID.String()+"/users/"+keycloakUserID.String()+"/mfa-reset"), nil)
	if err != nil {
		return err
	}
	c.setInternalHeaders(req, tenantID)

	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Warn("rp: ResetMFA transport error", "keycloak_user_id", keycloakUserID, "error", err.Error())
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("rp: ResetMFA returned %d: %s", resp.StatusCode, string(msg))
}

// createInvitedUserIdempotencyKey derives a stable RP-5 Idempotency-Key from
// (tenantID, email) — hashed rather than the raw email so the header value
// is always short, ASCII, and free of any character an email address could
// legally contain.
func createInvitedUserIdempotencyKey(tenantID uuid.UUID, email string) string {
	sum := sha256.Sum256([]byte(tenantID.String() + ":" + email))
	return fmt.Sprintf("invite-%x", sum)
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
