package postgres

import (
	"testing"

	pgdomain "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePortLogger is a scripted port.Logger — records the last call made to
// each level method so tests can assert msg/fields were forwarded
// unchanged (modulo fieldMap's []Field -> map[string]any conversion).
type fakePortLogger struct {
	lastMsg    string
	lastFields map[string]any
	lastLevel  string
}

func (f *fakePortLogger) Debug(msg string, fields map[string]any) {
	f.lastLevel, f.lastMsg, f.lastFields = "debug", msg, fields
}
func (f *fakePortLogger) Info(msg string, fields map[string]any) {
	f.lastLevel, f.lastMsg, f.lastFields = "info", msg, fields
}
func (f *fakePortLogger) Warn(msg string, fields map[string]any) {
	f.lastLevel, f.lastMsg, f.lastFields = "warn", msg, fields
}
func (f *fakePortLogger) Error(msg string, fields map[string]any) {
	f.lastLevel, f.lastMsg, f.lastFields = "error", msg, fields
}

func TestNewLoggerAdapter_ImplementsPgcommonDomainLogger(t *testing.T) {
	var _ pgdomain.Logger = NewLoggerAdapter(&fakePortLogger{})
}

func TestLoggerAdapter_Debug_ForwardsMsgAndFieldMap(t *testing.T) {
	fake := &fakePortLogger{}
	adapter := NewLoggerAdapter(fake)

	adapter.Debug("slow query", pgdomain.Field{Key: "duration_ms", Value: 42})

	assert.Equal(t, "debug", fake.lastLevel)
	assert.Equal(t, "slow query", fake.lastMsg)
	require.Contains(t, fake.lastFields, "duration_ms")
	assert.Equal(t, 42, fake.lastFields["duration_ms"])
}

func TestLoggerAdapter_Info_ForwardsMsgAndFieldMap(t *testing.T) {
	fake := &fakePortLogger{}
	adapter := NewLoggerAdapter(fake)

	adapter.Info("migration applied", pgdomain.Field{Key: "step", Value: "000000_initial_schema"})

	assert.Equal(t, "info", fake.lastLevel)
	assert.Equal(t, "migration applied", fake.lastMsg)
	assert.Equal(t, "000000_initial_schema", fake.lastFields["step"])
}

func TestLoggerAdapter_Warn_ForwardsMsgAndFieldMap(t *testing.T) {
	fake := &fakePortLogger{}
	adapter := NewLoggerAdapter(fake)

	adapter.Warn("retrying", pgdomain.Field{Key: "attempt", Value: 2})

	assert.Equal(t, "warn", fake.lastLevel)
	assert.Equal(t, "retrying", fake.lastMsg)
	assert.Equal(t, 2, fake.lastFields["attempt"])
}

func TestLoggerAdapter_Error_ForwardsMsgAndFieldMap(t *testing.T) {
	fake := &fakePortLogger{}
	adapter := NewLoggerAdapter(fake)

	adapter.Error("migration failed", pgdomain.Field{Key: "err", Value: "boom"})

	assert.Equal(t, "error", fake.lastLevel)
	assert.Equal(t, "migration failed", fake.lastMsg)
	assert.Equal(t, "boom", fake.lastFields["err"])
}

func TestLoggerAdapter_NoFieldsProducesEmptyMap(t *testing.T) {
	fake := &fakePortLogger{}
	adapter := NewLoggerAdapter(fake)

	adapter.Info("no fields here")

	require.NotNil(t, fake.lastFields)
	assert.Empty(t, fake.lastFields)
}

func TestLoggerAdapter_MultipleFieldsAllMapped(t *testing.T) {
	fake := &fakePortLogger{}
	adapter := NewLoggerAdapter(fake)

	adapter.Warn("multi",
		pgdomain.Field{Key: "a", Value: 1},
		pgdomain.Field{Key: "b", Value: "two"},
		pgdomain.Field{Key: "c", Value: true},
	)

	assert.Equal(t, map[string]any{"a": 1, "b": "two", "c": true}, fake.lastFields)
}
