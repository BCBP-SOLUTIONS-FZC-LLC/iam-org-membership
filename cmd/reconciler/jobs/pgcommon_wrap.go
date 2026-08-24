package jobs

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
)

// runInTxWithSysPool runs fn inside a pgx.Tx against the BYPASSRLS
// sysPool via pgcommon.RunInTx. Reconcilers span tenants so they must not
// go through the RLS-scoped app pool; sysPool itself has no GUCProvider
// (it's a real *pgcommon.Pool, just without RLS GUC injection), so this
// gets the same begin/commit/rollback semantics as every other
// transaction in this service — plus pgcommon's connection-acquire
// metrics and slow-query logging — instead of a hand-rolled Acquire/
// BeginTx/Commit/Rollback sequence.
//
// Uses default pgx.TxOptions; no reconciler currently needs SERIALIZABLE
// or READ ONLY. Add a variant when one does.
func runInTxWithSysPool(ctx context.Context, pool *pgcommon.Pool, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return pgcommon.RunInTx(ctx, pool, pgx.TxOptions{}, fn)
}
