package postgres

import (
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/domain"
)

// LoggerAdapter implements platform-pgcommon's pkg/domain.Logger (Debug/Info/
// Warn/Error(msg, ...domain.Field)) on top of port.Logger. Before pgcommon
// v1.2.0, Config.Logger/migrate.Runner.Logger were typed against pgcommon's
// unexported internal port.Logger — whose methods took a struct from an
// internal package — making them structurally impossible for any external
// module to implement. v1.2.0 retyped those fields against the new public
// domain.Logger, so pgcommon's slow-query logging and migration log output
// can now be routed into this service's own gincommon-backed sink (see
// internal/core/port.Logger) instead of going nowhere.
type LoggerAdapter struct {
	log port.Logger
}

var _ domain.Logger = LoggerAdapter{}

// NewLoggerAdapter wraps log as a pgcommon domain.Logger.
func NewLoggerAdapter(log port.Logger) LoggerAdapter {
	return LoggerAdapter{log: log}
}

func (a LoggerAdapter) Debug(msg string, fields ...domain.Field) { a.log.Debug(msg, fieldMap(fields)) }
func (a LoggerAdapter) Info(msg string, fields ...domain.Field)  { a.log.Info(msg, fieldMap(fields)) }
func (a LoggerAdapter) Warn(msg string, fields ...domain.Field)  { a.log.Warn(msg, fieldMap(fields)) }
func (a LoggerAdapter) Error(msg string, fields ...domain.Field) { a.log.Error(msg, fieldMap(fields)) }

func fieldMap(fields []domain.Field) map[string]any {
	m := make(map[string]any, len(fields))
	for _, f := range fields {
		m[f.Key] = f.Value
	}
	return m
}
