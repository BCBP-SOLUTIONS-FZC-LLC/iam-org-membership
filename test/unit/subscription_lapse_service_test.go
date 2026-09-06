// Unit tests for SubscriptionLapseService (I-16, §16 OQ-9/RP-C3).
//
// Test case IDs: I16-CFG-01, I16-CFG-02, I16-CFG-03, I16-ERR-01, I16-IDEMP-01
//
// These tests verify service-layer behaviour in isolation: that graceDays is
// propagated correctly to the repo and that errors are passed through
// unchanged. DB-level query correctness is exercised in test/postgres/.
package unit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── fake repo ────────────────────────────────────────────────────────────────

type slsRepo struct {
	port.TenantRepositoryNoop
	listFn func(ctx context.Context, graceDays int) ([]domain.Tenant, error)
}

func (r *slsRepo) ListSubscriptionLapses(ctx context.Context, graceDays int) ([]domain.Tenant, error) {
	if r.listFn != nil {
		return r.listFn(ctx, graceDays)
	}
	return nil, nil
}

var _ port.TenantRepository = (*slsRepo)(nil)

// ── I16-CFG-01: graceDays=0 → repo receives 0 ────────────────────────────────

// Test Case ID: I16-CFG-01
func TestSubscriptionLapseService_GraceDays0_PassedToRepo(t *testing.T) {
	var got int
	svc := service.NewSubscriptionLapseService(&slsRepo{
		listFn: func(_ context.Context, graceDays int) ([]domain.Tenant, error) {
			got = graceDays
			return nil, nil
		},
	}, 0)

	_, err := svc.List(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, got, "graceDays=0 must reach the repo — all cancelled qualify immediately")
}

// ── I16-CFG-02: graceDays=60 → repo receives 60 ──────────────────────────────

// Test Case ID: I16-CFG-02
func TestSubscriptionLapseService_GraceDays60_PassedToRepo(t *testing.T) {
	var got int
	svc := service.NewSubscriptionLapseService(&slsRepo{
		listFn: func(_ context.Context, graceDays int) ([]domain.Tenant, error) {
			got = graceDays
			return nil, nil
		},
	}, 60)

	_, err := svc.List(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 60, got, "graceDays=60 must reach the repo — extended grace window")
}

// ── I16-CFG-03: graceDays=30 (SUBSCRIPTION_GRACE_DAYS default) ───────────────

// Test Case ID: I16-CFG-03
func TestSubscriptionLapseService_GraceDays30_Default(t *testing.T) {
	var got int
	svc := service.NewSubscriptionLapseService(&slsRepo{
		listFn: func(_ context.Context, graceDays int) ([]domain.Tenant, error) {
			got = graceDays
			return nil, nil
		},
	}, 30)

	_, err := svc.List(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 30, got)
}

// ── I16-ERR-01: repo error is propagated unchanged ───────────────────────────

// Test Case ID: I16-ERR-01
func TestSubscriptionLapseService_RepoError_Propagated(t *testing.T) {
	sentinel := errors.New("pool exhausted")
	svc := service.NewSubscriptionLapseService(&slsRepo{
		listFn: func(context.Context, int) ([]domain.Tenant, error) {
			return nil, sentinel
		},
	}, 30)

	_, err := svc.List(context.Background())
	assert.ErrorIs(t, err, sentinel, "repo error must propagate — service must not swallow it")
}

// ── I16-IDEMP-01: repeated List() calls → same repo invoked, no caching ───────

// Test Case ID: I16-IDEMP-01
func TestSubscriptionLapseService_RepeatedCalls_NoStatefulBehavior(t *testing.T) {
	calls := 0
	tenantID := uuid.New()
	ct := domain.Tenant{ID: tenantID, Status: domain.StatusCancelled}
	svc := service.NewSubscriptionLapseService(&slsRepo{
		listFn: func(context.Context, int) ([]domain.Tenant, error) {
			calls++
			return []domain.Tenant{ct}, nil
		},
	}, 30)

	r1, err1 := svc.List(context.Background())
	require.NoError(t, err1)
	r2, err2 := svc.List(context.Background())
	require.NoError(t, err2)

	assert.Equal(t, 2, calls, "repo must be called on every List() — no in-memory caching")
	require.Len(t, r1, 1)
	require.Len(t, r2, 1)
	assert.Equal(t, r1[0].ID, r2[0].ID, "both calls must return same result from repo")
}
