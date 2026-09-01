// Unit tests for internal/core/service/catalog_service.go.
//
// Covers the two-tier (primary + stale-if-error) cache behaviour of
// CatalogService.Departments and CatalogService.Plans, the nil-cache fast
// path, the WithLogger injection, and the DepartmentByID / PlanByCode
// point-lookup helpers.
//
// Hand-rolled stubs only — no testcontainers. All tests run without Docker.
package unit_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── catalogTestCache ──────────────────────────────────────────────────────
// map-backed in-process cache fake that satisfies port.Cache.

type catalogTestCache struct {
	mu     sync.Mutex
	data   map[string][]byte
	getErr error // when non-nil, Get always returns this error
	setErr error // when non-nil, Set always returns this error
}

func newCatalogTestCache() *catalogTestCache {
	return &catalogTestCache{data: make(map[string][]byte)}
}

func (c *catalogTestCache) Get(_ context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.getErr != nil {
		return nil, c.getErr
	}
	v, ok := c.data[key]
	if !ok {
		return nil, nil // cache miss — port.Cache contract
	}
	return v, nil
}

func (c *catalogTestCache) MGet(_ context.Context, _ []string) ([][]byte, error) {
	return nil, nil
}

func (c *catalogTestCache) Set(_ context.Context, key string, v []byte, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.setErr != nil {
		return c.setErr
	}
	c.data[key] = v
	return nil
}

func (c *catalogTestCache) SetNX(_ context.Context, _ string, _ []byte, _ time.Duration) (bool, error) {
	return true, nil
}

func (c *catalogTestCache) Delete(_ context.Context, _ ...string) error { return nil }
func (c *catalogTestCache) Health(_ context.Context) error              { return nil }
func (c *catalogTestCache) Close() error                                { return nil }

// seed stores a pre-marshalled value under key so tests can exercise the
// cache-hit path without going through CatalogService.
func (c *catalogTestCache) seed(key string, v []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[key] = v
}

// has returns true if key exists in the backing map.
func (c *catalogTestCache) has(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.data[key]
	return ok
}

var _ port.Cache = (*catalogTestCache)(nil)

// ── fakeCatalogAdminClient ────────────────────────────────────────────────

type fakeCatalogAdminClient struct {
	departmentsFn func(ctx context.Context) ([]port.CatalogDepartment, error)
	plansFn       func(ctx context.Context) ([]port.CatalogPlan, error)
}

func (f *fakeCatalogAdminClient) Departments(ctx context.Context) ([]port.CatalogDepartment, error) {
	if f.departmentsFn != nil {
		return f.departmentsFn(ctx)
	}
	return nil, nil
}

func (f *fakeCatalogAdminClient) Plans(ctx context.Context) ([]port.CatalogPlan, error) {
	if f.plansFn != nil {
		return f.plansFn(ctx)
	}
	return nil, nil
}

var _ port.CatalogAdminClient = (*fakeCatalogAdminClient)(nil)

// ── stubLogger ────────────────────────────────────────────────────────────
// minimal port.Logger implementation for WithLogger injection tests.

type stubLogger struct {
	warnCalls int
}

func (s *stubLogger) Debug(_ string, _ map[string]any) {}
func (s *stubLogger) Info(_ string, _ map[string]any)  {}
func (s *stubLogger) Warn(_ string, _ map[string]any)  { s.warnCalls++ }
func (s *stubLogger) Error(_ string, _ map[string]any) {}

var _ port.Logger = (*stubLogger)(nil)

// ── helpers ───────────────────────────────────────────────────────────────

// sampleDepts returns a deterministic slice of department DTOs for client fakes.
func sampleDepts() []port.CatalogDepartment {
	return []port.CatalogDepartment{
		{ID: uuid.MustParse("d1000000-0000-0000-0000-000000000001"), Code: "engineering", Name: "Engineering", IsSystem: true, IsActive: true, RecordVersion: 1},
		{ID: uuid.MustParse("d2000000-0000-0000-0000-000000000002"), Code: "design", Name: "Design", IsSystem: true, IsActive: true, RecordVersion: 1},
	}
}

// samplePlans returns a deterministic slice of plan DTOs.
func samplePlans() []port.CatalogPlan {
	wfl := 10
	tl := 50
	return []port.CatalogPlan{
		{Code: domain.PlanStarter, DisplayName: "Starter", WorkflowTemplateLimit: &wfl, TenderLimit: &tl,
			TrialDurationDays: 30, SSOEnabled: false, CustomBranding: "none",
			FeatureSet: map[string]any{}, RecordVersion: 1},
		{Code: domain.PlanPro, DisplayName: "Pro", TrialDurationDays: 30, SSOEnabled: true,
			CustomBranding: "logo", FeatureSet: map[string]any{"sso": true}, RecordVersion: 1},
	}
}

// marshalDomainDepts marshals a slice of domain.Department to JSON bytes, as
// CatalogService stores it in the cache.
func marshalDomainDepts(t *testing.T, depts []domain.Department) []byte {
	t.Helper()
	b, err := json.Marshal(depts)
	require.NoError(t, err)
	return b
}

// marshalDomainPlans marshals a slice of domain.Plan to JSON bytes.
func marshalDomainPlans(t *testing.T, plans []domain.Plan) []byte {
	t.Helper()
	b, err := json.Marshal(plans)
	require.NoError(t, err)
	return b
}

// dtoDeptsToDomainDepts converts port DTOs to domain.Department the same way
// CatalogService does, so tests can compare expected vs actual.
func dtoDeptsToDomainDepts(dtos []port.CatalogDepartment) []domain.Department {
	out := make([]domain.Department, len(dtos))
	for i, d := range dtos {
		out[i] = domain.Department{
			ID: d.ID, Code: d.Code, Name: d.Name,
			IsSystem: d.IsSystem, IsActive: d.IsActive, RecordVersion: d.RecordVersion,
		}
	}
	return out
}

// dtoPlansToDomainPlans mirrors CatalogService's mapping from port.CatalogPlan
// to domain.Plan so tests can build the expected value.
func dtoPlansToDomainPlans(dtos []port.CatalogPlan) []domain.Plan {
	out := make([]domain.Plan, len(dtos))
	for i, p := range dtos {
		out[i] = domain.Plan{
			Code: p.Code, DisplayName: p.DisplayName,
			WorkflowTemplateLimit: p.WorkflowTemplateLimit, TenderLimit: p.TenderLimit,
			TrialDurationDays: p.TrialDurationDays, SSOEnabled: p.SSOEnabled,
			CustomBranding: domain.BrandingLevel(p.CustomBranding), FeatureSet: p.FeatureSet,
			RecordVersion: p.RecordVersion,
		}
	}
	return out
}

// ── Test: WithLogger ──────────────────────────────────────────────────────

// TestCatalogService_WithLogger_ReturnsSelf verifies that WithLogger is
// fluent — it returns the same *CatalogService pointer, non-nil, so callers
// can chain it in the constructor expression.
func TestCatalogService_WithLogger_ReturnsSelf(t *testing.T) {
	client := &fakeCatalogAdminClient{}
	cache := newCatalogTestCache()
	svc := service.NewCatalogService(client, cache)

	log := &stubLogger{}
	got := svc.WithLogger(log)

	require.NotNil(t, got, "WithLogger must return a non-nil *CatalogService")
	assert.Same(t, svc, got, "WithLogger must return the same pointer (fluent)")
}

// ── Test group: Departments ───────────────────────────────────────────────

// TestCatalogService_Departments_ColdCacheLiveClientSuccess covers the cold
// cache path: om:departments is absent, the live client succeeds, and both
// the primary key (om:departments) and the stale key (om:departments:stale)
// are populated in the cache for subsequent calls.
func TestCatalogService_Departments_ColdCacheLiveClientSuccess(t *testing.T) {
	dtos := sampleDepts()
	client := &fakeCatalogAdminClient{
		departmentsFn: func(_ context.Context) ([]port.CatalogDepartment, error) {
			return dtos, nil
		},
	}
	cache := newCatalogTestCache()
	svc := service.NewCatalogService(client, cache)

	got, err := svc.Departments(context.Background())

	require.NoError(t, err)
	want := dtoDeptsToDomainDepts(dtos)
	assert.Equal(t, want, got)

	// Both cache keys must have been populated.
	assert.True(t, cache.has("om:departments"), "primary key om:departments must be set after live call")
	assert.True(t, cache.has("om:departments:stale"), "stale key om:departments:stale must be set after live call")
}

// TestCatalogService_Departments_CacheHit verifies that when the primary key
// is present in the cache, the live client is never invoked.
func TestCatalogService_Departments_CacheHit(t *testing.T) {
	depts := dtoDeptsToDomainDepts(sampleDepts())
	cache := newCatalogTestCache()
	cache.seed("om:departments", marshalDomainDepts(t, depts))

	clientCalled := false
	client := &fakeCatalogAdminClient{
		departmentsFn: func(_ context.Context) ([]port.CatalogDepartment, error) {
			clientCalled = true
			return nil, errors.New("should not be called")
		},
	}
	svc := service.NewCatalogService(client, cache)

	got, err := svc.Departments(context.Background())

	require.NoError(t, err)
	assert.False(t, clientCalled, "client must not be called on a cache hit")
	assert.Equal(t, depts, got)
}

// TestCatalogService_Departments_LiveCallFailsStaleHit covers CAT-D4: the
// primary key is missing, the live client errors, but the stale key holds
// valid data — stale data must be returned with no error.
func TestCatalogService_Departments_LiveCallFailsStaleHit(t *testing.T) {
	staleDepts := dtoDeptsToDomainDepts(sampleDepts())
	cache := newCatalogTestCache()
	// Only seed the stale key; the primary key is absent.
	cache.seed("om:departments:stale", marshalDomainDepts(t, staleDepts))

	client := &fakeCatalogAdminClient{
		departmentsFn: func(_ context.Context) ([]port.CatalogDepartment, error) {
			return nil, errors.New("catalog service down")
		},
	}
	svc := service.NewCatalogService(client, cache)

	got, err := svc.Departments(context.Background())

	require.NoError(t, err, "stale fallback must suppress the live-call error")
	assert.Equal(t, staleDepts, got, "stale data must be returned as-is")
}

// TestCatalogService_Departments_LiveCallFailsNoCacheFail verifies that when
// both the primary and stale cache keys are absent and the live client errors,
// Departments returns domain.ErrCatalogUnavailable.
func TestCatalogService_Departments_LiveCallFailsNoCacheFail(t *testing.T) {
	cache := newCatalogTestCache() // empty — both keys absent
	client := &fakeCatalogAdminClient{
		departmentsFn: func(_ context.Context) ([]port.CatalogDepartment, error) {
			return nil, errors.New("catalog service down")
		},
	}
	svc := service.NewCatalogService(client, cache)

	_, err := svc.Departments(context.Background())

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrCatalogUnavailable,
		"must return ErrCatalogUnavailable when both cache tiers and the live call fail")
}

// TestCatalogService_Departments_NilCache verifies that CatalogService works
// correctly when cache=nil — it falls through directly to the live client on
// every call (no cache population, no cache reads).
func TestCatalogService_Departments_NilCache(t *testing.T) {
	dtos := sampleDepts()
	callCount := 0
	client := &fakeCatalogAdminClient{
		departmentsFn: func(_ context.Context) ([]port.CatalogDepartment, error) {
			callCount++
			return dtos, nil
		},
	}
	svc := service.NewCatalogService(client, nil) // nil cache

	got, err := svc.Departments(context.Background())

	require.NoError(t, err)
	assert.Equal(t, dtoDeptsToDomainDepts(dtos), got)
	assert.Equal(t, 1, callCount, "client called exactly once even with nil cache")

	// Second call — must hit the client again since there is no cache.
	_, _ = svc.Departments(context.Background())
	assert.Equal(t, 2, callCount, "client called again on second call with nil cache")
}

// ── Test group: DepartmentByID ────────────────────────────────────────────

// TestCatalogService_DepartmentByID_Found calls Departments internally and
// finds the department by its UUID.
func TestCatalogService_DepartmentByID_Found(t *testing.T) {
	dtos := sampleDepts()
	want := dtoDeptsToDomainDepts(dtos)[0] // first dept
	client := &fakeCatalogAdminClient{
		departmentsFn: func(_ context.Context) ([]port.CatalogDepartment, error) {
			return dtos, nil
		},
	}
	svc := service.NewCatalogService(client, newCatalogTestCache())

	got, err := svc.DepartmentByID(context.Background(), want.ID)

	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, want, *got)
}

// TestCatalogService_DepartmentByID_NotFound verifies that a UUID not present
// in the catalog returns domain.ErrDepartmentNotFound.
func TestCatalogService_DepartmentByID_NotFound(t *testing.T) {
	dtos := sampleDepts()
	client := &fakeCatalogAdminClient{
		departmentsFn: func(_ context.Context) ([]port.CatalogDepartment, error) {
			return dtos, nil
		},
	}
	svc := service.NewCatalogService(client, newCatalogTestCache())

	_, err := svc.DepartmentByID(context.Background(), uuid.New()) // random UUID not in catalog

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrDepartmentNotFound)
}

// ── Test group: Plans ─────────────────────────────────────────────────────

// TestCatalogService_Plans_ColdCacheLiveClientSuccess mirrors the departments
// cold-cache happy path for the plans catalog.
func TestCatalogService_Plans_ColdCacheLiveClientSuccess(t *testing.T) {
	dtos := samplePlans()
	client := &fakeCatalogAdminClient{
		plansFn: func(_ context.Context) ([]port.CatalogPlan, error) {
			return dtos, nil
		},
	}
	cache := newCatalogTestCache()
	svc := service.NewCatalogService(client, cache)

	got, err := svc.Plans(context.Background())

	require.NoError(t, err)
	want := dtoPlansToDomainPlans(dtos)
	assert.Equal(t, want, got)

	assert.True(t, cache.has("om:plans"), "primary key om:plans must be set after live call")
	assert.True(t, cache.has("om:plans:stale"), "stale key om:plans:stale must be set after live call")
}

// TestCatalogService_Plans_CacheHit verifies that a populated om:plans key
// prevents the live client from being called.
func TestCatalogService_Plans_CacheHit(t *testing.T) {
	plans := dtoPlansToDomainPlans(samplePlans())
	cache := newCatalogTestCache()
	cache.seed("om:plans", marshalDomainPlans(t, plans))

	clientCalled := false
	client := &fakeCatalogAdminClient{
		plansFn: func(_ context.Context) ([]port.CatalogPlan, error) {
			clientCalled = true
			return nil, errors.New("should not be called")
		},
	}
	svc := service.NewCatalogService(client, cache)

	got, err := svc.Plans(context.Background())

	require.NoError(t, err)
	assert.False(t, clientCalled, "client must not be called on a plans cache hit")
	assert.Equal(t, plans, got)
}

// TestCatalogService_Plans_LiveCallFailsStaleHit covers the stale-if-error
// fallback for the plans catalog (CAT-D4 applied to om:plans).
func TestCatalogService_Plans_LiveCallFailsStaleHit(t *testing.T) {
	stalePlans := dtoPlansToDomainPlans(samplePlans())
	cache := newCatalogTestCache()
	cache.seed("om:plans:stale", marshalDomainPlans(t, stalePlans))

	client := &fakeCatalogAdminClient{
		plansFn: func(_ context.Context) ([]port.CatalogPlan, error) {
			return nil, errors.New("catalog service down")
		},
	}
	svc := service.NewCatalogService(client, cache)

	got, err := svc.Plans(context.Background())

	require.NoError(t, err, "stale fallback must suppress the live-call error for plans")
	assert.Equal(t, stalePlans, got, "stale plans must be returned as-is")
}

// TestCatalogService_Plans_LiveCallFailsNoCacheFail ensures that when both
// the primary and stale plan keys are absent and the live client fails,
// Plans returns domain.ErrCatalogUnavailable.
func TestCatalogService_Plans_LiveCallFailsNoCacheFail(t *testing.T) {
	cache := newCatalogTestCache() // empty
	client := &fakeCatalogAdminClient{
		plansFn: func(_ context.Context) ([]port.CatalogPlan, error) {
			return nil, errors.New("catalog service down")
		},
	}
	svc := service.NewCatalogService(client, cache)

	_, err := svc.Plans(context.Background())

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrCatalogUnavailable,
		"must return ErrCatalogUnavailable when both plans cache tiers and live call fail")
}

// ── Test group: PlanByCode ────────────────────────────────────────────────

// TestCatalogService_PlanByCode_Found delegates to Plans then matches by code.
func TestCatalogService_PlanByCode_Found(t *testing.T) {
	dtos := samplePlans()
	want := dtoPlansToDomainPlans(dtos)[1] // PlanPro
	client := &fakeCatalogAdminClient{
		plansFn: func(_ context.Context) ([]port.CatalogPlan, error) {
			return dtos, nil
		},
	}
	svc := service.NewCatalogService(client, newCatalogTestCache())

	got, err := svc.PlanByCode(context.Background(), domain.PlanPro)

	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, want, *got)
}

// TestCatalogService_PlanByCode_NotFound verifies that an unknown plan code
// returns domain.ErrPlanNotFound.
func TestCatalogService_PlanByCode_NotFound(t *testing.T) {
	dtos := samplePlans()
	client := &fakeCatalogAdminClient{
		plansFn: func(_ context.Context) ([]port.CatalogPlan, error) {
			return dtos, nil
		},
	}
	svc := service.NewCatalogService(client, newCatalogTestCache())

	_, err := svc.PlanByCode(context.Background(), domain.TenantPlan("unknown_plan"))

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrPlanNotFound)
}

// ── Test group: nil-cache and bad-JSON edge cases ─────────────────────────

// TestCatalogService_setCachedDepartments_NilCache exercises the nil-cache
// guard in setCachedDepartments indirectly by verifying that Departments
// still succeeds when cache=nil (no panic on Set path either).
func TestCatalogService_setCachedDepartments_NilCache(t *testing.T) {
	dtos := sampleDepts()
	client := &fakeCatalogAdminClient{
		departmentsFn: func(_ context.Context) ([]port.CatalogDepartment, error) {
			return dtos, nil
		},
	}
	// Explicitly nil cache — covers both getCachedDepartments and
	// setCachedDepartments nil guards.
	svc := service.NewCatalogService(client, nil)

	got, err := svc.Departments(context.Background())

	require.NoError(t, err, "nil cache must not cause a panic or error on Departments")
	assert.Equal(t, dtoDeptsToDomainDepts(dtos), got)
}

// TestCatalogService_getCachedDepartments_BadJSON verifies that if the cache
// stores invalid JSON under the primary departments key, the service treats it
// as a cache miss and falls back to the live client rather than returning an
// error to the caller.
func TestCatalogService_getCachedDepartments_BadJSON(t *testing.T) {
	cache := newCatalogTestCache()
	cache.seed("om:departments", []byte(`this is not valid JSON`))

	dtos := sampleDepts()
	clientCalled := false
	client := &fakeCatalogAdminClient{
		departmentsFn: func(_ context.Context) ([]port.CatalogDepartment, error) {
			clientCalled = true
			return dtos, nil
		},
	}
	svc := service.NewCatalogService(client, cache)

	got, err := svc.Departments(context.Background())

	require.NoError(t, err, "bad JSON in primary cache must fall through to live client without error")
	assert.True(t, clientCalled, "live client must be called after bad-JSON cache miss")
	assert.Equal(t, dtoDeptsToDomainDepts(dtos), got)
}

// TestCatalogService_getCachedPlans_BadJSON mirrors the departments bad-JSON
// test for the plans catalog: invalid JSON under om:plans must be treated as a
// miss and the live client must be invoked instead.
func TestCatalogService_getCachedPlans_BadJSON(t *testing.T) {
	cache := newCatalogTestCache()
	cache.seed("om:plans", []byte(`{bad json`))

	dtos := samplePlans()
	clientCalled := false
	client := &fakeCatalogAdminClient{
		plansFn: func(_ context.Context) ([]port.CatalogPlan, error) {
			clientCalled = true
			return dtos, nil
		},
	}
	svc := service.NewCatalogService(client, cache)

	got, err := svc.Plans(context.Background())

	require.NoError(t, err, "bad JSON in primary plans cache must fall through to live client without error")
	assert.True(t, clientCalled, "live client must be called after bad-JSON plans cache miss")
	assert.Equal(t, dtoPlansToDomainPlans(dtos), got)
}

// ── Additional edge-case coverage ─────────────────────────────────────────

// TestCatalogService_Departments_WithLogger_StalePathEmitsWarn verifies that
// the stale-if-error fallback path emits a warning via the injected Logger
// (i.e. WithLogger wiring is exercised end-to-end under a real fallback scenario).
func TestCatalogService_Departments_WithLogger_StalePathEmitsWarn(t *testing.T) {
	staleDepts := dtoDeptsToDomainDepts(sampleDepts())
	cache := newCatalogTestCache()
	cache.seed("om:departments:stale", marshalDomainDepts(t, staleDepts))

	client := &fakeCatalogAdminClient{
		departmentsFn: func(_ context.Context) ([]port.CatalogDepartment, error) {
			return nil, errors.New("catalog service down")
		},
	}

	log := &stubLogger{}
	svc := service.NewCatalogService(client, cache).WithLogger(log)

	got, err := svc.Departments(context.Background())

	require.NoError(t, err)
	assert.Equal(t, staleDepts, got)
	assert.GreaterOrEqual(t, log.warnCalls, 1,
		"a Warn must be emitted through the injected Logger on the stale fallback path")
}

// TestCatalogService_Plans_WithLogger_StalePathEmitsWarn mirrors the above
// for the plans catalog.
func TestCatalogService_Plans_WithLogger_StalePathEmitsWarn(t *testing.T) {
	stalePlans := dtoPlansToDomainPlans(samplePlans())
	cache := newCatalogTestCache()
	cache.seed("om:plans:stale", marshalDomainPlans(t, stalePlans))

	client := &fakeCatalogAdminClient{
		plansFn: func(_ context.Context) ([]port.CatalogPlan, error) {
			return nil, errors.New("catalog service down")
		},
	}

	log := &stubLogger{}
	svc := service.NewCatalogService(client, cache).WithLogger(log)

	got, err := svc.Plans(context.Background())

	require.NoError(t, err)
	assert.Equal(t, stalePlans, got)
	assert.GreaterOrEqual(t, log.warnCalls, 1,
		"a Warn must be emitted through the injected Logger on the stale plans fallback path")
}

// TestCatalogService_Departments_SecondCallUsesCachedValue verifies that the
// primary key written on the first live call is served on the second call,
// meaning the live client is only invoked once for two consecutive Departments
// calls in the same process (cache warm-up correctness).
func TestCatalogService_Departments_SecondCallUsesCachedValue(t *testing.T) {
	dtos := sampleDepts()
	callCount := 0
	client := &fakeCatalogAdminClient{
		departmentsFn: func(_ context.Context) ([]port.CatalogDepartment, error) {
			callCount++
			return dtos, nil
		},
	}
	cache := newCatalogTestCache()
	svc := service.NewCatalogService(client, cache)

	_, err := svc.Departments(context.Background())
	require.NoError(t, err)

	_, err = svc.Departments(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 1, callCount, "live client must only be called once after warm-up")
}

// TestCatalogService_Plans_SecondCallUsesCachedValue mirrors the above for plans.
func TestCatalogService_Plans_SecondCallUsesCachedValue(t *testing.T) {
	dtos := samplePlans()
	callCount := 0
	client := &fakeCatalogAdminClient{
		plansFn: func(_ context.Context) ([]port.CatalogPlan, error) {
			callCount++
			return dtos, nil
		},
	}
	cache := newCatalogTestCache()
	svc := service.NewCatalogService(client, cache)

	_, err := svc.Plans(context.Background())
	require.NoError(t, err)

	_, err = svc.Plans(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 1, callCount, "live plans client must only be called once after warm-up")
}

// TestCatalogService_DepartmentByID_PropagatesCatalogUnavailable verifies
// that DepartmentByID surfaces ErrCatalogUnavailable when both cache tiers
// and the live Departments call all fail — it must not swallow the error.
func TestCatalogService_DepartmentByID_PropagatesCatalogUnavailable(t *testing.T) {
	cache := newCatalogTestCache() // both keys absent
	client := &fakeCatalogAdminClient{
		departmentsFn: func(_ context.Context) ([]port.CatalogDepartment, error) {
			return nil, errors.New("catalog service down")
		},
	}
	svc := service.NewCatalogService(client, cache)

	_, err := svc.DepartmentByID(context.Background(), uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrCatalogUnavailable,
		"DepartmentByID must propagate ErrCatalogUnavailable from the underlying Departments call")
}

// TestCatalogService_PlanByCode_PropagatesCatalogUnavailable mirrors the
// above for PlanByCode.
func TestCatalogService_PlanByCode_PropagatesCatalogUnavailable(t *testing.T) {
	cache := newCatalogTestCache() // empty
	client := &fakeCatalogAdminClient{
		plansFn: func(_ context.Context) ([]port.CatalogPlan, error) {
			return nil, errors.New("catalog service down")
		},
	}
	svc := service.NewCatalogService(client, cache)

	_, err := svc.PlanByCode(context.Background(), domain.PlanStarter)

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrCatalogUnavailable,
		"PlanByCode must propagate ErrCatalogUnavailable from the underlying Plans call")
}

// TestCatalogService_Departments_EmptyClientResult verifies that an empty
// but non-error Departments response from the client is accepted gracefully
// (returned as an empty slice, cache is still populated).
func TestCatalogService_Departments_EmptyClientResult(t *testing.T) {
	client := &fakeCatalogAdminClient{
		departmentsFn: func(_ context.Context) ([]port.CatalogDepartment, error) {
			return []port.CatalogDepartment{}, nil
		},
	}
	cache := newCatalogTestCache()
	svc := service.NewCatalogService(client, cache)

	got, err := svc.Departments(context.Background())

	require.NoError(t, err)
	assert.Empty(t, got, "empty client result must produce an empty domain slice")
	assert.True(t, cache.has("om:departments"), "cache must be populated even for an empty result")
}

// TestCatalogService_Plans_EmptyClientResult mirrors the above for plans.
func TestCatalogService_Plans_EmptyClientResult(t *testing.T) {
	client := &fakeCatalogAdminClient{
		plansFn: func(_ context.Context) ([]port.CatalogPlan, error) {
			return []port.CatalogPlan{}, nil
		},
	}
	cache := newCatalogTestCache()
	svc := service.NewCatalogService(client, cache)

	got, err := svc.Plans(context.Background())

	require.NoError(t, err)
	assert.Empty(t, got, "empty client result must produce an empty domain Plan slice")
	assert.True(t, cache.has("om:plans"), "om:plans cache must be populated even for an empty plan result")
}

// TestCatalogService_Plans_NilCache_LiveClientSuccess verifies the nil-cache
// path for Plans — mirrors TestCatalogService_Departments_NilCache.
func TestCatalogService_Plans_NilCache_LiveClientSuccess(t *testing.T) {
	dtos := samplePlans()
	callCount := 0
	client := &fakeCatalogAdminClient{
		plansFn: func(_ context.Context) ([]port.CatalogPlan, error) {
			callCount++
			return dtos, nil
		},
	}
	svc := service.NewCatalogService(client, nil)

	got, err := svc.Plans(context.Background())

	require.NoError(t, err)
	assert.Equal(t, dtoPlansToDomainPlans(dtos), got)
	assert.Equal(t, 1, callCount)

	_, _ = svc.Plans(context.Background())
	assert.Equal(t, 2, callCount, "client called again on second call with nil cache")
}

// TestCatalogService_Departments_BrandingLevel_Conversion sanity-checks that
// the CustomBranding string from CatalogPlan is preserved as domain.BrandingLevel.
func TestCatalogService_Plans_BrandingLevel_Conversion(t *testing.T) {
	dtos := []port.CatalogPlan{
		{Code: domain.PlanEnterprise, DisplayName: "Enterprise", CustomBranding: "logo",
			FeatureSet: map[string]any{}, RecordVersion: 1},
	}
	client := &fakeCatalogAdminClient{
		plansFn: func(_ context.Context) ([]port.CatalogPlan, error) {
			return dtos, nil
		},
	}
	svc := service.NewCatalogService(client, newCatalogTestCache())

	plans, err := svc.Plans(context.Background())
	require.NoError(t, err)
	require.Len(t, plans, 1)
	assert.Equal(t, domain.BrandingLogo, plans[0].CustomBranding,
		"CustomBranding string 'logo' must map to domain.BrandingLogo")
}
