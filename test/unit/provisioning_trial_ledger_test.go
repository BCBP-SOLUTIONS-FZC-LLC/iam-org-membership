// Unit tests for I-1's half of CR-4 and its seat input: the email hash is
// validated, lower-cased and claimed for the tenant inside the I-1
// transaction — also on a replay, so a tenant first created without the
// hash still records it — and a hash held by another tenant fails I-1.
package unit_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type claim struct {
	hash   string
	tenant uuid.UUID
}

type tlLedger struct {
	claims []claim
	err    error
}

func (l *tlLedger) ExistsEmailHash(context.Context, string) (bool, error) { return false, nil }
func (l *tlLedger) ClaimEmailHash(_ context.Context, h string, tenantID uuid.UUID) error {
	l.claims = append(l.claims, claim{h, tenantID})
	return l.err
}
func (l *tlLedger) PruneCreatedBefore(context.Context, time.Time, int) (int64, error) { return 0, nil }

var _ port.TrialSignupLedgerRepository = (*tlLedger)(nil)

var tlHash = strings.Repeat("0f", 32)

func TestTrialSignup_ClaimsLowerCasedHashForTenant(t *testing.T) {
	svc, _ := buildTrialProvisioningSvc(allSystemDepts())
	ledger := &tlLedger{}
	svc.WithTrialLedger(ledger)
	in := trialSignupInput()
	in.EmailHash = strings.ToUpper(tlHash)

	_, created, err := svc.TrialSignup(context.Background(), in)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, []claim{{tlHash, in.TenantID}}, ledger.claims)
}

// The backstop (or RP) created the tenant without a hash; the BFF's I-1
// replay must still record it.
func TestTrialSignup_Replay_StillClaimsHash(t *testing.T) {
	svc := service.NewProvisioningService(
		&ptdTenantRepoReplay{}, &ptdMembershipRepo{}, &ptdRoleRepo{}, nil,
		&ptdLabelRepo{}, &ptdTenantDeptRepo{}, allSystemDepts(), &ptdCatalogPlans{},
		&passthroughTxRunner{}, nil, nil,
	)
	ledger := &tlLedger{}
	svc.WithTrialLedger(ledger)
	in := trialSignupInput()
	in.EmailHash = tlHash

	_, created, err := svc.TrialSignup(context.Background(), in)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, []claim{{tlHash, in.TenantID}}, ledger.claims)
}

func TestTrialSignup_HashHeldByAnotherTenant_FailsI1(t *testing.T) {
	svc, _ := buildTrialProvisioningSvc(allSystemDepts())
	svc.WithTrialLedger(&tlLedger{err: domain.NewError(domain.ErrTrialAlreadyUsed, "used")})
	in := trialSignupInput()
	in.EmailHash = tlHash

	_, _, err := svc.TrialSignup(context.Background(), in)
	require.ErrorIs(t, err, domain.ErrTrialAlreadyUsed)
}

func TestTrialSignup_MalformedHash_Rejected(t *testing.T) {
	svc, _ := buildTrialProvisioningSvc(allSystemDepts())
	ledger := &tlLedger{}
	svc.WithTrialLedger(ledger)
	for _, h := range []string{"abc", strings.Repeat("a", 129), strings.Repeat("g", 64)} {
		in := trialSignupInput()
		in.EmailHash = h
		_, _, err := svc.TrialSignup(context.Background(), in)
		require.ErrorIs(t, err, domain.ErrValidation, h)
	}
	assert.Empty(t, ledger.claims)
}

func TestTrialSignup_NoHash_NoClaim(t *testing.T) {
	svc, _ := buildTrialProvisioningSvc(allSystemDepts())
	ledger := &tlLedger{}
	svc.WithTrialLedger(ledger)
	_, _, err := svc.TrialSignup(context.Background(), trialSignupInput())
	require.NoError(t, err)
	assert.Empty(t, ledger.claims)
}

func TestTrialSignup_LicensedSeats(t *testing.T) {
	for _, tc := range []struct {
		in, want int
	}{{0, service.DefaultTrialSeats}, {25, 25}} {
		svc, _ := buildTrialProvisioningSvc(allSystemDepts())
		in := trialSignupInput()
		in.LicensedSeats = tc.in
		tenant, _, err := svc.TrialSignup(context.Background(), in)
		require.NoError(t, err)
		assert.Equal(t, tc.want, tenant.LicensedSeats)
	}

	svc, _ := buildTrialProvisioningSvc(allSystemDepts())
	in := trialSignupInput()
	in.LicensedSeats = -1
	_, _, err := svc.TrialSignup(context.Background(), in)
	require.ErrorIs(t, err, domain.ErrValidation)
}
