// Package postgres — unit tests for LoggerAdapter (logger_adapter.go).
// LoggerAdapter wraps port.Logger and satisfies domain.Logger (pgcommon
// v1.2.0's public sink). Tests verify each log-level method routes through
// the underlying port.Logger with the correct message and field conversion.
// No Docker / testcontainers required.
package postgres

import (
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureLogger is a test double for port.Logger that records calls.
type captureLogger struct {
	debugCalls []callRecord
	infoCalls  []callRecord
	warnCalls  []callRecord
	errorCalls []callRecord
}

type callRecord struct {
	msg    string
	fields map[string]any
}

func (l *captureLogger) Debug(msg string, fields map[string]any) {
	l.debugCalls = append(l.debugCalls, callRecord{msg, fields})
}
func (l *captureLogger) Info(msg string, fields map[string]any) {
	l.infoCalls = append(l.infoCalls, callRecord{msg, fields})
}
func (l *captureLogger) Warn(msg string, fields map[string]any) {
	l.warnCalls = append(l.warnCalls, callRecord{msg, fields})
}
func (l *captureLogger) Error(msg string, fields map[string]any) {
	l.errorCalls = append(l.errorCalls, callRecord{msg, fields})
}

// TestLoggerAdapter_Debug routes to Debug on the underlying Logger.
func TestLoggerAdapter_Debug(t *testing.T) {
	log := &captureLogger{}
	a := NewLoggerAdapter(log)

	a.Debug("debug msg", domain.Field{Key: "k", Value: "v"})

	require.Len(t, log.debugCalls, 1, "Debug must forward exactly one call")
	assert.Equal(t, "debug msg", log.debugCalls[0].msg)
	assert.Equal(t, "v", log.debugCalls[0].fields["k"])
}

// TestLoggerAdapter_Info routes to Info on the underlying Logger.
func TestLoggerAdapter_Info(t *testing.T) {
	log := &captureLogger{}
	a := NewLoggerAdapter(log)

	a.Info("info msg", domain.Field{Key: "x", Value: 42})

	require.Len(t, log.infoCalls, 1)
	assert.Equal(t, "info msg", log.infoCalls[0].msg)
	assert.Equal(t, 42, log.infoCalls[0].fields["x"])
}

// TestLoggerAdapter_Warn routes to Warn on the underlying Logger.
func TestLoggerAdapter_Warn(t *testing.T) {
	log := &captureLogger{}
	a := NewLoggerAdapter(log)

	a.Warn("warn msg", domain.Field{Key: "a", Value: true})

	require.Len(t, log.warnCalls, 1)
	assert.Equal(t, "warn msg", log.warnCalls[0].msg)
	assert.Equal(t, true, log.warnCalls[0].fields["a"])
}

// TestLoggerAdapter_Error routes to Error on the underlying Logger.
func TestLoggerAdapter_Error(t *testing.T) {
	log := &captureLogger{}
	a := NewLoggerAdapter(log)

	a.Error("error msg", domain.Field{Key: "err", Value: "something broke"})

	require.Len(t, log.errorCalls, 1)
	assert.Equal(t, "error msg", log.errorCalls[0].msg)
	assert.Equal(t, "something broke", log.errorCalls[0].fields["err"])
}

// TestLoggerAdapter_NoFields verifies that zero fields produces an empty (but
// non-nil) map rather than panicking.
func TestLoggerAdapter_NoFields(t *testing.T) {
	log := &captureLogger{}
	a := NewLoggerAdapter(log)

	assert.NotPanics(t, func() {
		a.Info("no fields")
	})

	require.Len(t, log.infoCalls, 1)
	assert.NotNil(t, log.infoCalls[0].fields)
	assert.Empty(t, log.infoCalls[0].fields)
}

// TestLoggerAdapter_MultipleFields verifies that all fields in a varargs call
// are converted to the fields map correctly.
func TestLoggerAdapter_MultipleFields(t *testing.T) {
	log := &captureLogger{}
	a := NewLoggerAdapter(log)

	a.Debug("multi",
		domain.Field{Key: "one", Value: 1},
		domain.Field{Key: "two", Value: "dos"},
		domain.Field{Key: "three", Value: true},
	)

	require.Len(t, log.debugCalls, 1)
	fields := log.debugCalls[0].fields
	assert.Equal(t, 1, fields["one"])
	assert.Equal(t, "dos", fields["two"])
	assert.Equal(t, true, fields["three"])
}

// TestLoggerAdapter_ImplementsDomainLogger verifies the adapter satisfies the
// domain.Logger interface at compile time.
func TestLoggerAdapter_ImplementsDomainLogger(t *testing.T) {
	log := &captureLogger{}
	var _ domain.Logger = NewLoggerAdapter(log)
}
