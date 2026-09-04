package service

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// csErrCache wraps csCache, letting a test force Get to error for a given
// key (getErrKeys) independent of what's actually stored there — exercises
// getCachedDepartments/getCachedPlans's cache.Get-error fall-through branch,
// which csCache's plain map-backed Get can never produce on its own.
type csErrCache struct {
	*csCache
	getErrKeys map[string]error
}

func newCSErrCache() *csErrCache {
	return &csErrCache{csCache: newCSCache(), getErrKeys: map[string]error{}}
}

func (c *csErrCache) Get(ctx context.Context, key string) ([]byte, error) {
	if err, ok := c.getErrKeys[key]; ok {
		return nil, err
	}
	return c.csCache.Get(ctx, key)
}

var _ port.Cache = (*csErrCache)(nil)

// ── getCachedDepartments / getCachedPlans: cache.Get error falls through ─

func TestCatalogService_Departments_CacheGetErrorFallsThroughToClient(t *testing.T) {
	client := &fakeCatalogAdminClient{
		departmentsFn: func(context.Context) ([]port.CatalogDepartment, error) {
			return []port.CatalogDepartment{{ID: uuid.New(), Code: "LEGAL"}}, nil
		},
	}
	cache := newCSErrCache()
	cache.getErrKeys["om:departments"] = errors.New("cache unavailable")
	svc := NewCatalogService(client, cache)

	got, err := svc.Departments(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, 1, client.calls, "a Get error must fall through to the live client, not fail the call")
}

func TestCatalogService_Plans_CacheGetErrorFallsThroughToClient(t *testing.T) {
	client := &fakeCatalogAdminClient{
		plansFn: func(context.Context) ([]port.CatalogPlan, error) {
			return []port.CatalogPlan{{Code: domain.PlanStarter, DisplayName: "Starter"}}, nil
		},
	}
	cache := newCSErrCache()
	cache.getErrKeys["om:plans"] = errors.New("cache unavailable")
	svc := NewCatalogService(client, cache)

	got, err := svc.Plans(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, 1, client.calls)
}

// ── getCachedDepartments / getCachedPlans: corrupt JSON falls through ───

func TestCatalogService_Departments_CorruptCachedJSONFallsThroughToClient(t *testing.T) {
	client := &fakeCatalogAdminClient{
		departmentsFn: func(context.Context) ([]port.CatalogDepartment, error) {
			return []port.CatalogDepartment{{ID: uuid.New(), Code: "LEGAL"}}, nil
		},
	}
	cache := newCSCache()
	cache.values["om:departments"] = []byte("not-json")
	svc := NewCatalogService(client, cache)

	got, err := svc.Departments(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, 1, client.calls)
}

func TestCatalogService_Plans_CorruptCachedJSONFallsThroughToClient(t *testing.T) {
	client := &fakeCatalogAdminClient{
		plansFn: func(context.Context) ([]port.CatalogPlan, error) {
			return []port.CatalogPlan{{Code: domain.PlanStarter, DisplayName: "Starter"}}, nil
		},
	}
	cache := newCSCache()
	cache.values["om:plans"] = []byte("not-json")
	svc := NewCatalogService(client, cache)

	got, err := svc.Plans(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, 1, client.calls)
}

// ── Plans: client fails, stale-if-error key serves (CAT-D4) ─────────────

func TestCatalogService_Plans_ClientFailsServesStale(t *testing.T) {
	client := &fakeCatalogAdminClient{
		plansFn: func(context.Context) ([]port.CatalogPlan, error) {
			return nil, errors.New("catalog-admin-config unreachable")
		},
	}
	cache := newCSCache()
	cache.values["om:plans:stale"] = []byte(`[{"Code":"starter","DisplayName":"Starter"}]`)
	svc := NewCatalogService(client, cache)

	got, err := svc.Plans(context.Background())
	require.NoError(t, err, "a warm stale-if-error key must never surface as an error (CAT-D4)")
	require.Len(t, got, 1)
	assert.Equal(t, "Starter", got[0].DisplayName)
}

// ── DepartmentByID / PlanByCode: propagate the underlying catalog error ─

func TestCatalogService_DepartmentByID_PropagatesCatalogUnavailable(t *testing.T) {
	client := &fakeCatalogAdminClient{
		departmentsFn: func(context.Context) ([]port.CatalogDepartment, error) {
			return nil, errors.New("catalog-admin-config unreachable")
		},
	}
	svc := NewCatalogService(client, newCSCache())

	_, err := svc.DepartmentByID(context.Background(), uuid.New())
	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrCatalogUnavailable.Error(), de.Code)
}

// ── Plans: nil cache is a valid configuration (advisory-only) ──────────

func TestCatalogService_Plans_NilCache_FallsThroughToClientEveryCall(t *testing.T) {
	client := &fakeCatalogAdminClient{
		plansFn: func(context.Context) ([]port.CatalogPlan, error) {
			return []port.CatalogPlan{{Code: domain.PlanStarter, DisplayName: "Starter"}}, nil
		},
	}
	svc := NewCatalogService(client, nil)

	_, err := svc.Plans(context.Background())
	require.NoError(t, err)
	_, err = svc.Plans(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, client.calls, "with no cache, every call must reach the client")
}

func TestCatalogService_PlanByCode_PropagatesCatalogUnavailable(t *testing.T) {
	client := &fakeCatalogAdminClient{
		plansFn: func(context.Context) ([]port.CatalogPlan, error) {
			return nil, errors.New("catalog-admin-config unreachable")
		},
	}
	svc := NewCatalogService(client, newCSCache())

	_, err := svc.PlanByCode(context.Background(), domain.PlanStarter)
	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrCatalogUnavailable.Error(), de.Code)
}
