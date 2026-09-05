package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// trNoopCatalog is a PlanCatalogReader that always succeeds.
type trNoopCatalog struct{}

func (c *trNoopCatalog) Plans(context.Context) ([]domain.Plan, error) { return nil, nil }
func (c *trNoopCatalog) PlanByCode(_ context.Context, code domain.TenantPlan) (*domain.Plan, error) {
	return &domain.Plan{Code: code, TrialDurationDays: 14}, nil
}

var _ port.PlanCatalogReader = (*trNoopCatalog)(nil)

// trErrCatalog is a PlanCatalogReader that returns an error on PlanByCode.
type trErrCatalog struct{ err error }

func (c *trErrCatalog) Plans(context.Context) ([]domain.Plan, error) { return nil, nil }
func (c *trErrCatalog) PlanByCode(context.Context, domain.TenantPlan) (*domain.Plan, error) {
	return nil, c.err
}

var _ port.PlanCatalogReader = (*trErrCatalog)(nil)

func mkTrialReactivatedEnv(tenantID string) events.Envelope[json.RawMessage] {
	return events.Envelope[json.RawMessage]{
		ID:        uuid.New().String(),
		Type:      "TrialReactivated",
		TenantID:  tenantID,
		Timestamp: time.Now().UTC().Add(-1 * time.Second),
	}
}

// TestHandle_TrialReactivated_FindByIDNonNotFoundError_Propagates covers the
// branch at lines 153-154: when the pre-tx tenant peek returns a non-not-found
// error, Handle wraps and propagates it.
func TestHandle_TrialReactivated_FindByIDNonNotFoundError_Propagates(t *testing.T) {
	dbErr := errors.New("db_unavailable")
	tenants := &htTenantRepo{
		findByIDFn: func(_ context.Context, _ uuid.UUID) (*domain.Tenant, error) {
			return nil, dbErr
		},
	}
	idemp := &htIdempotency{}
	c := NewMembershipEventConsumer(&htSyncTxRunner{}, tenants, idemp, &trNoopCatalog{}, nil, 5*time.Minute, nil)

	env := mkTrialReactivatedEnv(uuid.New().String())
	err := c.Handle(context.Background(), env)

	require.Error(t, err)
	assert.ErrorIs(t, err, dbErr)
}

// TestHandle_TrialReactivated_NilCatalog_ReturnsError covers the branch at
// line 157-158: tenant found but catalog == nil → returns error.
func TestHandle_TrialReactivated_NilCatalog_ReturnsError(t *testing.T) {
	tenantID := uuid.New()
	tenants := &htTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, Plan: domain.PlanStarter}, nil
		},
	}
	idemp := &htIdempotency{}
	// catalog == nil: triggers "no PlanCatalogReader configured" error
	c := NewMembershipEventConsumer(&htSyncTxRunner{}, tenants, idemp, nil, nil, 5*time.Minute, nil)

	env := mkTrialReactivatedEnv(tenantID.String())
	err := c.Handle(context.Background(), env)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no PlanCatalogReader configured")
}

// TestHandle_TrialReactivated_CatalogPlanByCodeError_Propagates covers the
// branch at lines 161-163: catalog.PlanByCode returns an error → propagated.
func TestHandle_TrialReactivated_CatalogPlanByCodeError_Propagates(t *testing.T) {
	tenantID := uuid.New()
	catalogErr := errors.New("catalog_unavailable")
	tenants := &htTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, Plan: domain.PlanStarter}, nil
		},
	}
	idemp := &htIdempotency{}
	c := NewMembershipEventConsumer(&htSyncTxRunner{}, tenants, idemp, &trErrCatalog{err: catalogErr}, nil, 5*time.Minute, nil)

	env := mkTrialReactivatedEnv(tenantID.String())
	err := c.Handle(context.Background(), env)

	require.Error(t, err)
	assert.ErrorIs(t, err, catalogErr)
}
