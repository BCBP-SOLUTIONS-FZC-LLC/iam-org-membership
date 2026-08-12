// Extended invitation service tests: P6 reinvite cooldown, seat boundary.
package unit_test

import (
	"context"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── cooldownInviteRepo: fakeInviteRepo with configurable MostRecentCreatedAt ──

type cooldownInviteRepo struct {
	fakeInviteRepo
	mostRecentFn  func(ctx context.Context, tid uuid.UUID, email string) (time.Time, error)
	findPendingFn func(ctx context.Context, tid uuid.UUID, email string) (*domain.PendingInvitation, error)
	countWindowFn func(ctx context.Context, tid uuid.UUID, since time.Time) (int, error)
}

func (r *cooldownInviteRepo) MostRecentCreatedAt(ctx context.Context, tid uuid.UUID, email string) (time.Time, error) {
	if r.mostRecentFn != nil {
		return r.mostRecentFn(ctx, tid, email)
	}
	return time.Time{}, nil
}
func (r *cooldownInviteRepo) FindPendingByEmail(ctx context.Context, tid uuid.UUID, email string) (*domain.PendingInvitation, error) {
	if r.findPendingFn != nil {
		return r.findPendingFn(ctx, tid, email)
	}
	return nil, nil
}
func (r *cooldownInviteRepo) CountCreatedInWindow(ctx context.Context, tid uuid.UUID, since time.Time) (int, error) {
	if r.countWindowFn != nil {
		return r.countWindowFn(ctx, tid, since)
	}
	return 0, nil
}

var _ port.InvitationRepository = (*cooldownInviteRepo)(nil)

// ── P6-REINVITE-01: reinvite within cooldown → 429 ────────────────────

// Test Case ID:      P6-REINVITE-01
// Feature:           P-6 · re-invite within cooldown window → 429 reinvite_too_soon (PI-11)
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestInvite_ReinviteCooldown_Returns429(t *testing.T) {
	cooldown := time.Hour
	// Most recent invite was 30 minutes ago → still within 1-hour cooldown
	recentTime := time.Now().UTC().Add(-30 * time.Minute)
	inv := &cooldownInviteRepo{
		findPendingFn: func(_ context.Context, _ uuid.UUID, _ string) (*domain.PendingInvitation, error) {
			return nil, nil // no existing pending
		},
		mostRecentFn: func(_ context.Context, _ uuid.UUID, _ string) (time.Time, error) {
			return recentTime, nil
		},
	}
	svc := service.NewInvitationService(inv, nil, nil, nil, nil, nil, nil, nil, nil, 7).
		WithReinviteCooldown(cooldown)
	_, err := svc.Invite(context.Background(), uuid.New(),
		service.InvitationInput{Email: "user@example.com", FullName: "Test User"},
		uuid.New())

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.ErrorIs(t, err, domain.ErrReinviteTooSoon)
	retryAfter, ok := de.Details["retry_after_seconds"]
	assert.True(t, ok, "retry_after_seconds must be in response")
	assert.NotZero(t, retryAfter)
}

// P6-SEAT-BOUNDARY-01: the SEAT-1 transactional cap check fires inside RunInTx after
// the RP.CreateInvitedUser call. Testing this as a unit test requires mocking RP +
// preflightSeatCheck (CountActive, CountPending) + the inner-tx QueryRow calls.
// Full seat-cap coverage is in test/postgres/phase16_perf_test.go (TestP16_SLO_SeatPreflight100Concurrent).
// The ErrSeatLimitReached error code is proven correct by existing domain + middleware tests.

// P6-HAPPY-01: full 202 path requires postgres integration test (real TxRunner+DB for SEAT-1).
// Noted as postgres-only coverage.
