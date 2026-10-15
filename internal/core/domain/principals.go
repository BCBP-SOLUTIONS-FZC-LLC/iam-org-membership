package domain

import "github.com/google/uuid"

// Reserved system-principal UUIDs (CD-19). These identities are system
// service accounts, never real human users. They must be rejected by every
// membership-create, role-grant, and dept-membership-assign path even when
// the Token Service's AUTH-9 call fails open.
//
// Values MUST stay in sync with pkg/requestctx.IamSystemSentinelUUID and
// pkg/requestctx.CommercialSystemSentinelUUID.
var (
	// reservedIAMSystem is the GUC identity for IAM-internal operations
	// (maps to uuid.Nil in parseBridgedIdentity).
	reservedIAMSystem = uuid.Nil

	// reservedCommercialSystem is the sentinel UUID for the commercial-system
	// principal introduced in CD-19 (Billing, Entitlements callers).
	reservedCommercialSystem = uuid.MustParse("00000000-0000-0000-0000-0000000000c1")
)

// IsReservedSystemPrincipal reports whether id belongs to a reserved service
// identity that must never be enrolled as a member or granted a role. Returns
// true for both the iam-system sentinel (uuid.Nil) and the commercial-system
// sentinel. Called as a hard pre-check in invitation/membership/dept-membership
// service methods so that AUTH-9's Token Service fail-open path cannot
// accidentally allow enrollment of these principals.
func IsReservedSystemPrincipal(id uuid.UUID) bool {
	return id == reservedIAMSystem || id == reservedCommercialSystem
}
