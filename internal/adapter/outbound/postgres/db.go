// Package postgres implements the outbound repository ports backed by
// PostgreSQL through platform-pgcommon. TxRunner is the seam that binds
// transaction-local RLS GUC + tx-bound outbox publisher onto every write.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DSNFromEnv builds a PostgreSQL connection URL for the application pool by
// delegating host/port/user/password/dbname/sslmode parsing and DSN
// assembly to pgcommon.ConfigFromEnv() — the same env vars
// (DATABASE_URL/PG_HOST/PG_PORT/PG_USER/PG_PASSWORD/PG_DBNAME/PG_SSLMODE)
// platform-pgcommon itself reads to build the pool Config used by main.go,
// so there is exactly one DSN-assembly implementation instead of two drifting
// in parallel. Warnings from ConfigFromEnv (invalid/insecure settings
// replaced by defaults) are surfaced at the call site that owns a logger
// (see cmd/server/main.go); this helper only returns the DSN string.
//
// URL format is required because the migration runner (pgmigrate.Runner)
// calls url.Parse on the DSN after prepending "pgx5://"; a keyword/value DSN
// would produce invalid URL escapes (%20 for spaces) and fail at startup.
// pgcommon builds the DSN via net/url, which already produces this format.
func DSNFromEnv() string {
	cfg, _ := pgcommon.ConfigFromEnv()
	if os.Getenv("DATABASE_URL") != "" {
		// DATABASE_URL is returned verbatim by pgcommon.ConfigFromEnv — set
		// statement_timeout via its own query string, not appended here.
		return cfg.DSN
	}
	return ApplyStatementTimeout(cfg.DSN)
}

// ApplyStatementTimeout appends a server-side statement_timeout option to dsn
// so hung queries release pool connections instead of holding them for the
// full HTTP deadline. PG_STATEMENT_TIMEOUT accepts a Go duration string
// (e.g. "5s", "500ms"). This has no pgcommon equivalent — pgcommon.Config has
// no statement-timeout field — so it remains a small extension layered on
// top of the pgcommon-built DSN rather than a full DSN builder. Ignored when
// dsn is empty or PG_STATEMENT_TIMEOUT is unset.
func ApplyStatementTimeout(dsn string) string {
	if dsn == "" {
		return dsn
	}
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

// itoa is a tiny helper so callers can inline a LIMIT clause into a raw SQL
// string without importing strconv for a single conversion. Moved here
// (originally lived in the now-removed delegation_repository.go, ADR-0008
// v2) since invitation_repository.go's reconciler queries share it too.
func itoa(n int) string {
	if n <= 0 {
		return "100"
	}
	// Simple positive-int formatter.
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
