package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
)

// DepartmentCatalogReader is the domain-typed, cache-aware read surface
// over the global department catalog now owned by catalog-admin-config
// (ADR-0007 Wave 1, migration-runbook Phase 2). Implemented by
// service.CatalogService, which wraps CatalogAdminClient + Cache with a
// primary/stale-if-error two-tier cache (LLD §8, CAT-D4). Consumers that
// only need department lookups (DepartmentService, DeptMembershipService)
// depend on this narrower interface rather than the combined
// CatalogAdminClient, so their test fakes only need two methods.
type DepartmentCatalogReader interface {
	Departments(ctx context.Context) ([]domain.Department, error)
	DepartmentByID(ctx context.Context, id uuid.UUID) (*domain.Department, error)
}

// PlanCatalogReader is the domain-typed, cache-aware read surface over the
// global plan catalog, same rationale as DepartmentCatalogReader.
type PlanCatalogReader interface {
	Plans(ctx context.Context) ([]domain.Plan, error)
	PlanByCode(ctx context.Context, code domain.TenantPlan) (*domain.Plan, error)
}
