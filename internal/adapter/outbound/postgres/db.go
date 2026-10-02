// Package postgres implements the outbound repository ports backed by
// PostgreSQL through platform-pgcommon. TxRunner is the seam that binds
// transaction-local RLS GUC + tx-bound outbox publisher onto every write.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
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
//
// PG_STATEMENT_TIMEOUT is not part of the DSN: ConfigFromEnv reads it into
// Config.StatementTimeout and pgcommon applies it per transaction
// (SET LOCAL), which is safe behind PgBouncer transaction pooling.
func DSNFromEnv() string {
	cfg, _ := pgcommon.ConfigFromEnv()
	return cfg.DSN
}

// SystemPoolName is the pool metric label of the BYPASSRLS system pool, so
// its platform_db_* series stay separate from the app pool's.
const SystemPoolName = "system"

// SystemPoolConfig returns pgcommon.Config for the BYPASSRLS system pool
// (LLD §4.4), matching iam-user-profile. The system pool deliberately has
// no GUCProvider — cross-tenant reconciler / exporter / I-16 queries run
// under a BYPASSRLS role — but still connects through PgBouncer in
// production, so PGBouncerMode is forced true unconditionally
// (SimpleProtocol + MinConns:0). A bare pgcommon.Config{DSN, Logger}
// literal would leave PGBouncerMode at the Go zero-value false and drop
// ConfigFromEnv pool sizing, breaking transaction-pooling deployments
// even when the app pool correctly reads PG_BOUNCER_MODE.
//
// Pool sizing, lifetimes, SlowQueryThreshold and StatementTimeout are
// copied from ConfigFromEnv so sysPool and the app pool share one
// env-driven source of truth. PoolName is always SystemPoolName. Tracer is
// left unset — call sites wire gincommon.NewSpanTracer so db.query spans
// export through gincommon's TracerProvider.
func SystemPoolConfig(dsn string, log port.Logger) pgcommon.Config {
	cfg, _ := pgcommon.ConfigFromEnv()
	cfg.DSN = dsn
	cfg.PoolName = SystemPoolName
	cfg.GUCProvider = nil
	cfg.PGBouncerMode = true
	// ConfigFromEnv chose its MinConns default (2) for the PG_BOUNCER_MODE
	// it read, before PgBouncer mode was forced on here. Unless PG_MIN_CONNS
	// was set deliberately, zero it so NewPool applies the PgBouncer default
	// (0 — let PgBouncer manage backends) instead of pinning 2 idle
	// connections per replica through the pooler.
	if _, set := os.LookupEnv("PG_MIN_CONNS"); !set {
		cfg.MinConns = 0
	}
	cfg.Tracer = nil
	if log != nil {
		cfg.Logger = NewLoggerAdapter(log)
	} else {
		cfg.Logger = nil
	}
	return cfg
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

// MigrationDSNFromEnv returns the DSN for schema migrations:
// MIGRATION_DATABASE_URL when set, else the app DSN (pgcommon's own
// MigrationDSNFromEnv). Migrations MUST bypass PgBouncer because the
// migration runner uses pg_advisory_lock, which is session-scoped and
// breaks under transaction pooling (CONFIG-2), so MIGRATION_DATABASE_URL
// must be set whenever PG_BOUNCER_MODE is on. No statement timeout is
// applied: long DDL and the advisory-lock wait must not be cut short.
func MigrationDSNFromEnv() string {
	return pgcommon.MigrationDSNFromEnv()
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

// writeRetryOpts is pgcommon's documented high-throughput OLTP preset.
// Deadlock (40P01) and serialization failure (40001) retry with exponential
// backoff + jitter. Nested withPool joins (already inside a tx) do not
// retry — the outer TxRunner owns the attempt.
var writeRetryOpts = pgcommon.RetryOptions{
	MaxAttempts:    3,
	InitialWait:    10 * time.Millisecond,
	MaxWait:        500 * time.Millisecond,
	Multiplier:     2.0,
	JitterFraction: 0.25,
}

// RunInTx runs fn inside a transaction, injects a tx-bound publisher into
// the ctx, and maps low-level connection errors into
// domain.ErrDBUnavailable (503) so handlers get a consistent 5xx
// shape (§17). SQL-level errors (unique violation, FK, check) bubble up
// unchanged for service-layer classification. Contended writes retry via
// pgcommon.RunInTxWithRetryOpts on deadlock / serialization failure.
func (r *TxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return wrapConnErr(pgcommon.RunInTxWithRetryOpts(ctx, r.pool, pgx.TxOptions{}, writeRetryOpts, func(ctx context.Context, tx pgx.Tx) error {
		txCtx := port.WithTx(ctx, tx)
		if r.events != nil {
			txCtx = port.WithEventPublisher(txCtx, r.events)
		}
		return fn(txCtx)
	}))
}

// WithTx re-exports port.WithTx for existing call sites in this package's
// tests. New code should call port.WithTx directly.
func WithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return port.WithTx(ctx, tx)
}

// TxFromContext re-exports port.TxFromContext for existing call sites in
// this package's tests. New code should call port.TxFromContext directly.
func TxFromContext(ctx context.Context) (pgx.Tx, bool) {
	return port.TxFromContext(ctx)
}

// withPool runs fn inside a transaction, joining an existing one if present
// in ctx. Used by repository helpers so a read outside a service tx still
// binds RLS via pgcommon.RunInTx's checkout hook.
func withPool(ctx context.Context, pool *pgcommon.Pool, fn func(pgx.Tx) error) error {
	if tx, ok := port.TxFromContext(ctx); ok {
		return wrapConnErr(fn(tx))
	}
	return wrapConnErr(pgcommon.RunInTx(ctx, pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return fn(tx)
	}))
}

// wrapConnErr converts non-protocol database errors into
// ErrDBUnavailable. SQL-protocol errors (*pgconn.PgError) that are
// not connectivity/resource classes pass through so the service layer can
// distinguish an integrity violation from a network outage.
//
// SQLSTATE class 08 (connection exception), 53 (insufficient resources),
// 57 (operator intervention, except 57014) and 58 (system error) are
// remapped to domain.ErrDBUnavailable here so HTTP HandleError never needs
// to inspect a raw *pgconn.PgError — those classes are availability
// failures (503). A closed pool (pgcommon.IsPoolClosed: pgcommon's own
// sentinel after Close/DrainAndClose, or pgxpool's) is the other
// positively-identifiable connectivity failure. 57014 (query_canceled —
// statement_timeout fired) and 55P03 (lock_not_available — lock_timeout)
// are slow-query signals, not outages, and map to request_timeout.
//
// Everything else — including a plain Go error a caller's own RunInTx/
// withPool callback returns for its own business reasons — passes through
// completely unchanged. This function has no way to distinguish "the pool
// itself failed" from "fn's own business logic failed" for any error
// shape beyond the ones positively recognized above, since
// pgcommon.RunInTxWithRetryOpts returns both shapes identically; defaulting
// the unrecognized case to ErrDependencyUnavailable (as this used to) — a
// bug found and fixed in iam-realm-provisioner's identical wrapConnErr,
// then found here too during a cross-service alignment check — silently
// discarded the caller's real error under a misleading "database
// unavailable" 503 for every unrecognized failure, including deliberate
// business-rule errors a service intentionally returns from inside a
// transaction. HTTP HandleError already re-classifies a leaked raw PgError
// of these same connectivity/resource classes into 503 independently, so a
// genuine low-level connectivity failure that somehow isn't positively
// recognized here still degrades no worse than a generic 500, never a
// masked/wrong business error.
func wrapConnErr(err error) error {
	if err == nil {
		return nil
	}
	var de *domain.DomainError
	if errors.As(err, &de) {
		return err
	}
	// Deadline first: pgcommon's retry wraps BOTH the context error and the
	// last attempt's error when the request deadline cuts a deadlock /
	// serialization-failure backoff short ("retry wait … interrupted: %w
	// (last error: %w)"), so checking the deadlock first would misreport a
	// timeout as a conflict.
	// The original error stays in the chain (Cause wraps both), so a caller
	// can still test errors.Is(err, context.Canceled) — e.g. on shutdown.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &domain.DomainError{
			Code:    domain.ErrRequestTimeout.Error(),
			Message: "request deadline exceeded during a database operation",
			Cause:   fmt.Errorf("%w: %w", domain.ErrRequestTimeout, err),
		}
	}
	// Retries exhausted on a deadlock (40P01) or serialization failure
	// (40001): a genuine concurrent-write conflict, safe for the client to
	// retry — 409, not an unexplained 500.
	if pgcommon.IsDeadlock(err) || pgcommon.IsSerializationFailure(err) {
		return (&domain.DomainError{
			Code:    domain.ErrConflict.Error(),
			Message: "concurrent update conflict — retry the request",
			Cause:   fmt.Errorf("%w: %w", domain.ErrConflict, err),
		}).WithDetails(map[string]any{"code": "concurrent_update"})
	}
	if pgcommon.IsQueryCanceled(err) || pgcommon.IsLockNotAvailable(err) {
		return &domain.DomainError{
			Code:    domain.ErrRequestTimeout.Error(),
			Message: "database statement or lock timeout",
			Cause:   fmt.Errorf("%w: %w", domain.ErrRequestTimeout, err),
		}
	}
	if IsDBUnavailable(err) {
		// Cause keeps the original error so logs and errors.As still see
		// the SQLSTATE.
		return &domain.DomainError{
			Code:    domain.ErrDBUnavailable.Error(),
			Message: "database unavailable",
			Cause:   fmt.Errorf("%w: %w", domain.ErrDBUnavailable, err),
		}
	}
	return err
}

// IsDBUnavailable reports whether err is a database availability failure:
// SQLSTATE class 08, 53, 57 (except 57014) or 58, or a closed pool.
func IsDBUnavailable(err error) bool {
	return pgcommon.IsConnectionException(err) || pgcommon.IsInsufficientResources(err) ||
		pgcommon.IsOperatorIntervention(err) || pgcommon.IsSystemError(err) || pgcommon.IsPoolClosed(err)
}

// itoa is a tiny helper so callers can inline a LIMIT clause into a raw SQL
// string without importing strconv for a single conversion. Moved here
// (originally lived in the now-removed delegation_repository.go, ADR-0008
// v2) since invitation_repository.go's reconciler queries share it too.
func itoa(n int) string {
	if n <= 0 {
		return "100"
	}
	return strconv.Itoa(n)
}
