package service

// CD-19 guard tests: reserved system principals (iam-system and
// commercial-system) must be rejected by every membership-create, role-grant,
// and dept-membership-assign path. The TOKEN SERVICE's AUTH-9 check is
// fail-open; IsReservedSystemPrincipal is the hard pre-check that runs
// before any Token Service call, ensuring reserved principals can never
// be enrolled even when Token Service is down.

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tokenServiceAlwaysNotSA is a token service stub that always says "not a
// service account". This simulates the Token Service being configured but
// not knowing about the principal (its normal behaviour for unknown UUIDs).
type tokenServiceAlwaysNotSA struct{}

func (t *tokenServiceAlwaysNotSA) IsServiceAccount(_ context.Context, _, _ uuid.UUID) (bool, error) {
	return false, nil
}

// ── isServiceAccountOrDegrade — reserved principal pre-check ─────────────

func TestInvitationService_ReservedPrincipal_IamSystem_IsBlockedBeforeTokenService(t *testing.T) {
	svc := &InvitationService{tokenService: &tokenServiceAlwaysNotSA{}}
	// uuid.Nil is the iam-system sentinel.
	result := svc.isServiceAccountOrDegrade(context.Background(), uuid.New(), uuid.Nil)
	assert.True(t, result,
		"iam-system UUID must be reported as a service account before Token Service is called")
}

func TestInvitationService_ReservedPrincipal_CommercialSystem_IsBlockedBeforeTokenService(t *testing.T) {
	svc := &InvitationService{tokenService: &tokenServiceAlwaysNotSA{}}
	result := svc.isServiceAccountOrDegrade(context.Background(), uuid.New(), requestctx.CommercialSystemUUID)
	assert.True(t, result,
		"commercial-system UUID (CD-19) must be reported as a service account before Token Service is called")
}

func TestMembershipService_ReservedPrincipal_CommercialSystem_IsBlockedBeforeTokenService(t *testing.T) {
	svc := &MembershipService{tokenService: &tokenServiceAlwaysNotSA{}}
	result := svc.isServiceAccountOrDegrade(context.Background(), uuid.New(), requestctx.CommercialSystemUUID)
	assert.True(t, result,
		"commercial-system UUID must be blocked by MembershipService guard (CD-19)")
}

func TestDeptMembershipService_ReservedPrincipal_CommercialSystem_IsBlockedBeforeTokenService(t *testing.T) {
	svc := &DeptMembershipService{tokenService: &tokenServiceAlwaysNotSA{}}
	result := svc.isServiceAccountOrDegrade(context.Background(), uuid.New(), requestctx.CommercialSystemUUID)
	assert.True(t, result,
		"commercial-system UUID must be blocked by DeptMembershipService guard (CD-19)")
}

// TestReservedPrincipal_RegularUser_NotBlocked confirms a real user UUID
// is NOT incorrectly blocked by the reserved-principal pre-check.
func TestReservedPrincipal_RegularUser_NotBlocked(t *testing.T) {
	svc := &InvitationService{tokenService: &tokenServiceAlwaysNotSA{}}
	result := svc.isServiceAccountOrDegrade(context.Background(), uuid.New(), uuid.New())
	// tokenServiceAlwaysNotSA returns false → real user passes through.
	assert.False(t, result, "a random user UUID must not be blocked by the reserved-principal guard")
}

// TestIsReservedSystemPrincipal_IsConsistentWithRequestctxConstants ensures
// the domain helper and the requestctx package agree on which UUIDs are reserved.
func TestIsReservedSystemPrincipal_IsConsistentWithRequestctxConstants(t *testing.T) {
	require.True(t, domain.IsReservedSystemPrincipal(uuid.Nil),
		"uuid.Nil (iam-system) must be reserved in domain package")
	require.True(t, domain.IsReservedSystemPrincipal(requestctx.CommercialSystemUUID),
		"CommercialSystemUUID must be reserved in domain package (CD-19)")
}
