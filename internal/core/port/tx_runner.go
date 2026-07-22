package port

import "context"

// TxRunner is the seam services use to run business writes and event
// enqueues atomically (EVT-10, CONS-1..4). The concrete implementation is
// `postgres.TxRunner` which opens a pgx.Tx, injects a tx-bound
// ContextEventPublisher into ctx, and commits both together.
//
// Services depend on this port; they never touch pgx directly. Repositories
// participating in the tx read ctx via `postgres.TxFromContext` (through
// the `withPool` helper) so a write inside RunInTx joins the same tx.
type TxRunner interface {
	RunInTx(ctx context.Context, fn func(ctx context.Context) error) error
}
