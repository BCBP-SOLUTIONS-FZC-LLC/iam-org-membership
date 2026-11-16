package unit_test

// PI-11 vs SECURITY-8: a revoke must keep the invite email while the
// re-invite cooldown can still match it, otherwise invite→revoke→re-invite
// churn is unbounded. The invitation-expiry reconciler erases it later
// (EraseRevokedPII). With the cooldown disabled, revoke erases at once
// (not reachable from configuration — §12 ignores ≤ 0; kept as a defensive default).

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
)

type erasePIICountingRepo struct {
	*fakeInviteRepo
	erased int
}

func (r *erasePIICountingRepo) ErasePII(context.Context, uuid.UUID, uuid.UUID) error {
	r.erased++
	return nil
}

func revokeRepo() *erasePIICountingRepo {
	return &erasePIICountingRepo{fakeInviteRepo: &fakeInviteRepo{
		setStatusFn: func(_ context.Context, _, id uuid.UUID, _ domain.InvitationStatus, ver int64) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: id, RecordVersion: ver + 1}, nil
		},
		setKCCleanupPendingFn: func(context.Context, uuid.UUID, uuid.UUID, bool, int64) error { return nil },
	}}
}

func TestInvitation_Revoke_CooldownEnabled_KeepsEmailForPI11(t *testing.T) {
	repo := revokeRepo()
	svc := buildInvitationSvc(repo, nil).WithReinviteCooldown(15 * time.Minute)

	_, err := svc.Revoke(context.Background(), uuid.New(), uuid.New(), 1)
	require.NoError(t, err)
	assert.Zero(t, repo.erased, "PI-11: revoke must not erase the email while the cooldown can still match it")
}

func TestInvitation_Revoke_CooldownDisabled_ErasesImmediately(t *testing.T) {
	repo := revokeRepo()
	svc := buildInvitationSvc(repo, nil).WithReinviteCooldown(0)

	_, err := svc.Revoke(context.Background(), uuid.New(), uuid.New(), 1)
	require.NoError(t, err)
	assert.Equal(t, 1, repo.erased, "SECURITY-8: with no cooldown there is no reason to keep PII")
}
