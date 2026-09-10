package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

// CatalogService is the read-cutover seam for the departments/plans
// catalogs now owned by catalog-admin-config (ADR-0007 Wave 1,
// migration-runbook Phase 2 "cut over reads"). It implements both
// port.DepartmentCatalogReader and port.PlanCatalogReader by wrapping a
// port.CatalogAdminClient (the raw HTTP call) with a two-tier cache:
//
//   - Primary key (om:departments / om:plans), 600s TTL — matches the
//     TTL catalog-admin-config's own consumers are expected to use
//     (LLD §8). om:plans keeps its pre-existing name; om:departments is new.
//   - Stale-if-error key (om:departments:stale / om:plans:stale), 24h
//     TTL, refreshed opportunistically on every successful client call.
//     Served only when the primary key has expired AND the live call to
//     catalog-admin-config fails (CAT-D4) — this is a genuinely new
//     pattern in this codebase (no prior stale-if-error cache existed).
//
// If both tiers are empty and the client call fails, Departments/Plans
// return domain.ErrCatalogUnavailable — callers decide whether to
// propagate that (department_service.go, provisioning_service.go do) or
// swallow it (authz_service.go does, preserving its pre-existing
// swallow-on-error behavior on the I-8 hot path).
type CatalogService struct {
	client port.CatalogAdminClient
	cache  port.Cache
	log    port.SlogStyleLogger // optional — see WithLogger
}

var (
	_ port.DepartmentCatalogReader = (*CatalogService)(nil)
	_ port.PlanCatalogReader       = (*CatalogService)(nil)
)

func NewCatalogService(client port.CatalogAdminClient, cache port.Cache) *CatalogService {
	return &CatalogService{client: client, cache: cache}
}

// WithLogger injects the shared gincommon-backed Logger so this service's
// stale-if-error fallback warnings flow through the same sink as HTTP/
// consumer/outbound-client logs instead of slog.Default(). Optional — the
// zero value falls back to the top-level slog functions.
func (s *CatalogService) WithLogger(log port.Logger) *CatalogService {
	s.log = port.NewSlogStyleLogger(log)
	return s
}

const (
	catalogCacheTTL      = 600 * time.Second
	catalogStaleCacheTTL = 24 * time.Hour
)

// Departments returns the full global department catalog.
func (s *CatalogService) Departments(ctx context.Context) ([]domain.Department, error) {
	if cached := s.getCachedDepartments(ctx, cacheKeyDepartments()); cached != nil {
		return cached, nil
	}
	depts, err := s.client.Departments(ctx)
	if err != nil {
		if stale := s.getCachedDepartments(ctx, cacheKeyDepartmentsStale()); stale != nil {
			metrics.IncDependencyError("catalog", "departments", "fallback_served")
			s.log.WarnContext(ctx, "catalogadmin: Departments live call failed — serving stale-if-error fallback",
				"error", err.Error())
			return stale, nil
		}
		return nil, domain.NewError(domain.ErrCatalogUnavailable, "catalog service unavailable and no cached department data")
	}
	out := make([]domain.Department, len(depts))
	for i, d := range depts {
		out[i] = domain.Department{
			ID: d.ID, Code: d.Code, Name: d.Name,
			IsSystem: d.IsSystem, IsActive: d.IsActive, RecordVersion: d.RecordVersion,
		}
	}
	s.setCachedDepartments(ctx, out)
	return out, nil
}

// DepartmentByID scans the full catalog for id. Not independently cached
// — the whole catalog is a handful of rows, and Departments() already
// caches the full list (mirrors catalog-admin-config's own design, which
// caches only the whole-catalog blob, never a per-ID key).
func (s *CatalogService) DepartmentByID(ctx context.Context, id uuid.UUID) (*domain.Department, error) {
	all, err := s.Departments(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == id {
			return &all[i], nil
		}
	}
	return nil, domain.NewError(domain.ErrDepartmentNotFound, "department not found")
}

// Plans returns the full three-tier plan catalog.
func (s *CatalogService) Plans(ctx context.Context) ([]domain.Plan, error) {
	if cached := s.getCachedPlans(ctx, cacheKeyPlans()); cached != nil {
		return cached, nil
	}
	plans, err := s.client.Plans(ctx)
	if err != nil {
		if stale := s.getCachedPlans(ctx, cacheKeyPlansStale()); stale != nil {
			metrics.IncDependencyError("catalog", "plans", "fallback_served")
			s.log.WarnContext(ctx, "catalogadmin: Plans live call failed — serving stale-if-error fallback",
				"error", err.Error())
			return stale, nil
		}
		return nil, domain.NewError(domain.ErrCatalogUnavailable, "catalog service unavailable and no cached plan data")
	}
	out := make([]domain.Plan, len(plans))
	for i, p := range plans {
		out[i] = domain.Plan{
			Code: p.Code, DisplayName: p.DisplayName,
			WorkflowTemplateLimit: p.WorkflowTemplateLimit, TenderLimit: p.TenderLimit,
			TrialDurationDays: p.TrialDurationDays, SSOEnabled: p.SSOEnabled,
			CustomBranding: domain.BrandingLevel(p.CustomBranding), FeatureSet: p.FeatureSet,
			RecordVersion: p.RecordVersion,
		}
	}
	s.setCachedPlans(ctx, out)
	return out, nil
}

// PlanByCode scans the full catalog for code. Not independently cached —
// same rationale as DepartmentByID.
func (s *CatalogService) PlanByCode(ctx context.Context, code domain.TenantPlan) (*domain.Plan, error) {
	all, err := s.Plans(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Code == code {
			return &all[i], nil
		}
	}
	return nil, domain.NewError(domain.ErrPlanNotFound, "plan not found")
}

// ── cache helpers ─────────────────────────────────────────────────────

func (s *CatalogService) getCachedDepartments(ctx context.Context, key string) []domain.Department {
	if s.cache == nil {
		return nil
	}
	raw, err := s.cache.Get(ctx, key)
	if err != nil || raw == nil {
		return nil
	}
	var out []domain.Department
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func (s *CatalogService) setCachedDepartments(ctx context.Context, depts []domain.Department) {
	if s.cache == nil {
		return
	}
	raw, err := json.Marshal(depts)
	if err != nil {
		return
	}
	_ = s.cache.Set(ctx, cacheKeyDepartments(), raw, catalogCacheTTL)
	_ = s.cache.Set(ctx, cacheKeyDepartmentsStale(), raw, catalogStaleCacheTTL)
}

func (s *CatalogService) getCachedPlans(ctx context.Context, key string) []domain.Plan {
	if s.cache == nil {
		return nil
	}
	raw, err := s.cache.Get(ctx, key)
	if err != nil || raw == nil {
		return nil
	}
	var out []domain.Plan
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func (s *CatalogService) setCachedPlans(ctx context.Context, plans []domain.Plan) {
	if s.cache == nil {
		return
	}
	raw, err := json.Marshal(plans)
	if err != nil {
		return
	}
	_ = s.cache.Set(ctx, cacheKeyPlans(), raw, catalogCacheTTL)
	_ = s.cache.Set(ctx, cacheKeyPlansStale(), raw, catalogStaleCacheTTL)
}
