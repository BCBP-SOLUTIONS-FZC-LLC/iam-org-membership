package jobs

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// runInTxWithSysPool runs fn inside a pgx.Tx against the BYPASSRLS
// sysPool. Reconcilers span tenants so they must not go through the
// RLS-scoped app pool.
//
// Uses default pgx.TxOptions; no reconciler currently needs SERIALIZABLE
// or READ ONLY. Add a variant when one does.
func runInTxWithSysPool(ctx context.Context, pool *pgxpool.Pool, fn func(ctx context.Context, tx pgx.Tx) error) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	if err := fn(ctx, tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}
