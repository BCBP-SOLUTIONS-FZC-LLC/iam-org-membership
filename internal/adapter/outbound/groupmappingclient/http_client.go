// Package groupmappingclient is the outbound HTTP client for the Group
// Mapping / JIT Config Service (group-mapping-jit-config), which owns
// group_dept_role_mappings/group_tenant_role_mappings/group_dept_mappings
// as of ADR-0007 Wave 2 (Document 3, group-mapping-jit-config-service-lld.md).
//
// One read-only, mesh-only call:
//
//   - ResolveGroups — POST /internal/tenants/{tenant_id}/group-resolution (GM-I1)
//
// Timeout via GROUP_MAPPING_TIMEOUT_MS (default 300ms) — I-10's own
// end-to-end SLO is part of the federated-login flow's ~600-800ms p99
// budget, and this call's own target is <=50ms p99, so 300ms is generous
// headroom, not a tight budget; it exists only so a wedged Group Mapping
// Service can't stall a SAML login past the point where fail-open (§16,
// ADR-0007 Action Item 4) should have already kicked in.
//
// GM-I1 is mesh-only/mTLS-authenticated with no separate credential (the
// same "/internal/*" trust-boundary convention every other IAM internal
// route in this stack uses) but DOES run under the target tenant's GUC
// (group-mapping-jit-config LLD §5.2/§10.4) — the x-tenant-id header
// carries the real tenant being resolved for, matching userprofile's
// SetAvailability header convention, not catalogadmin's uuid.Nil
// placeholder (that service has no tenant concept; this one does).
//
// An unconfigured base URL or any transport/non-2xx failure is returned
// as a plain error — resilience (cache, stale-if-error, fail-open) is
// service.GroupMappingService's responsibility, not this client's.
package groupmappingclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

// xsvcService is this client's iam_xsvc_call_* label value (LLD §11.2).
const xsvcService = "group_mapping"

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

var _ port.GroupMappingClient = (*HTTPClient)(nil)

func NewHTTPClient(baseURL string, timeout time.Duration, logger Logger) *HTTPClient {
	if logger == nil {
		logger = slog.Default()
	}
	if timeout <= 0 {
		timeout = 300 * time.Millisecond
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

// New preserves the sibling clients' factory-name convention so main.go's
// wiring reads the same way for every outbound client. log is the shared
// gincommon-backed Logger (may be nil — see port.SlogStyleLogger).
func New(log port.Logger) *HTTPClient {
	baseURL := envOr("GROUP_MAPPING_BASE_URL", "")
	timeout := envDurationMs("GROUP_MAPPING_TIMEOUT_MS", 300*time.Millisecond)
	return NewHTTPClient(baseURL, timeout, port.NewSlogStyleLogger(log))
}

type groupResolutionRequest struct {
	Groups []string `json:"groups"`
}

// groupResolutionResponse mirrors GM-I1's response shape — empty arrays,
// never null, for any dimension with no match.
type groupResolutionResponse struct {
	DeptMappings []struct {
		KeycloakGroupName string    `json:"keycloak_group_name"`
		DepartmentID      uuid.UUID `json:"department_id"`
	} `json:"dept_mappings"`
	DeptRoleMappings []struct {
		KeycloakGroupName string `json:"keycloak_group_name"`
		RoleCode          string `json:"role_code"`
	} `json:"dept_role_mappings"`
	TenantRoleMappings []struct {
		KeycloakGroupName string `json:"keycloak_group_name"`
		RoleCode          string `json:"role_code"`
	} `json:"tenant_role_mappings"`
}

func (c *HTTPClient) ResolveGroups(ctx context.Context, tenantID uuid.UUID, groups []string) (*port.GroupResolution, error) {
	const endpoint = "group-resolution"
	if c.baseURL == "" {
		return nil, errors.New("groupmappingclient: baseURL not configured — GROUP_MAPPING_BASE_URL must be set")
	}
	buf, err := json.Marshal(groupResolutionRequest{Groups: groups})
	if err != nil {
		return nil, fmt.Errorf("groupmappingclient: marshal request: %w", err)
	}
	url := fmt.Sprintf("%s/internal/tenants/%s/group-resolution", c.baseURL, tenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("groupmappingclient: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setInternalHeaders(req, tenantID)

	start := time.Now()
	resp, err := c.client.Do(req)
	metrics.ObserveXsvcLatency(xsvcService, endpoint, time.Since(start).Seconds())
	if err != nil {
		metrics.IncXsvcError(xsvcService, endpoint, metrics.XsvcOutcome(err))
		c.logger.Warn("groupmappingclient: ResolveGroups transport error", "tenant_id", tenantID, "error", err.Error())
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode >= 500 {
			metrics.IncXsvcError(xsvcService, endpoint, "5xx")
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		c.logger.Warn("groupmappingclient: ResolveGroups non-2xx", "tenant_id", tenantID, "status", resp.StatusCode, "body", string(msg))
		return nil, fmt.Errorf("groupmappingclient: ResolveGroups returned %d: %s", resp.StatusCode, string(msg))
	}
	var out groupResolutionResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("groupmappingclient: decode response: %w", err)
	}

	res := &port.GroupResolution{
		DeptMappings:       make([]port.ResolvedDeptMapping, len(out.DeptMappings)),
		DeptRoleMappings:   make([]port.ResolvedDeptRoleMapping, len(out.DeptRoleMappings)),
		TenantRoleMappings: make([]port.ResolvedTenantRoleMapping, len(out.TenantRoleMappings)),
	}
	for i, d := range out.DeptMappings {
		res.DeptMappings[i] = port.ResolvedDeptMapping{KeycloakGroupName: d.KeycloakGroupName, DepartmentID: d.DepartmentID}
	}
	for i, d := range out.DeptRoleMappings {
		res.DeptRoleMappings[i] = port.ResolvedDeptRoleMapping{KeycloakGroupName: d.KeycloakGroupName, RoleCode: domain.DeptRole(d.RoleCode)}
	}
	for i, t := range out.TenantRoleMappings {
		res.TenantRoleMappings[i] = port.ResolvedTenantRoleMapping{KeycloakGroupName: t.KeycloakGroupName, RoleCode: domain.TenantRoleCode(t.RoleCode)}
	}
	return res, nil
}

// setInternalHeaders authenticates as the reserved iam-system principal.
// Unlike catalogadmin (which has no tenant concept), GM-I1 runs under the
// TARGET tenant's GUC — Core's I-10 call carries the tenant it is
// resolving for, and Group Mapping Service's RLS policy still applies to
// that call exactly as it would to a tenant-facing request (LLD §5.2).
func (c *HTTPClient) setInternalHeaders(req *http.Request, tenantID uuid.UUID) {
	req.Header.Set("x-user-id", "iam-system")
	req.Header.Set("x-tenant-id", tenantID.String())
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
