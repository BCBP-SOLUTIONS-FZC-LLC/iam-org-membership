package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
	"github.com/stretchr/testify/assert"
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

func TestDSNFromEnv_AppendsStatementTimeoutWhenSet(t *testing.T) {
	_ = os.Unsetenv("DATABASE_URL")
	t.Setenv("PG_USER", "org_membership_app")
	t.Setenv("PG_DBNAME", "org_membership")
	t.Setenv("PG_STATEMENT_TIMEOUT", "5s")
	dsn := DSNFromEnv()
	// Encoded as options=-c statement_timeout=5000 (ms).
	assert.Contains(t, dsn, "statement_timeout%3D5000",
		"5s must translate to statement_timeout=5000 in URL-encoded options")
}

func TestDSNFromEnv_InvalidStatementTimeoutIgnored(t *testing.T) {
	_ = os.Unsetenv("DATABASE_URL")
	t.Setenv("PG_USER", "org_membership_app")
	t.Setenv("PG_DBNAME", "org_membership")
	t.Setenv("PG_STATEMENT_TIMEOUT", "not-a-duration")
	dsn := DSNFromEnv()
	assert.NotContains(t, dsn, "statement_timeout",
		"invalid duration must silently fall through, not crash the process")
}

func TestDSNFromEnv_ZeroStatementTimeoutIgnored(t *testing.T) {
	_ = os.Unsetenv("DATABASE_URL")
	t.Setenv("PG_USER", "org_membership_app")
	t.Setenv("PG_DBNAME", "org_membership")
	t.Setenv("PG_STATEMENT_TIMEOUT", "0s")
	dsn := DSNFromEnv()
	assert.NotContains(t, dsn, "statement_timeout",
		"zero timeout is a no-op — the code requires d > 0")
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

func TestMigrationDSNFromEnv_AppliesStatementTimeout(t *testing.T) {
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://m:x@migrations.example:5432/omdb?sslmode=disable")
	t.Setenv("PG_STATEMENT_TIMEOUT", "5s")
	assert.Contains(t, MigrationDSNFromEnv(), "statement_timeout%3D5000")
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

func TestSystemPoolConfig_AppliesStatementTimeout(t *testing.T) {
	t.Setenv("PG_STATEMENT_TIMEOUT", "5s")
	cfg := SystemPoolConfig("postgres://sys@host/db?sslmode=disable", nil)
	assert.Contains(t, cfg.DSN, "statement_timeout%3D5000")
}

func TestApplyStatementTimeout_Idempotent(t *testing.T) {
	t.Setenv("PG_STATEMENT_TIMEOUT", "5s")
	once := ApplyStatementTimeout("postgres://u@h/db?sslmode=disable")
	assert.Equal(t, once, ApplyStatementTimeout(once))
}

// ── wrapConnErr ──────────────────────────────────────────────────────────

// wrapConnErr remaps SQLSTATE class 08/53/57/58 (availability failures)
// to domain.ErrDBUnavailable. Other PgErrors (e.g. 23505 unique_violation)
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

// puddle.ErrClosedPool (surfaced by pgxpool.Pool.BeginTx/Acquire on a
// closed pool) is the one non-PgError shape wrapConnErr does positively
// recognize as a genuine connectivity failure — a closed pool is never a
// SQL-protocol response, but is unambiguously "the database is
// unavailable", not a caller's business error.
func TestWrapConnErr_ClosedPoolMapsToDBUnavailable(t *testing.T) {
	got := wrapConnErr(puddle.ErrClosedPool)
	assert.ErrorIs(t, got, domain.ErrDBUnavailable)
}

func TestWrapConnErr_ContextCanceledPassesThroughUnchanged(t *testing.T) {
	got := wrapConnErr(context.Canceled)
	assert.ErrorIs(t, got, context.Canceled)
}

func TestWrapConnErr_DeadlineExceededPassesThroughUnchanged(t *testing.T) {
	got := wrapConnErr(context.DeadlineExceeded)
	assert.ErrorIs(t, got, context.DeadlineExceeded)
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

func (l *testLoggerAdapter) Debug(msg string, fields map[string]any) {}
func (l *testLoggerAdapter) Info(msg string, fields map[string]any)  {}
func (l *testLoggerAdapter) Warn(msg string, fields map[string]any)  {}
func (l *testLoggerAdapter) Error(msg string, fields map[string]any) {}

var _ port.Logger = (*testLoggerAdapter)(nil)
