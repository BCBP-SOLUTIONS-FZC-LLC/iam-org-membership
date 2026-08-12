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
	// wrapped in a DomainError. Filters deleted_at IS NULL — offboarded
	// (soft-deleted) tenants return ErrTenantNotFound.
	FindByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)

	// FindByIDIncludingDeleted returns the tenant row regardless of deleted_at.
	// Used only by iam-system internal paths (I-2 RP cleanup, reconcilers) that
	// must act on offboarded tenants after O&M soft-deletes them. Never called
	// from public/operator routes.
	FindByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)

	// Update applies patch under optimistic-locking (CONC-1..4) inside the
	// caller's transaction. On WHERE record_version mismatch, returns
	// ErrOptimisticLockConflict wrapped in DomainError with Details
	// containing the current record_version + updated_at (CONC-4).
	Update(ctx context.Context, id uuid.UUID, patch *domain.TenantPatch) (*domain.Tenant, error)

	// SetRealmSyncPending marks the tenant row realm_sync_pending=true so the
	// realm-config-sync reconciler picks it up (T-15 Option A, §16 A58).
	// Called by TenantService.Patch when RP.PatchRealmConfig fails after a
	// successful local UPDATE — the committed local change must be reconciled.
	// Idempotent; no optimistic-lock (setting the flag is safe to repeat).
	SetRealmSyncPending(ctx context.Context, tenantID uuid.UUID) error

	// Insert creates a new tenant row (used by I-1 in Phase 4 and by the
	// TrialTenantProvisioned consumer in Phase 3). Phase 2 exposes it for
	// admin-side seeding paths only.
	//
	// LLD I-1 idempotency: on ON CONFLICT (id) DO NOTHING the row already
	// existed; the returned bool wasCreated=false signals the caller should
	// skip re-seeding and respond 200 (idempotent replay) instead of 201.
	// wasCreated=true means the row was freshly inserted this call.
	Insert(ctx context.Context, t *domain.Tenant) (*domain.Tenant, bool, error)
}
