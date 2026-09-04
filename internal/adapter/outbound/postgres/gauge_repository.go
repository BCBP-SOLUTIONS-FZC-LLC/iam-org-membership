package postgres

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
)

// GaugeRepository serves the read-only, cross-tenant aggregate queries that
// back the DB-state gauges in §11.2 (tenant_ownerless, realm_sync_pending,
// seat_overage_active, pending_invitations_stale). Those gauges are counts
// of current table state rather than event tallies, so nothing on a request
// or reconciler path can maintain them; cmd/server polls this repository
// on an interval instead.
//
// It MUST be constructed with the BYPASSRLS sysPool (§4.4) — every query
// here spans tenants and would return zero rows through the RLS-scoped app
// pool with no tenant GUC bound.
//
// Unlike the other repositories in this package it implements no core port:
// its only consumer is the composition root's exporter goroutines, so
// routing it through the domain would add an interface no service uses.
// Matches iam-realm-provisioner's GaugeRepository: SQL stays in the
// postgres adapter; exporters never call WithConn / pgxpool.
type GaugeRepository struct {
	pool *pgcommon.Pool
}

// NewGaugeRepository constructs a GaugeRepository over the BYPASSRLS pool.
func NewGaugeRepository(sysPool *pgcommon.Pool) *GaugeRepository {
	return &GaugeRepository{pool: sysPool}
}

const ownerlessTenantsSQL = `
SELECT count(*) FROM tenants
 WHERE ownerless_since IS NOT NULL AND deleted_at IS NULL`

// CountOwnerlessTenants returns tenants with ownerless_since set (T-13).
func (r *GaugeRepository) CountOwnerlessTenants(ctx context.Context) (int64, error) {
	return r.count(ctx, ownerlessTenantsSQL)
}

const realmSyncPendingSQL = `
SELECT count(*) FROM tenants
 WHERE realm_sync_pending = true AND deleted_at IS NULL`

// CountRealmSyncPending returns tenants waiting on a realm-config sync (T-15).
func (r *GaugeRepository) CountRealmSyncPending(ctx context.Context) (int64, error) {
	return r.count(ctx, realmSyncPendingSQL)
}

const seatOverageActiveSQL = `
SELECT count(*) FROM tenants
 WHERE overage_since IS NOT NULL AND deleted_at IS NULL`

// CountSeatOverageActive returns tenants currently in seat overage (SEAT-5).
func (r *GaugeRepository) CountSeatOverageActive(ctx context.Context) (int64, error) {
	return r.count(ctx, seatOverageActiveSQL)
}

const pendingInvitationsStaleSQL = `
SELECT count(*) FROM pending_invitations
 WHERE status = 'pending' AND expires_at < now()`

// CountPendingInvitationsStale returns pending invitations past expires_at.
func (r *GaugeRepository) CountPendingInvitationsStale(ctx context.Context) (int64, error) {
	return r.count(ctx, pendingInvitationsStaleSQL)
}

func (r *GaugeRepository) count(ctx context.Context, sql string) (int64, error) {
	var n int64
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql).Scan(&n)
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}
