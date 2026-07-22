package postgres

import (
	"context"
	"embed"
	"io/fs"

	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/migrate"
)

// migrationsFS embeds every .sql file under migrations/. Files are numbered
// NNNNNN_<name>.up.sql / .down.sql per §19. Phase 0 ships an empty directory
// (with a .gitkeep) — Phase 1 populates it with the 18 initial migrations
// per §19.5 ordering.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// RunMigrations applies all pending domain migrations against dsn. Uses the
// direct Postgres DSN (bypassing PgBouncer, CONFIG-2) because the runner
// acquires a pg_advisory_lock which is session-scoped.
func RunMigrations(ctx context.Context, dsn string) error {
	// fs.Sub on an embedded FS with a known directory path is infallible;
	// an error here would be a build-time programming mistake.
	sub, _ := fs.Sub(migrationsFS, "migrations")
	return (&pgmigrate.Runner{FS: sub, DSN: dsn}).Up(ctx)
}
