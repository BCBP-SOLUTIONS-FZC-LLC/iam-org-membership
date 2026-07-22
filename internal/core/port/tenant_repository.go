package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
)

// TenantRepository owns reads/writes on the tenants row for the current
// RLS-scoped tenant. Under normal calls RLS restricts visibility to the
// caller's tenant (the tenants policy uses `id = current_setting`, so
// FindByID always returns the caller's own row when it exists).
type TenantRepository interface {
	// FindByID returns the tenant row matching id. If RLS blocks the row
	// (caller is not the same tenant), returns domain.ErrTenantNotFound
	// wrapped in a DomainError.
	FindByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)

	// Update applies patch under optimistic-locking (CONC-1..4) inside the
	// caller's transaction. On WHERE record_version mismatch, returns
	// ErrOptimisticLockConflict wrapped in DomainError with Details
	// containing the current record_version + updated_at (CONC-4).
	Update(ctx context.Context, id uuid.UUID, patch *domain.TenantPatch) (*domain.Tenant, error)

	// Insert creates a new tenant row (used by I-1 in Phase 4 and by the
	// TrialTenantProvisioned consumer in Phase 3). Phase 2 exposes it for
	// admin-side seeding paths only.
	Insert(ctx context.Context, t *domain.Tenant) (*domain.Tenant, error)
}
