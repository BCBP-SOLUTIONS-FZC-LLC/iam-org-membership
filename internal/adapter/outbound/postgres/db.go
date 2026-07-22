// Package postgres implements the outbound repository ports backed by
// PostgreSQL through platform-pgcommon. TxRunner is the seam that binds
// transaction-local RLS GUC + tx-bound outbox publisher onto every write.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DSNFromEnv builds the DSN for the application (RLS-scoped) pool. When
// PG_BOUNCER_MODE=true this should point at PgBouncer (transaction-pool
// mode) — the pool's GUCProvider will emit SET LOCAL app.tenant_id on every
// checkout so the GUC binds transactionally, never at session scope (RLS-6).
//
// Returns URL form (not keyword/value): platform-pgcommon's migration runner
// wraps the DSN with url.Parse and rejects keyword form.
func DSNFromEnv() string {
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		return dsn
	}
	host := envOrDB("PG_HOST", "localhost")
	port := envOrDB("PG_PORT", "5432")
	user := os.Getenv("PG_USER")
	pass := os.Getenv("PG_PASSWORD")
	dbname := envOrDB("PG_DBNAME", "org_membership")
	sslmode := envOrDB("PG_SSLMODE", "require")
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		url.PathEscape(user), url.PathEscape(pass), host, port, dbname, sslmode)

	// Server-side statement timeout so hung queries release pool connections
	// rather than holding them for the full HTTP deadline. Accepts a Go
	// duration string (e.g. "5s", "500ms"). Ignored when DATABASE_URL is set.
	if t := os.Getenv("PG_STATEMENT_TIMEOUT"); t != "" {
		if d, err := time.ParseDuration(t); err == nil && d > 0 {
			dsn += fmt.Sprintf("&options=-c%%20statement_timeout%%3D%d", d.Milliseconds())
		}
	}
	return dsn
}

// SystemDSNFromEnv returns the DSN for the privileged cross-tenant pool used
// by the reconciler binary and the seat-overage / realm-sync / trial-cleanup
// jobs. In production it MUST point to org_membership_migrator (BYPASSRLS,
// RLS-4). Falls back to DSNFromEnv() for local dev — cross-tenant queries
// will then be RLS-filtered to zero rows (safe no-op).
func SystemDSNFromEnv() string {
	if dsn := os.Getenv("SYSTEM_DATABASE_URL"); dsn != "" {
		return dsn
	}
	return DSNFromEnv()
}

// MigrationDSNFromEnv returns the DSN for schema migrations. Migrations MUST
// bypass PgBouncer because the migration runner uses pg_advisory_lock which
// is session-scoped and breaks under transaction pooling (CONFIG-2).
// MIGRATION_DATABASE_URL must be set whenever PG_BOUNCER_MODE=true.
func MigrationDSNFromEnv() string {
	if dsn := os.Getenv("MIGRATION_DATABASE_URL"); dsn != "" {
		return dsn
	}
	return DSNFromEnv()
}

func envOrDB(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// TxRunner wraps pgcommon.Pool to implement service.TxRunner. When a
// port.EventPublisher is provided it is injected into the tx context so
// services can enqueue events atomically via
// port.EventPublisherFromContext(txCtx) without importing this package.
type TxRunner struct {
	pool   *pgcommon.Pool
	events port.EventPublisher
}

// NewTxRunner constructs a TxRunner. Pass nil for events during bootstrap
// paths (e.g. migration-only reconciler runs) where no outbox writes occur.
func NewTxRunner(pool *pgcommon.Pool, events port.EventPublisher) *TxRunner {
	return &TxRunner{pool: pool, events: events}
}

// RunInTx runs fn inside a transaction, injects a tx-bound publisher into
// the ctx, and maps low-level connection errors into
// domain.ErrDependencyUnavailable (503) so handlers get a consistent 5xx
// shape (§17). SQL-level errors (unique violation, FK, check) bubble up
// unchanged for service-layer classification.
func (r *TxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return wrapConnErr(pgcommon.RunInTx(ctx, r.pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		txCtx := withTx(ctx, tx)
		// Also expose the running tx via the service-layer key so
		// provisioning code that must UPDATE tables lacking dedicated
		// repo methods (e.g. tenants.ownerless_since in I-5) can reach it
		// without importing this package.
		txCtx = service.WithTx(txCtx, tx)
		if r.events != nil {
			txCtx = port.WithEventPublisher(txCtx, &txBoundPublisher{pub: r.events, tx: tx})
		}
		return fn(txCtx)
	}))
}

// txBoundPublisher adapts port.EventPublisher (requires pgx.Tx) to
// port.ContextEventPublisher (used via context lookup by the service layer).
type txBoundPublisher struct {
	pub port.EventPublisher
	tx  pgx.Tx
}

func (p *txBoundPublisher) EnqueueCtx(ctx context.Context, event *domain.DomainEvent) error {
	return p.pub.Enqueue(ctx, p.tx, event)
}

// txKey stores the active pgx.Tx in context so repository methods can join
// an in-flight transaction rather than opening a nested one.
type txKey struct{}

func withTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// TxFromContext retrieves the active pgx.Tx set by RunInTx, if any.
// Repository helpers call withPool which joins the tx when present.
func TxFromContext(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	return tx, ok
}

// withPool runs fn inside a transaction, joining an existing one if present
// in ctx. Used by repository helpers so a read outside a service tx still
// binds RLS via pgcommon.RunInTx's checkout hook.
func withPool(ctx context.Context, pool *pgcommon.Pool, fn func(pgx.Tx) error) error {
	if tx, ok := TxFromContext(ctx); ok {
		return wrapConnErr(fn(tx))
	}
	return wrapConnErr(pgcommon.RunInTx(ctx, pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return fn(tx)
	}))
}

// wrapConnErr converts non-protocol database errors into
// ErrDependencyUnavailable. SQL-protocol errors (pgconn.PgError) and
// context cancellations pass through unchanged so the service layer can
// distinguish an integrity violation from a network outage.
func wrapConnErr(err error) error {
	if err == nil {
		return nil
	}
	var de *domain.DomainError
	if errors.As(err, &de) {
		return err
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return err // server responded with a SQL error — not a connectivity failure
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return domain.NewError(domain.ErrDependencyUnavailable, "database unavailable")
}

// suppress unused-import warning until we add repositories in later phases;
// withPool is exercised through the future repository layer.
var _ = withPool
