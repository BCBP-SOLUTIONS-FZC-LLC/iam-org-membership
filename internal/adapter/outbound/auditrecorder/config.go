package auditrecorder

import (
	"os"

	auditconfig "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/config"
)

// SourceService is this service's AL-5 source_service. It must equal the
// name used everywhere else (the chart's serviceAccount.name, the principal
// in Audit Log's internalCallerPrincipals).
const SourceService = "iam-org-membership"

// LoadConfig reads the AUDIT_* variables (platform-audit LLD §12) with
// AUDIT_SOURCE_SERVICE defaulting to SourceService, so a local run without
// it still starts. A malformed value fails with an error naming the
// variable. AUDIT_LOG_BASE_URL empty disables delivery: entries queue in
// pending_audit_entries and the backlog gauges report them.
func LoadConfig() (auditconfig.Config, error) {
	return auditconfig.FromLookup("", func(name string) (string, bool) {
		v, ok := os.LookupEnv(name)
		if name == auditconfig.EnvSourceService && v == "" {
			return SourceService, true
		}
		return v, ok
	})
}
