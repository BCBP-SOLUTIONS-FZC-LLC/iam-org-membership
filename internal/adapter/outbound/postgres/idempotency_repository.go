package postgres

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
)

// IdempotencyRepository implements port.IdempotencyStore against the
// processed_events table (§4.2), shared by every SQS consumer this service
// runs. See port.IdempotencyStore's doc comment for why MarkProcessed
// joins the caller's transaction (EVT-14) rather than opening its own, the
// one deliberate difference from iam-user-profile's equivalent store.
type IdempotencyRepository struct {
	pool *pgcommon.Pool
}

var _ port.IdempotencyStore = (*IdempotencyRepository)(nil)

// NewIdempotencyRepository builds an IdempotencyRepository.
func NewIdempotencyRepository(pool *pgcommon.Pool) *IdempotencyRepository {
	return &IdempotencyRepository{pool: pool}
}

// IsProcessed reports whether eventID has already been recorded as
// processed by consumer. A standalone read outside any caller transaction.
func (r *IdempotencyRepository) IsProcessed(ctx context.Context, consumer, eventID string) (bool, error) {
	var exists bool
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM processed_events WHERE event_id = $1 AND consumer = $2)`,
			eventID, consumer,
		).Scan(&exists)
	})
	if err != nil {
		return false, err
	}
	return exists, nil
}

// MarkProcessed records eventID as processed by consumer. Inside
// TxRunner.RunInTx this joins the outer transaction so the dedup write
// commits or rolls back atomically with the projection.
func (r *IdempotencyRepository) MarkProcessed(ctx context.Context, consumer, eventID string) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO processed_events (event_id, consumer)
			VALUES ($1, $2)
			ON CONFLICT (event_id, consumer) DO NOTHING`, eventID, consumer)
		return err
	})
}
