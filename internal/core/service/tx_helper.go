package service

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// pgadapterTxFromContext is a service-layer accessor to the running pgx.Tx
// injected by postgres.TxRunner.RunInTx via a private context key. Used
// only by provisioning + invitation code that must UPDATE tables lacking
// dedicated repo methods (e.g. tenants.ownerless_since).
//
// Runtime path: postgres.TxRunner puts the tx under postgres.txKey{} (a
// package-private key type). We can't access that key directly, so this
// helper walks ctx.Value chains looking for a value that satisfies the
// pgx.Tx interface. The postgres package's txBoundPublisher already stores
// the tx pointer inside itself; adjacent to that we look up the tx.
//
// The public seam is `port.EventPublisherFromContext`. Its returned type
// is `*postgres.txBoundPublisher` whose `.tx` field we can't read. So we
// take the simplest route: the postgres package exposes TxFromContext as
// a package function. We re-export it here under a private name so
// service code has a clean import boundary.
func pgadapterTxFromContext(ctx context.Context) (pgx.Tx, bool) {
	// This is a *service*-package helper; it cannot import
	// internal/adapter/outbound/postgres (Clean Architecture — services
	// depend only on port + domain). Since the running tx is under a
	// private key in the postgres package, we resolve it here via the
	// context's untyped Value walk.
	//
	// Concretely: the tx is put under postgres.txKey{}. We know the key's
	// concrete type-name matches "postgres.txKey"; the value implements
	// pgx.Tx. Rather than depend on that name (fragile), Phase 4 wires a
	// small helper via the port package (see port.TxAccessor).
	if v, ok := ctx.Value(txAccessorKey{}).(pgx.Tx); ok {
		return v, true
	}
	return nil, false
}

// txAccessorKey is the shared context key services and adapters use to
// pass the running tx WITHOUT importing the postgres package. The
// TxRunner in the postgres adapter must ALSO store its tx under this key
// (in addition to its private key) so services can read it back.
type txAccessorKey struct{}

// WithTx stores tx in ctx under the shared txAccessorKey. Called by the
// postgres.TxRunner integration.
func WithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txAccessorKey{}, tx)
}

// TxFromContext is the public accessor mirroring the package-private
// pgadapterTxFromContext. Used by cmd/reconciler/jobs.
func TxFromContext(ctx context.Context) (pgx.Tx, bool) {
	return pgadapterTxFromContext(ctx)
}
