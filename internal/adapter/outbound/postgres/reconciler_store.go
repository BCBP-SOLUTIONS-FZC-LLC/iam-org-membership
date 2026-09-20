package postgres

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ReconcilerStore implements port.ReconcilerStore against the BYPASSRLS
// sysPool. Cross-tenant sweeps (seat-overage candidates, realm-sync,
// trial hard-delete, outbox / processed_events prune) live here so
// cmd/reconciler/jobs never issues SQL.
type ReconcilerStore struct {
	pool *pgcommon.Pool
}

var _ port.ReconcilerStore = (*ReconcilerStore)(nil)

func NewReconcilerStore(pool *pgcommon.Pool) *ReconcilerStore {
	return &ReconcilerStore{pool: pool}
}

func (s *ReconcilerStore) ListSeatOverageCandidates(ctx context.Context, limit int) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := withPool(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id FROM tenants
			WHERE deleted_at IS NULL
			  AND (overage_since IS NOT NULL OR licensed_seats > 0)
			LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}

func (s *ReconcilerStore) ListRealmSyncPending(ctx context.Context, limit int) ([]port.RealmSyncCandidate, error) {
	var out []port.RealmSyncCandidate
	err := withPool(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, local_accounts_enabled FROM tenants
			WHERE realm_sync_pending = true AND deleted_at IS NULL
			ORDER BY local_accounts_enabled ASC
			LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c port.RealmSyncCandidate
			if err := rows.Scan(&c.TenantID, &c.LocalAccountsEnabled); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

func (s *ReconcilerStore) ClearRealmSyncPending(ctx context.Context, tenantID uuid.UUID) error {
	return withPool(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET realm_sync_pending = false WHERE id = $1`, tenantID)
		return err
	})
}

func (s *ReconcilerStore) HardDeleteExpiredTrials(ctx context.Context, graceDays int) (int, error) {
	var n int
	err := withPool(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			DELETE FROM tenants
			WHERE status = 'trial_expired'
			  AND trial_ends_at < now() - make_interval(days => $1)
			  AND deleted_at IS NULL`, graceDays)
		if err != nil {
			return err
		}
		n = int(tag.RowsAffected())
		return nil
	})
	return n, err
}

func (s *ReconcilerStore) PruneProcessedEvents(ctx context.Context, ttlDays, limit int) (int, error) {
	var n int
	err := withPool(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			DELETE FROM processed_events
			WHERE (event_id, consumer) IN (
				SELECT event_id, consumer FROM processed_events
				WHERE processed_at < now() - make_interval(days => $1)
				LIMIT $2
			)`, ttlDays, limit)
		if err != nil {
			return err
		}
		n = int(tag.RowsAffected())
		return nil
	})
	return n, err
}
