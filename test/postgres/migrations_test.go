//go:build integration

// Phase 18 · 0%-units sweep — migration rollback sanity. Every .up.sql
// under internal/adapter/outbound/postgres/migrations/ has a .down.sql
// sibling; this test proves that the down migrations actually work by
// applying up, rolling back N steps, then re-applying up. If any down
// migration is broken (missing table drop, orphaned enum, etc.) the
// second up would fail.
package postgres_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/migrate"
)

// TestUpDownUpRoundTrips — apply all migrations up, roll back
// every single one, then re-apply up. Proves each .down.sql is a valid
// inverse of its .up.sql (no dangling constraints / enums / roles).
func TestUpDownUpRoundTrips(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping migration round-trip in short mode")
	}
	// setupTestDB gives us a fresh Postgres + a superuser DSN via the raw
	// pool's connection string, plus applies migrations up. Grab the DSN
	// from an env-hosted assertion by re-running setup with a distinct
	// call so we can drive the migrate.Runner ourselves.
	_, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	dsn := os.Getenv("PHASE18_MIG_DSN")
	if dsn == "" {
		// Fall back: extract from the raw pool's config.
		dsn = rawPool.Config().ConnConfig.ConnString()
	}

	migFS := loadMigrationDir(t)
	runner := &pgmigrate.Runner{FS: migFS, DSN: dsn}

	// Count how many up migrations exist so we can roll back exactly that
	// many. Every .up.sql = one migration step.
	upCount := countUpFiles(t, migFS)
	require.Positive(t, upCount, "must have at least one up migration to roll back")

	// Roll all the way back. Migration numbering starts at 000000 so the
	// runner is happy running the full set down.
	require.NoError(t, runner.Down(ctx, upCount),
		"P18-MIG-001: rolling all migrations back must succeed")

	// Re-apply up — this only works if every .down.sql cleanly undid its
	// .up.sql; leftover DDL (dangling type, orphan constraint) would
	// collide on the second up.
	require.NoError(t, runner.Up(ctx),
		"P18-MIG-001: re-applying migrations after a full down must succeed")

	// Sanity: the tenants table exists and is queryable post round-trip.
	var n int
	require.NoError(t, rawPool.QueryRow(ctx, `SELECT count(*) FROM tenants`).Scan(&n))
	assert.Equal(t, 0, n, "fresh table must be empty after up→down→up cycle")
}

// TestEveryUpHasDownSibling — pairing check. Rule: every
// NNNNNN_<name>.up.sql must have a matching .down.sql. A missing down
// file would break the round-trip test above but also blocks any real
// production rollback.
func TestEveryUpHasDownSibling(t *testing.T) {
	t.Parallel()
	migFS := loadMigrationDir(t)
	entries, err := fs.ReadDir(migFS, ".")
	require.NoError(t, err)

	seen := map[string]struct{ up, down bool }{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		var base string
		switch {
		case len(name) > 7 && name[len(name)-7:] == ".up.sql":
			base = name[:len(name)-7]
			m := seen[base]
			m.up = true
			seen[base] = m
		case len(name) > 9 && name[len(name)-9:] == ".down.sql":
			base = name[:len(name)-9]
			m := seen[base]
			m.down = true
			seen[base] = m
		}
	}

	for base, pair := range seen {
		assert.True(t, pair.up, "%s: .up.sql missing", base)
		assert.True(t, pair.down, "%s: .down.sql missing — every up must have a down", base)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

// loadMigrationDir mounts internal/adapter/outbound/postgres/migrations
// from the working directory as a fs.FS. Tests run from the package
// directory (test/postgres), so we walk up to the repo root.
func loadMigrationDir(t testing.TB) fs.FS {
	t.Helper()
	cwd, err := os.Getwd()
	require.NoError(t, err)
	// From test/postgres → repo root is 2 dirs up.
	root := filepath.Join(cwd, "..", "..")
	migPath := filepath.Join(root, "internal", "adapter", "outbound", "postgres", "migrations")
	info, err := os.Stat(migPath)
	require.NoError(t, err)
	require.True(t, info.IsDir())
	return os.DirFS(migPath)
}

func countUpFiles(t testing.TB, migFS fs.FS) int {
	t.Helper()
	entries, err := fs.ReadDir(migFS, ".")
	require.NoError(t, err)
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) > 7 && name[len(name)-7:] == ".up.sql" {
			n++
		}
	}
	return n
}
