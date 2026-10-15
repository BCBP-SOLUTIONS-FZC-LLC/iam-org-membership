package requestctx

import "github.com/google/uuid"

// Reserved principal identifiers used across the service's auth middleware
// and GUC bindings. The string values double as x-tenant-roles header values
// and as GUC app.user_id labels on internal writes.
const (
	// IamSystemPrincipal is the reserved role string for IAM-internal callers
	// (Realm Provisioner, Event Consumer, reconcilers). Accepted on every
	// /api/v1/internal/* route (AUTH-5 / IAPI-2 / RLS-5).
	IamSystemPrincipal = "iam-system"

	// CommercialSystemPrincipal is the reserved role string for Commercial
	// domain callers (Billing, Entitlements). Accepted ONLY on the three
	// routes listed in commercialSystemAllowedRoutes (CD-19).
	CommercialSystemPrincipal = "commercial-system"

	// IamSystemSentinelUUID is the fixed UUID that parseBridgedIdentity maps
	// the "iam-system" user-id string to. uuid.Nil in Go; documented as the
	// cross-repo contract in internal_handler.go (GAP-TAC-OM-15 / DLG-OM-GAP-004).
	IamSystemSentinelUUID = "00000000-0000-0000-0000-0000000000a1"

	// CommercialSystemSentinelUUID is the fixed UUID assigned to the
	// commercial-system principal. Chosen outside the iam-system range (a1…)
	// so the two identities are distinguishable in logs and audit entries.
	CommercialSystemSentinelUUID = "00000000-0000-0000-0000-0000000000c1"
)

// CommercialSystemUUID is the sentinel UUID for commercial-system callers,
// exported so domain/service code can guard against it being enrolled as a member.
var CommercialSystemUUID = uuid.MustParse(CommercialSystemSentinelUUID)
