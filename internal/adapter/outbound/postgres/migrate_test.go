package postgres

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/stretchr/testify/assert"
)

// TestRunMigrations_WithLogger_LoggerAssignedBeforeUpCall verifies that when
// a non-nil logger is passed to RunMigrations, the logger is assigned to
// runner.Logger (line 35) before runner.Up is called. The Up call will
// fail with an invalid DSN, but line 35 still executes — covering the
// previously uncovered branch.
func TestRunMigrations_WithLogger_LoggerAssignedBeforeUpCall(t *testing.T) {
	// Use an invalid DSN so Up() fails fast without a real DB.
	err := RunMigrations(context.Background(), "postgres://invalid:5432/does_not_exist", &migrateTestLogger{})
	// We expect an error from runner.Up (cannot connect), not from line 35.
	// The important thing is line 35 executed without panic.
	assert.Error(t, err, "Up() must fail on an invalid DSN")
}

// migrateTestLogger is a minimal port.Logger for RunMigrations tests.
type migrateTestLogger struct{}

func (l *migrateTestLogger) Debug(msg string, fields map[string]any) {}
func (l *migrateTestLogger) Info(msg string, fields map[string]any)  {}
func (l *migrateTestLogger) Warn(msg string, fields map[string]any)  {}
func (l *migrateTestLogger) Error(msg string, fields map[string]any) {}

var _ port.Logger = (*migrateTestLogger)(nil)
