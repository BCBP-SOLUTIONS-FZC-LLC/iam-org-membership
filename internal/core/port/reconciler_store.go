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
// processed_events pruning is not here either: it is platform-events'
// inbox.Store.Prune (port.InboxPruner, cmd/reconciler/jobs/
// processed_events_prune.go).
type ReconcilerStore interface {
	ListSeatOverageCandidates(ctx context.Context, limit int) ([]uuid.UUID, error)
	ListRealmSyncPending(ctx context.Context, limit int) ([]RealmSyncCandidate, error)
	ClearRealmSyncPending(ctx context.Context, tenantID uuid.UUID) error
	// ListExpiredTrialTenants returns at most limit tenant IDs whose status is
	// 'trial_expired' and trial_ends_at is older than graceDays. The caller is
	// responsible for deleting each tenant atomically with its purge event.
	ListExpiredTrialTenants(ctx context.Context, graceDays, limit int) ([]uuid.UUID, error)
	// ListTenantsWithParkedEvents returns at most limit tenant IDs that have
	// parked events (CD-50, EVT-18) and a tenants row (live or soft-deleted),
	// i.e. parked events a replay can now resolve.
	ListTenantsWithParkedEvents(ctx context.Context, limit int) ([]uuid.UUID, error)
}

// RealmSyncCandidate is one tenants row waiting for T-15 realm-config push.
type RealmSyncCandidate struct {
	TenantID             uuid.UUID
	LocalAccountsEnabled bool
}
