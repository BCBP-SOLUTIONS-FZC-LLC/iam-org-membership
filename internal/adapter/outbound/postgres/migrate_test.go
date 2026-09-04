package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// RunMigrations always fails fast against a connection-refused DSN (no
// Docker/live Postgres involved) — these tests only need to exercise the
// optional-logger plumbing branch (len(log) > 0 && log[0] != nil) and the
// no-logger call shape; the actual migration application is covered by the
// Docker-backed test/postgres suite via setupTestDB.
const badMigrationDSN = "postgres://baduser:badpass@127.0.0.1:1/nosuchdb?sslmode=disable"

func TestRunMigrations_NoLoggerReturnsErrorOnUnreachableDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := RunMigrations(ctx, badMigrationDSN)
	assert.Error(t, err)
}

func TestRunMigrations_WithLoggerWiresLoggerAdapterAndReturnsError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fake := &fakePortLogger{}
	err := RunMigrations(ctx, badMigrationDSN, fake)
	assert.Error(t, err, "unreachable DB must still surface an error with a logger wired")
}

func TestRunMigrations_NilLoggerInVariadicSkipsWiring(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := RunMigrations(ctx, badMigrationDSN, nil)
	assert.Error(t, err)
}
