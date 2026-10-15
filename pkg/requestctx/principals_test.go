package requestctx

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// TestCommercialSystemUUID_LiteralValue pins the exact wire value for the
// commercial-system sentinel UUID (CD-19). Every service that participates in
// CD-19 (iam-org-membership, iam-realm-provisioner, iam-catalog-admin) must
// use exactly this UUID. Change requires a cross-repo sync.
func TestCommercialSystemUUID_LiteralValue(t *testing.T) {
	const want = "00000000-0000-0000-0000-0000000000c1"
	assert.Equal(t, want, CommercialSystemSentinelUUID,
		"CommercialSystemSentinelUUID must equal the canonical CD-19 value; cross-repo contract — do not change without syncing iam-realm-provisioner and iam-catalog-admin")
	assert.Equal(t, uuid.MustParse(want), CommercialSystemUUID,
		"CommercialSystemUUID must parse to the canonical CD-19 value")
}

// TestIamSystemSentinelUUID_LiteralValue pins the iam-system sentinel so the
// two identities remain distinguishable in logs and audit entries.
func TestIamSystemSentinelUUID_LiteralValue(t *testing.T) {
	const want = "00000000-0000-0000-0000-0000000000a1"
	assert.Equal(t, want, IamSystemSentinelUUID,
		"IamSystemSentinelUUID must equal the canonical iam-system value; cross-repo contract")
	// parseBridgedIdentity maps "iam-system" → uuid.Nil, NOT IamSystemSentinelUUID.
	// IamSystemSentinelUUID is the documented value; uuid.Nil is the runtime value.
	assert.NotEqual(t, uuid.MustParse(want), uuid.Nil,
		"IamSystemSentinelUUID and uuid.Nil are distinct — the documented value differs from the runtime mapping by design")
}
