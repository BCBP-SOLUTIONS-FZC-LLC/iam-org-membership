package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	pgdomain "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/domain"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/migrate"
)

// migrationsFS embeds every .sql file under migrations/. Files are numbered
// NNNNNN_<name>.up.sql / .down.sql per §19: the consolidated
// 000000_initial_schema plus the additive migrations after it.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// RunMigrations applies all pending domain migrations against dsn. Uses the
// direct Postgres DSN (bypassing PgBouncer, CONFIG-2) because the runner
// acquires a pg_advisory_lock which is session-scoped.
//
// log is optional (variadic so every existing call site keeps compiling
// unchanged) — when provided, each migration step is logged through it via
// LoggerAdapter.
func RunMigrations(ctx context.Context, dsn string, log ...port.Logger) error {
	// fs.Sub on an embedded FS with a known directory path is infallible;
	// an error here would be a build-time programming mistake.
	sub, _ := fs.Sub(migrationsFS, "migrations")
	runner := &pgmigrate.Runner{FS: sub, DSN: dsn}
	if len(log) > 0 && log[0] != nil {
		runner.Logger = NewLoggerAdapter(log[0])
	}
	return runner.Up(ctx)
}

// MigrateSchema brings the database to this build's schema: platform-events'
// outbox schema first (outbox_events, outbox_dead_letters — the library's
// own, left as it defines it), then the domain migrations, whose GRANTs on
// those tables apply only if they already exist. Both run over the direct
// DSN under pgmigrate's advisory lock.
//
// It is run once per release by the migrate Job (cmd/migrate, a Helm
// pre-install/pre-upgrade hook), not by every replica: with RUN_MIGRATIONS
// unset or true, cmd/server also runs it at startup (local dev, tests).
// pgmigrate.ErrMigrationDirty (a previous run failed mid-migration) and
// ErrVersionNotInSource (the database is newer than this image — e.g. a
// rollback to an older image) are wrapped with what to do next.
func MigrateSchema(ctx context.Context, dsn string, log port.Logger) error {
	var logger pgdomain.Logger
	if log != nil {
		logger = NewLoggerAdapter(log)
	}
	if err := outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: dsn, Logger: logger}); err != nil {
		return explainMigrationError("outbox schema", err)
	}
	if err := RunMigrations(ctx, dsn, log); err != nil {
		return explainMigrationError("domain migrations", err)
	}
	return nil
}

func explainMigrationError(stage string, err error) error {
	switch {
	case errors.Is(err, pgmigrate.ErrMigrationDirty):
		return fmt.Errorf("%s: a previous migration failed part-way and left the schema dirty — repair it by hand, then force the version (see docs/lld §19): %w", stage, err)
	case errors.Is(err, pgmigrate.ErrVersionNotInSource):
		return fmt.Errorf("%s: the database schema is newer than this image (rolled back to an older build?) — deploy a build that contains the applied migration, or run its down migration first: %w", stage, err)
	default:
		return fmt.Errorf("%s: %w", stage, err)
	}
}
