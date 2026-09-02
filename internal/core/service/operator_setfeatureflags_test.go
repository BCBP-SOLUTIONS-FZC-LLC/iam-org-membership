package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── stubs (whitebox, package-private) ──────────────────────────────────

type callThruTxRunner struct {
	runErr error
}

func (r callThruTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if r.runErr != nil {
		return r.runErr
	}
	return fn(ctx)
}

type ffTenantRepo struct {
	port.TenantRepositoryNoop
	findByIDFn        func(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)
	setFeatureFlagsFn func(ctx context.Context, id uuid.UUID, flags []byte, expectedVersion int64) error
}

func (r *ffTenantRepo) FindByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	if r.findByIDFn != nil {
		return r.findByIDFn(ctx, id)
	}
	return nil, errors.New("FindByID not stubbed")
}
func (r *ffTenantRepo) SetFeatureFlags(ctx context.Context, id uuid.UUID, flags []byte, expectedVersion int64) error {
	if r.setFeatureFlagsFn != nil {
		return r.setFeatureFlagsFn(ctx, id, flags, expectedVersion)
	}
	return nil
}

type ffCache struct {
	deleteCalls []string
}

func (c *ffCache) Get(context.Context, string) ([]byte, error)      { return nil, nil }
func (c *ffCache) MGet(context.Context, []string) ([][]byte, error) { return nil, nil }
func (c *ffCache) Set(context.Context, string, []byte, time.Duration) error {
	return nil
}
func (c *ffCache) SetNX(context.Context, string, []byte, time.Duration) (bool, error) {
	return false, nil
}
func (c *ffCache) Delete(_ context.Context, keys ...string) error {
	c.deleteCalls = append(c.deleteCalls, keys...)
	return nil
}
func (c *ffCache) Health(context.Context) error { return nil }
func (c *ffCache) Close() error                 { return nil }

var _ port.Cache = (*ffCache)(nil)
var _ port.TenantRepository = (*ffTenantRepo)(nil)

func buildOperatorWithPool(tenants port.TenantRepository, tr port.TxRunner, cache port.Cache) *OperatorService {
	return &OperatorService{
		tenants: tenants, txRunner: tr, cache: cache,
	}
}

// ── SetFeatureFlags — validation branch ─────────────────────────────────

func TestOperator_SetFeatureFlags_NestedObjectRejected(t *testing.T) {
	svc := buildOperatorWithPool(nil, nil, nil)
	_, err := svc.SetFeatureFlags(context.Background(), uuid.New(),
		map[string]any{"custom_branding": map[string]string{"k": "v"}}, 1)

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
	assert.Equal(t, "invalid_feature_value", de.Details["code"])
	assert.Equal(t, "custom_branding", de.Details["key"])
}

func TestOperator_SetFeatureFlags_ArrayRejected(t *testing.T) {
	svc := buildOperatorWithPool(nil, nil, nil)
	_, err := svc.SetFeatureFlags(context.Background(), uuid.New(),
		map[string]any{"sso_enabled": []int{1, 2, 3}}, 1)

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "invalid_feature_value", de.Details["code"])
}

func TestOperator_SetFeatureFlags_UnknownKeyRejected(t *testing.T) {
	svc := buildOperatorWithPool(nil, nil, nil)
	_, err := svc.SetFeatureFlags(context.Background(), uuid.New(),
		map[string]any{"sso_enable": true}, 1)

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
	assert.Equal(t, "unknown_feature_flag", de.Details["code"])
	assert.Equal(t, "sso_enable", de.Details["key"])
}

func TestOperator_SetFeatureFlags_AllScalarTypesAccepted(t *testing.T) {
	tenantID := uuid.New()
	tenants := &ffTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id}, nil
		},
	}
	svc := buildOperatorWithPool(tenants, callThruTxRunner{}, nil)

	_, err := svc.SetFeatureFlags(context.Background(), tenantID, map[string]any{
		"sso_enabled":           true,
		"custom_branding":       "logo",
		"require_mfa_all_users": true,
	}, 1)
	require.NoError(t, err)
}

func TestOperator_SetFeatureFlags_HappyPathUpdatesTenant(t *testing.T) {
	tenantID := uuid.New()
	var gotID uuid.UUID
	var gotFlags []byte
	var gotVer int64
	tenants := &ffTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, RecordVersion: 2}, nil
		},
		setFeatureFlagsFn: func(_ context.Context, id uuid.UUID, flags []byte, ver int64) error {
			gotID, gotFlags, gotVer = id, flags, ver
			return nil
		},
	}
	cache := &ffCache{}
	svc := buildOperatorWithPool(tenants, callThruTxRunner{}, cache)

	got, err := svc.SetFeatureFlags(context.Background(), tenantID, map[string]any{"sso_enabled": true}, 2)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, tenantID, gotID)
	assert.JSONEq(t, `{"sso_enabled":true}`, string(gotFlags))
	assert.EqualValues(t, 2, gotVer)
	assert.EqualValues(t, 2, got.RecordVersion)
	assert.Contains(t, cache.deleteCalls, "om:tenant:"+tenantID.String())
}

func TestOperator_SetFeatureFlags_NilMapDefaultsToEmpty(t *testing.T) {
	tenants := &ffTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id}, nil
		},
	}
	svc := buildOperatorWithPool(tenants, callThruTxRunner{}, nil)
	_, err := svc.SetFeatureFlags(context.Background(), uuid.New(), nil, 1)
	require.NoError(t, err, "nil flags map must be treated as empty, not rejected")
}

func TestOperator_SetFeatureFlags_ExecErrorPropagates(t *testing.T) {
	execErr := errors.New("db down")
	tenants := &ffTenantRepo{
		setFeatureFlagsFn: func(context.Context, uuid.UUID, []byte, int64) error { return execErr },
	}
	svc := buildOperatorWithPool(tenants, callThruTxRunner{}, nil)
	_, err := svc.SetFeatureFlags(context.Background(), uuid.New(), map[string]any{}, 1)
	assert.ErrorIs(t, err, execErr)
}

func TestOperator_SetFeatureFlags_TenantReadErrorAfterUpdateSurfaces(t *testing.T) {
	tenants := &ffTenantRepo{
		findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
			return nil, errors.New("tenant gone")
		},
	}
	svc := buildOperatorWithPool(tenants, callThruTxRunner{}, nil)
	_, err := svc.SetFeatureFlags(context.Background(), uuid.New(), map[string]any{"sso_enabled": true}, 1)
	assert.ErrorContains(t, err, "tenant gone")
}
