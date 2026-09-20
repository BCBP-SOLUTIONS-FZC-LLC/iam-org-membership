package port

import (
	"context"

	"github.com/google/uuid"
)

// ReconcilerStore is the BYPASSRLS (sysPool) persistence port for
// cross-tenant reconciler sweeps. Constructed against org_membership_migrator
// — never the RLS-scoped app pool, or every query would silently collapse
// to the caller's single tenant.
// Outbox pruning is NOT on this interface — it lives entirely in
// platform-events' outbox.Runner.PrunePublished (cmd/reconciler/jobs/
// outbox_prune.go), never hand-rolled SQL. A prior PruneOutbox method here
// duplicated that logic; removed 2026-09-20 once confirmed it had zero
// production callers (the migration to PrunePublished predates this).
type ReconcilerStore interface {
	ListSeatOverageCandidates(ctx context.Context, limit int) ([]uuid.UUID, error)
	ListRealmSyncPending(ctx context.Context, limit int) ([]RealmSyncCandidate, error)
	ClearRealmSyncPending(ctx context.Context, tenantID uuid.UUID) error
	HardDeleteExpiredTrials(ctx context.Context, graceDays int) (int, error)
	PruneProcessedEvents(ctx context.Context, ttlDays, limit int) (int, error)
}

// RealmSyncCandidate is one tenants row waiting for T-15 realm-config push.
type RealmSyncCandidate struct {
	TenantID             uuid.UUID
	LocalAccountsEnabled bool
}
