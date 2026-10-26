package postgres

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ParkedTenantEventRepository is pending_tenant_events (CD-50, EVT-18). It
// runs on the RLS-scoped app pool: every caller binds the tenant's own GUC.
type ParkedTenantEventRepository struct {
	pool *pgcommon.Pool
}

var _ port.ParkedTenantEventStore = (*ParkedTenantEventRepository)(nil)

func NewParkedTenantEventRepository(pool *pgcommon.Pool) *ParkedTenantEventRepository {
	return &ParkedTenantEventRepository{pool: pool}
}

func (r *ParkedTenantEventRepository) Park(ctx context.Context, ev port.ParkedTenantEvent) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO pending_tenant_events (event_id, tenant_id, event_type, occurred_at, envelope)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (event_id) DO NOTHING`,
			ev.EventID, ev.TenantID, ev.EventType, ev.OccurredAt, ev.Envelope)
		return err
	})
}

func (r *ParkedTenantEventRepository) ListForTenant(ctx context.Context, tenantID uuid.UUID) ([]port.ParkedTenantEvent, error) {
	var out []port.ParkedTenantEvent
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT event_id, tenant_id, event_type, occurred_at, envelope, parked_at
			FROM pending_tenant_events
			WHERE tenant_id = $1
			ORDER BY occurred_at, event_id`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ev port.ParkedTenantEvent
			if err := rows.Scan(&ev.EventID, &ev.TenantID, &ev.EventType, &ev.OccurredAt, &ev.Envelope, &ev.ParkedAt); err != nil {
				return err
			}
			out = append(out, ev)
		}
		return rows.Err()
	})
	return out, err
}

func (r *ParkedTenantEventRepository) Claim(ctx context.Context, eventID uuid.UUID) (bool, error) {
	var claimed bool
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM pending_tenant_events WHERE event_id = $1`, eventID)
		if err != nil {
			return err
		}
		claimed = tag.RowsAffected() == 1
		return nil
	})
	return claimed, err
}

// LockTenantProvisioning takes pg_advisory_xact_lock on the tenant id. It
// must run inside the caller's transaction — the lock is released at commit
// or rollback, never held across transactions.
func (r *ParkedTenantEventRepository) LockTenantProvisioning(ctx context.Context, tenantID uuid.UUID) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 50))`, tenantID)
		return err
	})
}
