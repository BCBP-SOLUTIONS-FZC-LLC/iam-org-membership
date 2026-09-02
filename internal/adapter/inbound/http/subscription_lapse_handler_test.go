// Handler-layer tests for InternalHandler.ListSubscriptionLapses
// (I-16, §16 RP-C3).
package http

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

type slhTenantRepo struct {
	port.TenantRepositoryNoop
	listFn func(context.Context, int) ([]domain.Tenant, error)
}

func (f *slhTenantRepo) FindByID(context.Context, uuid.UUID) (*domain.Tenant, error) {
	return nil, errors.New("not used")
}
func (f *slhTenantRepo) FindByIDIncludingDeleted(context.Context, uuid.UUID) (*domain.Tenant, error) {
	return nil, errors.New("not used")
}
func (f *slhTenantRepo) Update(context.Context, uuid.UUID, *domain.TenantPatch) (*domain.Tenant, error) {
	return nil, errors.New("not used")
}
func (f *slhTenantRepo) SetRealmSyncPending(context.Context, uuid.UUID) error {
	return errors.New("not used")
}
func (f *slhTenantRepo) Insert(context.Context, *domain.Tenant) (*domain.Tenant, bool, error) {
	return nil, false, errors.New("not used")
}
func (f *slhTenantRepo) ListSubscriptionLapses(ctx context.Context, graceDays int) ([]domain.Tenant, error) {
	return f.listFn(ctx, graceDays)
}

func TestListSubscriptionLapses_Success_200(t *testing.T) {
	tenantID := uuid.New()
	cancelledAt := time.Now().Add(-45 * 24 * time.Hour)
	repo := &slhTenantRepo{listFn: func(_ context.Context, graceDays int) ([]domain.Tenant, error) {
		assert.Equal(t, 30, graceDays)
		return []domain.Tenant{
			{ID: tenantID, RealmID: "acme", RealmType: domain.RealmDedicated, Status: domain.StatusCancelled, CancelledAt: &cancelledAt},
		}, nil
	}}
	svc := service.NewSubscriptionLapseService(repo, 30)
	h := &InternalHandler{subscriptionLapse: svc}

	c, w := buildCtx(http.MethodGet, "/api/v1/internal/subscription-lapses", ``, systemCtx())
	h.ListSubscriptionLapses(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), tenantID.String())
	assert.Contains(t, w.Body.String(), `"realm_id":"acme"`)
}

func TestListSubscriptionLapses_RepoError_Propagates(t *testing.T) {
	repo := &slhTenantRepo{listFn: func(context.Context, int) ([]domain.Tenant, error) {
		return nil, domain.NewError(domain.ErrDBUnavailable, "db down")
	}}
	svc := service.NewSubscriptionLapseService(repo, 30)
	h := &InternalHandler{subscriptionLapse: svc}

	c, w := buildCtx(http.MethodGet, "/api/v1/internal/subscription-lapses", ``, systemCtx())
	h.ListSubscriptionLapses(c)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestListSubscriptionLapses_EmptySet_200(t *testing.T) {
	repo := &slhTenantRepo{listFn: func(context.Context, int) ([]domain.Tenant, error) {
		return nil, nil
	}}
	svc := service.NewSubscriptionLapseService(repo, 30)
	h := &InternalHandler{subscriptionLapse: svc}

	c, w := buildCtx(http.MethodGet, "/api/v1/internal/subscription-lapses", ``, systemCtx())
	h.ListSubscriptionLapses(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"tenants":[]`)
}
