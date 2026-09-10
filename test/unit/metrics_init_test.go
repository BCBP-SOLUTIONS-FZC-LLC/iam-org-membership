// Registers the shared Prometheus metrics collectors once for this test
// binary. Several service-layer branches only increment a counter when it's
// non-nil (e.g. membership_service.go SetStatus's WFI-13
// metrics.DelegateSuspendImpact.Inc() calls) — outside a real server these
// vars stay nil (registerMetrics is gated behind metrics.Register's
// sync.Once, invoked only from cmd/server/cmd/reconciler in production), so
// those Inc() lines are otherwise unreachable from this package's tests.
// Registering here (once, via metrics.Register's own sync.Once) lets any
// test in this binary exercise those branches through its normal fakes,
// without each test needing to know about metrics wiring itself.
package unit_test

import (
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
)

func init() {
	metrics.Register("test")
}
