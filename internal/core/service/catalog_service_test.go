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

// ── fakes local to this file ────────────────────────────────────────────

type fakeCatalogAdminClient struct {
	departmentsFn func(context.Context) ([]port.CatalogDepartment, error)
	plansFn       func(context.Context) ([]port.CatalogPlan, error)
	calls         int
}

func (f *fakeCatalogAdminClient) Departments(ctx context.Context) ([]port.CatalogDepartment, error) {
	f.calls++
	if f.departmentsFn != nil {
		return f.departmentsFn(ctx)
	}
	return nil, errors.New("not used")
}

func (f *fakeCatalogAdminClient) Plans(ctx context.Context) ([]port.CatalogPlan, error) {
	f.calls++
	if f.plansFn != nil {
		return f.plansFn(ctx)
	}
	return nil, errors.New("not used")
}

var _ port.CatalogAdminClient = (*fakeCatalogAdminClient)(nil)

// csCache is a minimal in-memory port.Cache for CatalogService tests.
type csCache struct {
	values map[string][]byte
}

func newCSCache() *csCache { return &csCache{values: map[string][]byte{}} }

func (c *csCache) Get(_ context.Context, key string) ([]byte, error) { return c.values[key], nil }
func (c *csCache) MGet(_ context.Context, keys []string) ([][]byte, error) {
	out := make([][]byte, len(keys))
	for i, k := range keys {
		out[i] = c.values[k]
	}
	return out, nil
}
func (c *csCache) Set(_ context.Context, key string, value []byte, _ time.Duration) error {
	c.values[key] = value
	return nil
}
func (c *csCache) SetNX(_ context.Context, key string, value []byte, _ time.Duration) (bool, error) {
	if _, ok := c.values[key]; ok {
		return false, nil
	}
	c.values[key] = value
	return true, nil
}
func (c *csCache) Delete(_ context.Context, keys ...string) error {
	for _, k := range keys {
		delete(c.values, k)
	}
	return nil
}
func (c *csCache) Health(_ context.Context) error { return nil }
func (c *csCache) Close() error                   { return nil }

var _ port.Cache = (*csCache)(nil)

// ── Departments ──────────────────────────────────────────────────────

func TestCatalogService_Departments_CacheMissPopulatesPrimaryAndStale(t *testing.T) {
	deptID := uuid.New()
	client := &fakeCatalogAdminClient{
		departmentsFn: func(context.Context) ([]port.CatalogDepartment, error) {
			return []port.CatalogDepartment{{ID: deptID, Code: "LEGAL", IsSystem: true, IsActive: true, RecordVersion: 1}}, nil
		},
	}
	cache := newCSCache()
	svc := NewCatalogService(client, cache)

	got, err := svc.Departments(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, deptID, got[0].ID)
	assert.NotEmpty(t, cache.values["om:departments"], "primary key must be populated")
	assert.NotEmpty(t, cache.values["om:departments:stale"], "stale key must be populated alongside the primary")
	assert.Equal(t, 1, client.calls)
}

func TestCatalogService_Departments_CacheHitSkipsClient(t *testing.T) {
	client := &fakeCatalogAdminClient{}
	cache := newCSCache()
	cache.values["om:departments"] = []byte(`[{"ID":"11111111-1111-1111-1111-111111111111","Code":"LEGAL"}]`)
	svc := NewCatalogService(client, cache)

	got, err := svc.Departments(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, 0, client.calls, "warm primary cache must short-circuit the client entirely")
}

func TestCatalogService_Departments_ClientFailsServesStale(t *testing.T) {
	client := &fakeCatalogAdminClient{
		departmentsFn: func(context.Context) ([]port.CatalogDepartment, error) {
			return nil, errors.New("catalog-admin-config unreachable")
		},
	}
	cache := newCSCache()
	cache.values["om:departments:stale"] = []byte(`[{"ID":"11111111-1111-1111-1111-111111111111","Code":"LEGAL"}]`)
	svc := NewCatalogService(client, cache)

	got, err := svc.Departments(context.Background())
	require.NoError(t, err, "a warm stale-if-error key must never surface as an error (CAT-D4)")
	require.Len(t, got, 1)
	assert.Equal(t, "LEGAL", got[0].Code)
}

func TestCatalogService_Departments_ClientFailsAndNoStale_ReturnsCatalogServiceUnavailable(t *testing.T) {
	client := &fakeCatalogAdminClient{
		departmentsFn: func(context.Context) ([]port.CatalogDepartment, error) {
			return nil, errors.New("catalog-admin-config unreachable")
		},
	}
	svc := NewCatalogService(client, newCSCache())

	_, err := svc.Departments(context.Background())
	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrCatalogServiceUnavailable.Error(), de.Code)
}

func TestCatalogService_DepartmentByID_FoundAndNotFound(t *testing.T) {
	deptID := uuid.New()
	client := &fakeCatalogAdminClient{
		departmentsFn: func(context.Context) ([]port.CatalogDepartment, error) {
			return []port.CatalogDepartment{{ID: deptID, Code: "LEGAL", IsActive: true}}, nil
		},
	}
	svc := NewCatalogService(client, newCSCache())

	got, err := svc.DepartmentByID(context.Background(), deptID)
	require.NoError(t, err)
	assert.Equal(t, "LEGAL", got.Code)

	_, err = svc.DepartmentByID(context.Background(), uuid.New())
	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrDepartmentNotFound.Error(), de.Code)
}

// ── Plans ────────────────────────────────────────────────────────────

func TestCatalogService_Plans_CacheMissPopulatesPrimaryAndStale(t *testing.T) {
	client := &fakeCatalogAdminClient{
		plansFn: func(context.Context) ([]port.CatalogPlan, error) {
			return []port.CatalogPlan{{Code: domain.PlanStarter, DisplayName: "Starter", RecordVersion: 1}}, nil
		},
	}
	cache := newCSCache()
	svc := NewCatalogService(client, cache)

	got, err := svc.Plans(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.NotEmpty(t, cache.values["om:plans"])
	assert.NotEmpty(t, cache.values["om:plans:stale"])
}

func TestCatalogService_PlanByCode_FoundAndNotFound(t *testing.T) {
	client := &fakeCatalogAdminClient{
		plansFn: func(context.Context) ([]port.CatalogPlan, error) {
			return []port.CatalogPlan{{Code: domain.PlanStarter, DisplayName: "Starter"}}, nil
		},
	}
	svc := NewCatalogService(client, newCSCache())

	got, err := svc.PlanByCode(context.Background(), domain.PlanStarter)
	require.NoError(t, err)
	assert.Equal(t, "Starter", got.DisplayName)

	_, err = svc.PlanByCode(context.Background(), domain.PlanEnterprise)
	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrPlanNotFound.Error(), de.Code)
}

func TestCatalogService_Plans_ClientFailsAndNoStale_ReturnsCatalogServiceUnavailable(t *testing.T) {
	client := &fakeCatalogAdminClient{
		plansFn: func(context.Context) ([]port.CatalogPlan, error) {
			return nil, errors.New("catalog-admin-config unreachable")
		},
	}
	svc := NewCatalogService(client, newCSCache())

	_, err := svc.Plans(context.Background())
	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrCatalogServiceUnavailable.Error(), de.Code)
}

// ── nil cache is a valid configuration (advisory-only) ────────────────

func TestCatalogService_NilCache_FallsThroughToClientEveryCall(t *testing.T) {
	client := &fakeCatalogAdminClient{
		departmentsFn: func(context.Context) ([]port.CatalogDepartment, error) {
			return []port.CatalogDepartment{{ID: uuid.New(), Code: "LEGAL"}}, nil
		},
	}
	svc := NewCatalogService(client, nil)

	_, err := svc.Departments(context.Background())
	require.NoError(t, err)
	_, err = svc.Departments(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, client.calls, "with no cache, every call must reach the client")
}
