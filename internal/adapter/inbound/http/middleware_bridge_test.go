package http

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// RegisterValidators is a no-op today (validators land in a later phase).
// Calling it must not panic and must be safe to call multiple times.
func TestRegisterValidators_NoOpNoPanic(t *testing.T) {
	assert.NotPanics(t, RegisterValidators)
	assert.NotPanics(t, RegisterValidators) // idempotent
}

// Note on GUCBridgeMiddleware: it depends on gincommon.RequestContext(c),
// which requires the platform's ContextMiddleware to have populated the
// gin context with a *domain.RequestContext from the gincommon internal
// package (unreachable from external tests). The middleware is exercised
// end-to-end in test/postgres and test/integration; unit-level testing
// would require constructing gincommon's internal type.
