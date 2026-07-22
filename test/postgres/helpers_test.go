//go:build integration

package postgres_test

import (
	"github.com/jackc/pgx/v5"
)

// Type aliases so the RLS test file doesn't have to import pgx directly
// (keeps its import block focused on test-observable concerns).
type pgxTx = pgx.Tx

func pgxTxOpts() pgx.TxOptions { return pgx.TxOptions{} }
