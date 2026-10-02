package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── DSNFromEnv ─────────────────────────────────────────────────────────

func TestDSNFromEnv_UsesDatabaseURLShortcutWhenSet(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x:y@override.example:5432/appdb?sslmode=disable")
	assert.Equal(t,
		"postgres://x:y@override.example:5432/appdb?sslmode=disable",
		DSNFromEnv(),
		"DATABASE_URL shortcut short-circuits before any component parsing")
}

func TestDSNFromEnv_BuildsFromComponents(t *testing.T) {
	_ = os.Unsetenv("DATABASE_URL")
	_ = os.Unsetenv("PG_STATEMENT_TIMEOUT")
	t.Setenv("PG_HOST", "db.internal")
	t.Setenv("PG_PORT", "6432")
	t.Setenv("PG_USER", "org_membership_app")
	t.Setenv("PG_PASSWORD", "secret")
	t.Setenv("PG_DBNAME", "org_membership")
	t.Setenv("PG_SSLMODE", "require")

	dsn := DSNFromEnv()
	assert.True(t, strings.HasPrefix(dsn, "postgres://"))
	assert.Contains(t, dsn, "org_membership_app:secret")
	assert.Contains(t, dsn, "@db.internal:6432/org_membership")
	assert.Contains(t, dsn, "sslmode=require")
}

func TestDSNFromEnv_URLEscapesSlashInUser(t *testing.T) {
	_ = os.Unsetenv("DATABASE_URL")
	_ = os.Unsetenv("PG_STATEMENT_TIMEOUT")
	t.Setenv("PG_HOST", "localhost")
	t.Setenv("PG_PORT", "5432")
	t.Setenv("PG_USER", "u/name") // slash must be escaped to %2F
	t.Setenv("PG_PASSWORD", "pw")
	t.Setenv("PG_DBNAME", "d")
	t.Setenv("PG_SSLMODE", "disable")

	dsn := DSNFromEnv()
	assert.Contains(t, dsn, "u%2Fname:pw",
		"slash in username must be url-escaped so the DSN parses correctly")
}

func TestDSNFromEnv_EmptyWhenUserAndDBNameUnset(t *testing.T) {
	// pgcommon.ConfigFromEnv requires PG_USER + PG_DBNAME to build a DSN —
	// unlike the old hand-rolled builder, there is no "org_membership"
	// fallback database name. An empty DSN lets NewPool fail fast with a
	// clear connection error instead of silently connecting to the wrong
	// database.
	_ = os.Unsetenv("DATABASE_URL")
	_ = os.Unsetenv("PG_HOST")
	_ = os.Unsetenv("PG_PORT")
	_ = os.Unsetenv("PG_USER")
	_ = os.Unsetenv("PG_PASSWORD")
	_ = os.Unsetenv("PG_DBNAME")
	_ = os.Unsetenv("PG_SSLMODE")
	_ = os.Unsetenv("PG_STATEMENT_TIMEOUT")

	assert.Empty(t, DSNFromEnv())
}

func TestDSNFromEnv_DefaultsHostPortSSLModeWhenUnset(t *testing.T) {
	_ = os.Unsetenv("DATABASE_URL")
	_ = os.Unsetenv("PG_HOST")
	_ = os.Unsetenv("PG_PORT")
	t.Setenv("PG_USER", "org_membership_app")
	t.Setenv("PG_PASSWORD", "secret")
	t.Setenv("PG_DBNAME", "org_membership")
	_ = os.Unsetenv("PG_SSLMODE")
	_ = os.Unsetenv("PG_STATEMENT_TIMEOUT")

	dsn := DSNFromEnv()
	assert.Contains(t, dsn, "@localhost:5432/org_membership")
	assert.Contains(t, dsn, "sslmode=require",
		"default sslmode is 'require' — safe default")
}

// PG_STATEMENT_TIMEOUT is applied per transaction by pgcommon
// (Config.StatementTimeout), never as a DSN startup option: PgBouncer may
// reject unknown startup parameters, and appending to a DSN with no query
// string corrupted the database name.
func TestDSNFromEnv_NeverCarriesStatementTimeout(t *testing.T) {
	_ = os.Unsetenv("DATABASE_URL")
	t.Setenv("PG_USER", "org_membership_app")
	t.Setenv("PG_DBNAME", "org_membership")
	t.Setenv("PG_STATEMENT_TIMEOUT", "5s")
	assert.NotContains(t, DSNFromEnv(), "statement_timeout")

	t.Setenv("DATABASE_URL", "postgres://a:b@app.example:5432/omdb")
	assert.Equal(t, "postgres://a:b@app.example:5432/omdb", DSNFromEnv(),
		"a DATABASE_URL without a query string must stay intact")
}

// ── SystemDSNFromEnv ───────────────────────────────────────────────────

func TestSystemDSNFromEnv_UsesSystemVarWhenSet(t *testing.T) {
	t.Setenv("SYSTEM_DATABASE_URL", "postgres://sys:x@system.example:5432/omdb")
	assert.Equal(t,
		"postgres://sys:x@system.example:5432/omdb",
		SystemDSNFromEnv())
}

func TestSystemDSNFromEnv_FallsBackToDSNFromEnv(t *testing.T) {
	_ = os.Unsetenv("SYSTEM_DATABASE_URL")
	t.Setenv("DATABASE_URL", "postgres://a:b@app.example:5432/omdb")
	assert.Equal(t, DSNFromEnv(), SystemDSNFromEnv(),
		"unset SYSTEM_DATABASE_URL must fall through to the app DSN")
}

// ── MigrationDSNFromEnv ────────────────────────────────────────────────

func TestMigrationDSNFromEnv_UsesMigrationVarWhenSet(t *testing.T) {
	_ = os.Unsetenv("PG_STATEMENT_TIMEOUT")
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://m:x@migrations.example:5432/omdb")
	assert.Equal(t,
		"postgres://m:x@migrations.example:5432/omdb",
		MigrationDSNFromEnv())
}

// Migrations get no statement timeout: long DDL and the advisory-lock wait
// must not be killed after PG_STATEMENT_TIMEOUT.
func TestMigrationDSNFromEnv_IgnoresStatementTimeout(t *testing.T) {
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://m:x@migrations.example:5432/omdb?sslmode=disable")
	t.Setenv("PG_STATEMENT_TIMEOUT", "5s")
	assert.Equal(t, "postgres://m:x@migrations.example:5432/omdb?sslmode=disable", MigrationDSNFromEnv())
}

func TestMigrationDSNFromEnv_FallsBackToDSNFromEnv(t *testing.T) {
	_ = os.Unsetenv("MIGRATION_DATABASE_URL")
	t.Setenv("DATABASE_URL", "postgres://a:b@app.example:5432/omdb")
	assert.Equal(t, DSNFromEnv(), MigrationDSNFromEnv(),
		"unset MIGRATION_DATABASE_URL falls through to the app DSN")
}

// ── SystemPoolConfig ───────────────────────────────────────────────────

func TestSystemPoolConfig_ForcesPGBouncerMode(t *testing.T) {
	// Env would otherwise leave PGBouncerMode false; sysPool must force it.
	t.Setenv("PG_BOUNCER_MODE", "false")
	t.Setenv("PG_MAX_CONNS", "20")
	t.Setenv("PG_SLOW_QUERY_THRESHOLD", "200ms")
	_ = os.Unsetenv("PG_STATEMENT_TIMEOUT")

	cfg := SystemPoolConfig("postgres://sys@host/db", nil)
	assert.Equal(t, "postgres://sys@host/db", cfg.DSN)
	assert.True(t, cfg.PGBouncerMode, "sysPool must force PGBouncerMode:true — zero-value false breaks PgBouncer txn pooling")
	assert.Nil(t, cfg.GUCProvider, "sysPool must not inject tenant GUCs")
	assert.Nil(t, cfg.Tracer, "Tracer is wired by the call site, not SystemPoolConfig")
	assert.Nil(t, cfg.Logger, "nil log must leave Logger unset")
	assert.Equal(t, int32(20), cfg.MaxConns, "sysPool inherits pool sizing from ConfigFromEnv")
}

// ConfigFromEnv's direct-mode MinConns default (2) must not survive the
// forced PgBouncer mode; an explicit PG_MIN_CONNS still wins.
func TestSystemPoolConfig_MinConnsFollowsForcedPGBouncerMode(t *testing.T) {
	t.Setenv("PG_BOUNCER_MODE", "false")
	_ = os.Unsetenv("PG_MIN_CONNS")
	assert.Zero(t, SystemPoolConfig("postgres://sys@host/db", nil).MinConns,
		"unset PG_MIN_CONNS → NewPool's PgBouncer default, not direct mode's 2")

	t.Setenv("PG_MIN_CONNS", "3")
	assert.EqualValues(t, 3, SystemPoolConfig("postgres://sys@host/db", nil).MinConns, "an explicit PG_MIN_CONNS is kept")
}

func TestSystemPoolConfig_NonNilLoggerWiresAdapter(t *testing.T) {
	_ = os.Unsetenv("PG_STATEMENT_TIMEOUT")
	cfg := SystemPoolConfig("postgres://sys@host/db", &fakePortLogger{})
	assert.NotNil(t, cfg.Logger, "non-nil port.Logger must be wrapped via NewLoggerAdapter")
}

func TestSystemPoolConfig_StatementTimeoutFromConfigNotDSN(t *testing.T) {
	t.Setenv("PG_STATEMENT_TIMEOUT", "5s")
	cfg := SystemPoolConfig("postgres://sys@host/db?sslmode=disable", nil)
	assert.Equal(t, "postgres://sys@host/db?sslmode=disable", cfg.DSN)
	assert.Equal(t, 5*time.Second, cfg.StatementTimeout, "pgcommon applies it per transaction")
}

// Both pools emit platform_db_*{pool=...}; sharing "default" would merge
// the BYPASSRLS pool's series into the app pool's.
func TestSystemPoolConfig_HasItsOwnPoolName(t *testing.T) {
	t.Setenv("PG_POOL_NAME", "default")
	assert.Equal(t, SystemPoolName, SystemPoolConfig("postgres://sys@host/db", nil).PoolName)
}

// ── wrapConnErr ──────────────────────────────────────────────────────────

// wrapConnErr remaps SQLSTATE class 08/53/57 (except 57014)/58
// (availability failures) to domain.ErrDBUnavailable. Other PgErrors (e.g. 23505 unique_violation)
// still pass through so the service layer can classify them.
func TestWrapConnErr_AvailabilitySQLStateMapsToDBUnavailable(t *testing.T) {
	for _, code := range []string{"08006", "53300", "57P01", "58030"} {
		t.Run(code, func(t *testing.T) {
			pgErr := &pgconn.PgError{Code: code}
			got := wrapConnErr(pgErr)
			assert.ErrorIs(t, got, domain.ErrDBUnavailable)
		})
	}
}

// statement_timeout (57014) and lock_timeout (55P03) are slow-query
// signals: request_timeout, not db_unavailable.
func TestWrapConnErr_StatementOrLockTimeout_IsRequestTimeout(t *testing.T) {
	for _, code := range []string{"57014", "55P03"} {
		t.Run(code, func(t *testing.T) {
			got := wrapConnErr(&pgconn.PgError{Code: code})
			assert.ErrorIs(t, got, domain.ErrRequestTimeout)
			assert.NotErrorIs(t, got, domain.ErrDBUnavailable)
		})
	}
}

// A deadline that cuts pgcommon's deadlock/serialization retry wait short
// wraps BOTH errors ("retry wait … interrupted: %w (last error: %w)"); it is
// a timeout (503), never a conflict.
func TestWrapConnErr_DeadlineDuringRetryWait_IsRequestTimeout(t *testing.T) {
	interrupted := fmt.Errorf("pgcommon: retry wait after attempt %d interrupted: %w (last error: %w)",
		1, context.DeadlineExceeded, &pgconn.PgError{Code: "40P01"})
	got := wrapConnErr(interrupted)
	assert.ErrorIs(t, got, domain.ErrRequestTimeout)
	assert.NotErrorIs(t, got, domain.ErrConflict)

	assert.ErrorIs(t, wrapConnErr(context.DeadlineExceeded), domain.ErrRequestTimeout)
	assert.ErrorIs(t, wrapConnErr(fmt.Errorf("begin tx: %w", context.Canceled)), domain.ErrRequestTimeout)
}

// Retries exhausted on a deadlock / serialization failure is a concurrent
// write conflict the client can retry: 409 concurrent_update, not a 500.
func TestWrapConnErr_RetriesExhausted_IsConcurrentUpdateConflict(t *testing.T) {
	for _, code := range []string{"40P01", "40001"} {
		t.Run(code, func(t *testing.T) {
			pgErr := &pgconn.PgError{Code: code}
			exhausted := fmt.Errorf("pgcommon: transaction failed after %d attempts: %w", 3, pgErr)
			got := wrapConnErr(exhausted)
			assert.ErrorIs(t, got, domain.ErrConflict)
			assert.ErrorIs(t, got, pgErr, "the PgError stays inspectable")
			var de *domain.DomainError
			require.True(t, errors.As(got, &de))
			assert.Equal(t, "concurrent_update", de.Details["code"])
		})
	}
}

func TestWrapConnErr_ConstraintPgErrorPassesThroughUnchanged(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "23505"}
	got := wrapConnErr(pgErr)
	assert.Same(t, pgErr, got, "non-availability PgError must pass through, not get remapped")
}

func TestWrapConnErr_DomainErrorPassesThroughUnchanged(t *testing.T) {
	de := domain.NewError(domain.ErrValidation, "bad input")
	got := wrapConnErr(de)
	assert.Same(t, error(de), got)
}

func TestWrapConnErr_PgxNoRowsPassesThroughUnchanged(t *testing.T) {
	got := wrapConnErr(pgx.ErrNoRows)
	assert.ErrorIs(t, got, pgx.ErrNoRows)
}

// CRITICAL: an unrecognized plain Go error — the shape a caller's own
// RunInTx/withPool callback returns for its own business reasons — must
// pass through completely unchanged, not get silently reclassified as
// ErrDependencyUnavailable. wrapConnErr has no way to distinguish "the pool
// itself failed" from "fn's own business logic failed" for anything it
// can't positively identify as a connectivity/resource failure, so
// defaulting an unrecognized error to ErrDependencyUnavailable would
// discard the caller's real error under a misleading 503 — exactly the
// bug this test guards against (found and fixed in the same wrapConnErr
// pattern in the sibling iam-realm-provisioner service, then found here
// too on a cross-service alignment check).
func TestWrapConnErr_UnrecognizedGenericErrorPassesThroughUnchanged(t *testing.T) {
	businessErr := errors.New("dial tcp: connection refused")

	got := wrapConnErr(businessErr)

	assert.Same(t, businessErr, got)
	assert.ErrorIs(t, got, businessErr)
}

// A closed pool is the one non-PgError shape wrapConnErr positively
// recognizes as a connectivity failure (pgcommon.IsPoolClosed). pgcommon's
// own closed-pool error, which needs a live pool to produce, is covered in
// test/postgres.
func TestWrapConnErr_ClosedPoolMapsToDBUnavailable(t *testing.T) {
	assert.ErrorIs(t, wrapConnErr(puddle.ErrClosedPool), domain.ErrDBUnavailable)
}

// A context error becomes request_timeout but stays in the chain, so
// shutdown-aware callers can still match it.
func TestWrapConnErr_ContextCanceledStaysMatchable(t *testing.T) {
	got := wrapConnErr(context.Canceled)
	assert.ErrorIs(t, got, context.Canceled)
	assert.ErrorIs(t, got, domain.ErrRequestTimeout)
}

func TestWrapConnErr_DeadlineExceededStaysMatchable(t *testing.T) {
	got := wrapConnErr(context.DeadlineExceeded)
	assert.ErrorIs(t, got, context.DeadlineExceeded)
	assert.ErrorIs(t, got, domain.ErrRequestTimeout)
}

func TestWrapConnErr_NilPassesThrough(t *testing.T) {
	assert.NoError(t, wrapConnErr(nil))
}

// TestItoa_ZeroReturns100 covers the n <= 0 branch in itoa.
func TestItoa_ZeroReturns100(t *testing.T) {
	assert.Equal(t, "100", itoa(0))
	assert.Equal(t, "100", itoa(-5))
}

// TestSystemPoolConfig_NonNilLogger_SetsLoggerAdapter verifies the
// `if log != nil { cfg.Logger = NewLoggerAdapter(log) }` branch.
func TestSystemPoolConfig_NonNilLogger_SetsLoggerAdapter(t *testing.T) {
	cfg := SystemPoolConfig("postgres://u:p@h/db?sslmode=disable", &testLoggerAdapter{})
	assert.NotNil(t, cfg.Logger, "non-nil port.Logger must set cfg.Logger to a LoggerAdapter")
}

// testLoggerAdapter is a minimal port.Logger for SystemPoolConfig tests.
type testLoggerAdapter struct{}

// fakePortLogger is an alias for testLoggerAdapter used by SystemPoolConfig logger tests.
type fakePortLogger = testLoggerAdapter

func (l *testLoggerAdapter) Debug(msg string, fields map[string]any) {}
func (l *testLoggerAdapter) Info(msg string, fields map[string]any)  {}
func (l *testLoggerAdapter) Warn(msg string, fields map[string]any)  {}
func (l *testLoggerAdapter) Error(msg string, fields map[string]any) {}

var _ port.Logger = (*testLoggerAdapter)(nil)
