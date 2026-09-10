// Package catalogadmin is the outbound HTTP client for the Catalog /
// Admin Config Service (catalog-admin-config), which owns the global
// departments/plans catalogs as of ADR-0007 Wave 1. Two read-only,
// mesh-only bulk methods:
//
//   - Departments — GET /api/v1/internal/departments (CAT-I1)
//   - Plans       — GET /api/v1/internal/plans        (CAT-I2)
//
// Timeout via CATALOG_ADMIN_TIMEOUT_MS (default 3000).
//
// Unlike the sibling outbound clients in this package family
// (realmprovisioner, userprofile, workflow), an unconfigured base URL is
// NOT a fail-open condition here — fabricating department/plan data would
// be a worse failure mode than erroring. Both methods return a plain
// error when CATALOG_ADMIN_BASE_URL is unset; resilience comes from
// service.CatalogService's cache + stale-if-error layer, not from this
// client inventing a safe default.
package catalogadmin

import (
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

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

// xsvcService is this client's platform_dependency_* target_service
// label value (LLD §11.2) — the downstream peer being called, distinct
// from the metric's "service" const label (this service's own identity).
const xsvcService = "catalog"

type HTTPClient struct {
	baseURL string
	client  *http.Client
	logger  Logger
}

// Logger is the structured logging interface this client uses (Warn only).
// *slog.Logger satisfies it directly (existing tests keep working
// unchanged); so does port.SlogStyleLogger, which New() passes in from
// main.go so these warnings flow through the same gincommon-backed sink as
// the rest of the service instead of slog.Default().
type Logger interface {
	Warn(msg string, args ...any)
}

var _ port.CatalogAdminClient = (*HTTPClient)(nil)

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

// New preserves the sibling clients' factory-name convention so main.go's
// wiring reads the same way for every outbound client. log is the shared
// gincommon-backed Logger (may be nil — see port.SlogStyleLogger).
func New(log port.Logger) *HTTPClient {
	baseURL := envOr("CATALOG_ADMIN_BASE_URL", "")
	timeout := envDurationMs("CATALOG_ADMIN_TIMEOUT_MS", 3*time.Second)
	return NewHTTPClient(baseURL, timeout, port.NewSlogStyleLogger(log))
}

// catalogDepartmentsResponse mirrors catalog-admin-config's
// InternalDepartmentsResponse DTO (CAT-I1).
type catalogDepartmentsResponse struct {
	Departments []struct {
		ID            uuid.UUID `json:"id"`
		Code          string    `json:"code"`
		Name          string    `json:"name"`
		IsSystem      bool      `json:"is_system"`
		IsActive      bool      `json:"is_active"`
		RecordVersion int64     `json:"record_version"`
	} `json:"departments"`
	AsOf string `json:"as_of"`
}

// catalogPlansResponse mirrors catalog-admin-config's
// InternalPlansResponse DTO (CAT-I2).
type catalogPlansResponse struct {
	Plans []struct {
		Code                  string         `json:"code"`
		DisplayName           string         `json:"display_name"`
		WorkflowTemplateLimit *int           `json:"workflow_template_limit"`
		TenderLimit           *int           `json:"tender_limit"`
		TrialDurationDays     int            `json:"trial_duration_days"`
		SSOEnabled            bool           `json:"sso_enabled"`
		CustomBranding        string         `json:"custom_branding"`
		FeatureSet            map[string]any `json:"feature_set"`
		RecordVersion         int64          `json:"record_version"`
	} `json:"plans"`
	RecordVersions map[string]int64 `json:"record_versions"`
}

func (c *HTTPClient) Departments(ctx context.Context) ([]port.CatalogDepartment, error) {
	const endpoint = "departments"
	if c.baseURL == "" {
		return nil, errors.New("catalogadmin: baseURL not configured — CATALOG_ADMIN_BASE_URL must be set")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/v1/internal/departments", nil)
	if err != nil {
		return nil, err
	}
	c.setInternalHeaders(req)

	start := time.Now()
	resp, err := c.client.Do(req)
	metrics.ObserveDependencyLatency(xsvcService, endpoint, time.Since(start).Seconds())
	if err != nil {
		metrics.IncDependencyError(xsvcService, endpoint, metrics.DependencyOutcome(err))
		c.logger.Warn("catalogadmin: Departments transport error", "error", err.Error())
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode >= 500 {
			metrics.IncDependencyError(xsvcService, endpoint, "5xx")
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("catalogadmin: Departments returned %d: %s", resp.StatusCode, string(msg))
	}
	var out catalogDepartmentsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	depts := make([]port.CatalogDepartment, len(out.Departments))
	for i, d := range out.Departments {
		depts[i] = port.CatalogDepartment{
			ID: d.ID, Code: d.Code, Name: d.Name,
			IsSystem: d.IsSystem, IsActive: d.IsActive, RecordVersion: d.RecordVersion,
		}
	}
	return depts, nil
}

func (c *HTTPClient) Plans(ctx context.Context) ([]port.CatalogPlan, error) {
	const endpoint = "plans"
	if c.baseURL == "" {
		return nil, errors.New("catalogadmin: baseURL not configured — CATALOG_ADMIN_BASE_URL must be set")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/v1/internal/plans", nil)
	if err != nil {
		return nil, err
	}
	c.setInternalHeaders(req)

	start := time.Now()
	resp, err := c.client.Do(req)
	metrics.ObserveDependencyLatency(xsvcService, endpoint, time.Since(start).Seconds())
	if err != nil {
		metrics.IncDependencyError(xsvcService, endpoint, metrics.DependencyOutcome(err))
		c.logger.Warn("catalogadmin: Plans transport error", "error", err.Error())
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode >= 500 {
			metrics.IncDependencyError(xsvcService, endpoint, "5xx")
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("catalogadmin: Plans returned %d: %s", resp.StatusCode, string(msg))
	}
	var out catalogPlansResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	plans := make([]port.CatalogPlan, len(out.Plans))
	for i, p := range out.Plans {
		plans[i] = port.CatalogPlan{
			Code: domain.TenantPlan(p.Code), DisplayName: p.DisplayName,
			WorkflowTemplateLimit: p.WorkflowTemplateLimit, TenderLimit: p.TenderLimit,
			TrialDurationDays: p.TrialDurationDays, SSOEnabled: p.SSOEnabled,
			CustomBranding: p.CustomBranding, FeatureSet: p.FeatureSet,
			RecordVersion: p.RecordVersion,
		}
	}
	return plans, nil
}

// setInternalHeaders authenticates as the reserved iam-system principal
// (RLS-5/IAPI-2-equivalent on the catalog-admin-config side). That
// service has no tenant concept, but its identity-bridge middleware still
// requires a well-formed UUID in x-tenant-id — uuid.Nil is the
// placeholder value, ignored server-side.
func (c *HTTPClient) setInternalHeaders(req *http.Request) {
	req.Header.Set("x-user-id", "iam-system")
	req.Header.Set("x-tenant-id", uuid.Nil.String())
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
