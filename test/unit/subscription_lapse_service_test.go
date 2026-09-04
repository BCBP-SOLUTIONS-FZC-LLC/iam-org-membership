// Unit tests for internal/core/service/subscription_lapse_service.go (I-16,
// §16 RP-C3). Previously exercised only via postgres integration tests —
// its single collaborator (port.TenantRepository.ListSubscriptionLapses) is
// a plain interface, fakeable the same way every other service in this
// package is.
package unit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slTenantRepo is a configurable TenantRepository for
// SubscriptionLapseService.List — only ListSubscriptionLapses matters.
type slTenantRepo struct {
	invTenantRepo
	listLapsesFn     func(ctx context.Context, graceDays int) ([]domain.Tenant, error)
	gotGraceDays     int
	listLapsesCalled bool
}

func (r *slTenantRepo) ListSubscriptionLapses(ctx context.Context, graceDays int) ([]domain.Tenant, error) {
	r.listLapsesCalled = true
	r.gotGraceDays = graceDays
	if r.listLapsesFn != nil {
		return r.listLapsesFn(ctx, graceDays)
	}
	return nil, nil
}

func TestNewSubscriptionLapseService_ReturnsNonNil(t *testing.T) {
	svc := service.NewSubscriptionLapseService(&slTenantRepo{}, 30)
	require.NotNil(t, svc)
}

func TestSubscriptionLapseService_List_DelegatesGraceDaysAndPropagatesResult(t *testing.T) {
	want := []domain.Tenant{{ID: uuid.New()}, {ID: uuid.New()}}
	repo := &slTenantRepo{listLapsesFn: func(context.Context, int) ([]domain.Tenant, error) {
		return want, nil
	}}
	svc := service.NewSubscriptionLapseService(repo, 30)

	got, err := svc.List(context.Background())
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.True(t, repo.listLapsesCalled)
	assert.Equal(t, 30, repo.gotGraceDays)
}

func TestSubscriptionLapseService_List_PropagatesRepoError(t *testing.T) {
	repoErr := errors.New("db down")
	repo := &slTenantRepo{listLapsesFn: func(context.Context, int) ([]domain.Tenant, error) {
		return nil, repoErr
	}}
	svc := service.NewSubscriptionLapseService(repo, 45)

	_, err := svc.List(context.Background())
	assert.ErrorIs(t, err, repoErr)
}
