package postgres

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// ── envOrDB ────────────────────────────────────────────────────────────

func TestEnvOrDB_ReturnsValueWhenSet(t *testing.T) {
	t.Setenv("PG_TEST_KEY_1", "custom")
	assert.Equal(t, "custom", envOrDB("PG_TEST_KEY_1", "default"))
}

func TestEnvOrDB_ReturnsDefaultWhenUnset(t *testing.T) {
	_ = os.Unsetenv("PG_TEST_KEY_UNSET")
	assert.Equal(t, "default", envOrDB("PG_TEST_KEY_UNSET", "default"))
}

func TestEnvOrDB_EmptyStringTreatedAsUnset(t *testing.T) {
	t.Setenv("PG_TEST_KEY_2", "")
	assert.Equal(t, "fallback", envOrDB("PG_TEST_KEY_2", "fallback"))
}

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

func TestDSNFromEnv_DefaultsWhenNothingSet(t *testing.T) {
	// Unset every input — DSN should use the coded defaults.
	_ = os.Unsetenv("DATABASE_URL")
	_ = os.Unsetenv("PG_HOST")
	_ = os.Unsetenv("PG_PORT")
	_ = os.Unsetenv("PG_USER")
	_ = os.Unsetenv("PG_PASSWORD")
	_ = os.Unsetenv("PG_DBNAME")
	_ = os.Unsetenv("PG_SSLMODE")
	_ = os.Unsetenv("PG_STATEMENT_TIMEOUT")

	dsn := DSNFromEnv()
	assert.Contains(t, dsn, "@localhost:5432/org_membership")
	assert.Contains(t, dsn, "sslmode=require",
		"default sslmode is 'require' — safe default")
}

func TestDSNFromEnv_AppendsStatementTimeoutWhenSet(t *testing.T) {
	_ = os.Unsetenv("DATABASE_URL")
	t.Setenv("PG_STATEMENT_TIMEOUT", "5s")
	dsn := DSNFromEnv()
	// Encoded as options=-c statement_timeout=5000 (ms).
	assert.Contains(t, dsn, "statement_timeout%3D5000",
		"5s must translate to statement_timeout=5000 in URL-encoded options")
}

func TestDSNFromEnv_InvalidStatementTimeoutIgnored(t *testing.T) {
	_ = os.Unsetenv("DATABASE_URL")
	t.Setenv("PG_STATEMENT_TIMEOUT", "not-a-duration")
	dsn := DSNFromEnv()
	assert.NotContains(t, dsn, "statement_timeout",
		"invalid duration must silently fall through, not crash the process")
}

func TestDSNFromEnv_ZeroStatementTimeoutIgnored(t *testing.T) {
	_ = os.Unsetenv("DATABASE_URL")
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
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://m:x@migrations.example:5432/omdb")
	assert.Equal(t,
		"postgres://m:x@migrations.example:5432/omdb",
		MigrationDSNFromEnv())
}

func TestMigrationDSNFromEnv_FallsBackToDSNFromEnv(t *testing.T) {
	_ = os.Unsetenv("MIGRATION_DATABASE_URL")
	t.Setenv("DATABASE_URL", "postgres://a:b@app.example:5432/omdb")
	assert.Equal(t, DSNFromEnv(), MigrationDSNFromEnv(),
		"unset MIGRATION_DATABASE_URL falls through to the app DSN")
}
