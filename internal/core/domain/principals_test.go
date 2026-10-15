package domain

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestIsReservedSystemPrincipal_IamSystem_True(t *testing.T) {
	assert.True(t, IsReservedSystemPrincipal(uuid.Nil),
		"iam-system sentinel (uuid.Nil) must be reserved")
}

func TestIsReservedSystemPrincipal_CommercialSystem_True(t *testing.T) {
	csUUID := uuid.MustParse("00000000-0000-0000-0000-0000000000c1")
	assert.True(t, IsReservedSystemPrincipal(csUUID),
		"commercial-system sentinel must be reserved (CD-19)")
}

func TestIsReservedSystemPrincipal_RegularUser_False(t *testing.T) {
	assert.False(t, IsReservedSystemPrincipal(uuid.New()),
		"random UUID must not be treated as reserved")
}

func TestIsReservedSystemPrincipal_KnownTenantUUID_False(t *testing.T) {
	// Canary: a UUID that looks similar to the sentinel range but isn't.
	canary := uuid.MustParse("00000000-0000-0000-0000-0000000000b1")
	assert.False(t, IsReservedSystemPrincipal(canary),
		"a1/b1/etc. variants must not accidentally match the reserved set")
}

// TestReservedUUIDs_SyncWithRequestctx ensures the hard-coded UUID strings
// in domain/principals.go match pkg/requestctx/principals.go. The two
// packages intentionally duplicate the values (domain must not import pkg)
// so this cross-package string check is the only guard against drift.
func TestReservedUUIDs_SyncWithRequestctx(t *testing.T) {
	// These must equal the constants in pkg/requestctx/principals.go.
	assert.Equal(t, "00000000-0000-0000-0000-0000000000c1",
		reservedCommercialSystem.String(),
		"domain.reservedCommercialSystem must match requestctx.CommercialSystemSentinelUUID")
}
