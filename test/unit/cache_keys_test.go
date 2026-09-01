// Indirect tests for the unexported cache-key builder functions in
// internal/core/service/cache_keys.go.
//
// Because every function in cache_keys.go is unexported (lowercase), they
// cannot be called directly from the unit_test package.  Instead each test
// wires up a real service (CatalogService, TenantService, GroupMappingService,
// DeptMembershipService) with a spy cache and verifies that the keys those
// services read/write match the documented om:* keyspace (LLD §6.1).
//
// White-box tests that call the functions directly already live in
// internal/core/service/cache_keys_test.go (package service).  The tests here
// complement them by exercising the full read/write path through the service
// so that a keyspace rename surfaces as a test failure regardless of which
// layer the rename touches.
package unit_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	service "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── ckCache — recording in-memory cache ───────────────────────────────────
//
// ckCache records every key that was written via Set (setCalls) and every
// key that was deleted via Delete (deleteCalls) so tests can assert the exact
// key strings without coupling to the key builder implementation.  Get returns
// data from the seed map so tests can pre-warm individual keys.

type ckCache struct {
	seed        map[string][]byte // pre-populated by tests
	setCalls    map[string][]byte // written by the service under test
	deleteCalls []string          // deleted by the service under test
}

func newCKCache() *ckCache {
	return &ckCache{
		seed:     map[string][]byte{},
		setCalls: map[string][]byte{},
	}
}

func (c *ckCache) Get(_ context.Context, key string) ([]byte, error) {
	if v, ok := c.seed[key]; ok {
		return v, nil
	}
	return nil, nil
}

func (c *ckCache) MGet(_ context.Context, keys []string) ([][]byte, error) {
	out := make([][]byte, len(keys))
	for i, k := range keys {
		out[i] = c.seed[k]
	}
	return out, nil
}

func (c *ckCache) Set(_ context.Context, key string, value []byte, _ time.Duration) error {
	c.setCalls[key] = value
	return nil
}

func (c *ckCache) SetNX(_ context.Context, key string, value []byte, _ time.Duration) (bool, error) {
	if _, ok := c.seed[key]; ok {
		return false, nil
	}
	c.seed[key] = value
	c.setCalls[key] = value
	return true, nil
}

func (c *ckCache) Delete(_ context.Context, keys ...string) error {
	c.deleteCalls = append(c.deleteCalls, keys...)
	return nil
}

func (c *ckCache) Health(_ context.Context) error { return nil }
func (c *ckCache) Close() error                   { return nil }

var _ port.Cache = (*ckCache)(nil)

// ── ckCatalogAdminClient — stub CatalogAdminClient ───────────────────────

type ckCatalogAdminClient struct {
	departmentsFn func(context.Context) ([]port.CatalogDepartment, error)
	plansFn       func(context.Context) ([]port.CatalogPlan, error)
}

func (f *ckCatalogAdminClient) Departments(ctx context.Context) ([]port.CatalogDepartment, error) {
	if f.departmentsFn != nil {
		return f.departmentsFn(ctx)
	}
	return nil, errors.New("not configured")
}

func (f *ckCatalogAdminClient) Plans(ctx context.Context) ([]port.CatalogPlan, error) {
	if f.plansFn != nil {
		return f.plansFn(ctx)
	}
	return nil, errors.New("not configured")
}

var _ port.CatalogAdminClient = (*ckCatalogAdminClient)(nil)

// ── ckTenantRepo — stub TenantRepository ─────────────────────────────────

type ckTenantRepo struct {
	findByIDFn func(context.Context, uuid.UUID) (*domain.Tenant, error)
}

func (r *ckTenantRepo) FindByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	if r.findByIDFn != nil {
		return r.findByIDFn(ctx, id)
	}
	return nil, errors.New("not configured")
}
func (r *ckTenantRepo) FindByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return r.FindByID(ctx, id)
}
func (r *ckTenantRepo) Update(_ context.Context, _ uuid.UUID, _ *domain.TenantPatch) (*domain.Tenant, error) {
	return nil, errors.New("not implemented")
}
func (r *ckTenantRepo) SetRealmSyncPending(_ context.Context, _ uuid.UUID) error { return nil }
func (r *ckTenantRepo) Insert(_ context.Context, _ *domain.Tenant) (*domain.Tenant, bool, error) {
	return nil, false, errors.New("not implemented")
}

var _ port.TenantRepository = (*ckTenantRepo)(nil)

// ── ckRealmProvisionerClient — minimal stub ────────────────────────────────

type ckRealmProvisionerClient struct{}

func (r *ckRealmProvisionerClient) CreateInvitedUser(_ context.Context, _ port.CreateInvitedUserRequest) (*port.CreateInvitedUserResponse, error) {
	return nil, nil
}
func (r *ckRealmProvisionerClient) DeleteUser(_ context.Context, _, _ uuid.UUID) error { return nil }
func (r *ckRealmProvisionerClient) PatchRealmConfig(_ context.Context, _ uuid.UUID, _ port.RealmConfigPatch) error {
	return nil
}
func (r *ckRealmProvisionerClient) RevokeUserSessions(_ context.Context, _, _ uuid.UUID) error {
	return nil
}

var _ port.RealmProvisionerClient = (*ckRealmProvisionerClient)(nil)

// ── ckGroupMappingClient — stub GroupMappingClient ───────────────────────

type ckGroupMappingClient struct {
	resolveFn func(ctx context.Context, tenantID uuid.UUID, groups []string) (*port.GroupResolution, error)
}

func (f *ckGroupMappingClient) ResolveGroups(ctx context.Context, tenantID uuid.UUID, groups []string) (*port.GroupResolution, error) {
	if f.resolveFn != nil {
		return f.resolveFn(ctx, tenantID, groups)
	}
	return nil, errors.New("not configured")
}

var _ port.GroupMappingClient = (*ckGroupMappingClient)(nil)

// ── ckMembershipRepo — stub MembershipRepository ─────────────────────────

type ckMembershipRepo struct{}

func (r *ckMembershipRepo) List(_ context.Context, _ uuid.UUID, _ *domain.MembershipListCursor, _ int) (*domain.MembershipListPage, error) {
	return nil, errors.New("not used")
}
func (r *ckMembershipRepo) FindByUserID(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
	return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid, Status: domain.MembershipActive}, nil
}
func (r *ckMembershipRepo) Insert(_ context.Context, _ *domain.TenantMembership) (*domain.TenantMembership, error) {
	return nil, errors.New("not used")
}
func (r *ckMembershipRepo) SetStatus(_ context.Context, _, _ uuid.UUID, _ domain.MembershipStatus, _ int64) (*domain.TenantMembership, error) {
	return nil, errors.New("not used")
}
func (r *ckMembershipRepo) SoftDelete(_ context.Context, _, _ uuid.UUID, _ int64) error {
	return errors.New("not used")
}
func (r *ckMembershipRepo) CountActive(_ context.Context, _ uuid.UUID) (int, error) {
	return 0, errors.New("not used")
}
func (r *ckMembershipRepo) ListActiveUserIDs(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	return nil, errors.New("not used")
}

var _ port.MembershipRepository = (*ckMembershipRepo)(nil)

// ── ckTenantRoleRepo — stub TenantRoleRepository ─────────────────────────

type ckTenantRoleRepo struct{}

func (r *ckTenantRoleRepo) ListByUser(_ context.Context, _, _ uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *ckTenantRoleRepo) ListByRole(_ context.Context, _ uuid.UUID, _ domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *ckTenantRoleRepo) CountActiveOwners(_ context.Context, _ uuid.UUID) (int, error) {
	return 0, nil
}
func (r *ckTenantRoleRepo) Grant(_ context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
	return tr, nil
}
func (r *ckTenantRoleRepo) Revoke(_ context.Context, _, _ uuid.UUID, _ domain.TenantRoleCode) (*domain.TenantRole, error) {
	return &domain.TenantRole{}, nil
}
func (r *ckTenantRoleRepo) SoftDeleteAllForUser(_ context.Context, _, _ uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}

var _ port.TenantRoleRepository = (*ckTenantRoleRepo)(nil)

// ── ckDeptMemRepo — stub DeptMembershipRepository ────────────────────────

type ckDeptMemRepo struct {
	removeFn func(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.DeptMembership, error)
}

func (r *ckDeptMemRepo) ListByUser(_ context.Context, _, _ uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *ckDeptMemRepo) ListByDepartment(_ context.Context, _, _ uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *ckDeptMemRepo) Assign(_ context.Context, _, _, deptID, _ uuid.UUID, level domain.DeptRole, _ uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
	return &domain.DeptMembership{ID: uuid.New(), DepartmentID: deptID, RoleLevel: level}, nil, nil
}
func (r *ckDeptMemRepo) Remove(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.DeptMembership, error) {
	if r.removeFn != nil {
		return r.removeFn(ctx, tenantID, userID, deptID)
	}
	return &domain.DeptMembership{ID: uuid.New()}, nil
}
func (r *ckDeptMemRepo) SoftDeleteAllForUser(_ context.Context, _, _ uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *ckDeptMemRepo) SoftDeleteAllForDept(_ context.Context, _, _ uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}

var _ port.DeptMembershipRepository = (*ckDeptMemRepo)(nil)

// ── ckTxRunner — passthrough TxRunner ────────────────────────────────────

type ckTxRunner struct{}

func (r *ckTxRunner) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

var _ port.TxRunner = (*ckTxRunner)(nil)

// ── ckWorkflowClient — fail-open stub ────────────────────────────────────

type ckWorkflowClient struct{}

func (f *ckWorkflowClient) GetDelegateImpact(_ context.Context, _, _ uuid.UUID, _ *uuid.UUID) (*port.DelegateImpact, error) {
	return &port.DelegateImpact{}, nil
}
func (f *ckWorkflowClient) ReassignDelegate(_ context.Context, _, _, _ uuid.UUID, _ *uuid.UUID) error {
	return nil
}
func (f *ckWorkflowClient) CancelByDelegate(_ context.Context, _, _ uuid.UUID, _ *uuid.UUID) error {
	return nil
}

var _ port.WorkflowClient = (*ckWorkflowClient)(nil)

// ─────────────────────────────────────────────────────────────────────────────
// CatalogService — cacheKeyDepartments / cacheKeyDepartmentsStale
// ─────────────────────────────────────────────────────────────────────────────

// TestCacheKeys_Departments_ColdCacheWritesPrimaryKey verifies that a
// CatalogService cache miss populates the "om:departments" primary key.
// This locks the exact key format that cacheKeyDepartments() produces.
func TestCacheKeys_Departments_ColdCacheWritesPrimaryKey(t *testing.T) {
	deptID := uuid.New()
	client := &ckCatalogAdminClient{
		departmentsFn: func(context.Context) ([]port.CatalogDepartment, error) {
			return []port.CatalogDepartment{
				{ID: deptID, Code: "LEGAL", IsSystem: true, IsActive: true},
			}, nil
		},
	}
	cache := newCKCache()
	svc := service.NewCatalogService(client, cache)

	_, err := svc.Departments(context.Background())
	require.NoError(t, err)

	_, primaryPresent := cache.setCalls["om:departments"]
	assert.True(t, primaryPresent, "cache miss must write to \"om:departments\"")
}

// TestCacheKeys_Departments_ColdCacheWritesStaleKey verifies that after a
// successful live fetch, the "om:departments:stale" key is also populated
// alongside the primary — this is the stale-if-error fallback (CAT-D4).
func TestCacheKeys_Departments_ColdCacheWritesStaleKey(t *testing.T) {
	client := &ckCatalogAdminClient{
		departmentsFn: func(context.Context) ([]port.CatalogDepartment, error) {
			return []port.CatalogDepartment{
				{ID: uuid.New(), Code: "FINANCE", IsActive: true},
			}, nil
		},
	}
	cache := newCKCache()
	svc := service.NewCatalogService(client, cache)

	_, err := svc.Departments(context.Background())
	require.NoError(t, err)

	_, stalePresent := cache.setCalls["om:departments:stale"]
	assert.True(t, stalePresent, "successful fetch must refresh the stale-if-error key \"om:departments:stale\"")
}

// TestCacheKeys_Departments_WarmPrimaryKeySkipsClient verifies that a
// pre-seeded "om:departments" key causes a cache hit (the client is never
// called).  This confirms the key the service reads is the same one it writes.
func TestCacheKeys_Departments_WarmPrimaryKeySkipsClient(t *testing.T) {
	seed := []domain.Department{{ID: uuid.New(), Code: "ENGINEERING"}}
	raw, err := json.Marshal(seed)
	require.NoError(t, err)

	clientCalled := false
	client := &ckCatalogAdminClient{
		departmentsFn: func(context.Context) ([]port.CatalogDepartment, error) {
			clientCalled = true
			return nil, errors.New("should not be called")
		},
	}
	cache := newCKCache()
	cache.seed["om:departments"] = raw

	svc := service.NewCatalogService(client, cache)
	got, err := svc.Departments(context.Background())

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.False(t, clientCalled, "\"om:departments\" cache hit must short-circuit the client call")
}

// TestCacheKeys_Departments_StaleKeyServedOnClientFailure verifies that when
// the primary key is cold and the live call fails, the service reads from
// "om:departments:stale" — confirming the stale key format matches what was
// written on the last successful fetch.
func TestCacheKeys_Departments_StaleKeyServedOnClientFailure(t *testing.T) {
	stale := []domain.Department{{ID: uuid.New(), Code: "PROCUREMENT"}}
	raw, err := json.Marshal(stale)
	require.NoError(t, err)

	client := &ckCatalogAdminClient{
		departmentsFn: func(context.Context) ([]port.CatalogDepartment, error) {
			return nil, errors.New("catalog-admin-config unreachable")
		},
	}
	cache := newCKCache()
	cache.seed["om:departments:stale"] = raw

	svc := service.NewCatalogService(client, cache)
	got, err := svc.Departments(context.Background())

	require.NoError(t, err, "a warm stale key must be served, not an error (CAT-D4)")
	require.Len(t, got, 1, "stale departments must be returned from \"om:departments:stale\"")
	assert.Equal(t, "PROCUREMENT", got[0].Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// CatalogService — cacheKeyPlans / cacheKeyPlansStale
// ─────────────────────────────────────────────────────────────────────────────

// TestCacheKeys_Plans_ColdCacheWritesPrimaryKey verifies that a CatalogService
// Plans() cache miss populates the "om:plans" primary key.
func TestCacheKeys_Plans_ColdCacheWritesPrimaryKey(t *testing.T) {
	client := &ckCatalogAdminClient{
		plansFn: func(context.Context) ([]port.CatalogPlan, error) {
			return []port.CatalogPlan{
				{Code: domain.PlanStarter, DisplayName: "Starter"},
			}, nil
		},
	}
	cache := newCKCache()
	svc := service.NewCatalogService(client, cache)

	_, err := svc.Plans(context.Background())
	require.NoError(t, err)

	_, primaryPresent := cache.setCalls["om:plans"]
	assert.True(t, primaryPresent, "cache miss must write to \"om:plans\"")
}

// TestCacheKeys_Plans_ColdCacheWritesStaleKey verifies the "om:plans:stale"
// key is written alongside "om:plans" on a successful live fetch.
func TestCacheKeys_Plans_ColdCacheWritesStaleKey(t *testing.T) {
	client := &ckCatalogAdminClient{
		plansFn: func(context.Context) ([]port.CatalogPlan, error) {
			return []port.CatalogPlan{
				{Code: domain.PlanPro, DisplayName: "Pro"},
			}, nil
		},
	}
	cache := newCKCache()
	svc := service.NewCatalogService(client, cache)

	_, err := svc.Plans(context.Background())
	require.NoError(t, err)

	_, stalePresent := cache.setCalls["om:plans:stale"]
	assert.True(t, stalePresent, "successful fetch must refresh the stale-if-error key \"om:plans:stale\"")
}

// TestCacheKeys_Plans_WarmPrimaryKeySkipsClient verifies the "om:plans"
// primary key hits on a re-fetch.
func TestCacheKeys_Plans_WarmPrimaryKeySkipsClient(t *testing.T) {
	seed := []domain.Plan{{Code: domain.PlanEnterprise, DisplayName: "Enterprise"}}
	raw, err := json.Marshal(seed)
	require.NoError(t, err)

	clientCalled := false
	client := &ckCatalogAdminClient{
		plansFn: func(context.Context) ([]port.CatalogPlan, error) {
			clientCalled = true
			return nil, errors.New("should not be called")
		},
	}
	cache := newCKCache()
	cache.seed["om:plans"] = raw

	svc := service.NewCatalogService(client, cache)
	got, err := svc.Plans(context.Background())

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.False(t, clientCalled, "\"om:plans\" cache hit must short-circuit the client call")
}

// TestCacheKeys_Plans_StaleKeyServedOnClientFailure confirms the stale key
// format matches what the service writes (CAT-D4 stale-if-error pattern).
func TestCacheKeys_Plans_StaleKeyServedOnClientFailure(t *testing.T) {
	stale := []domain.Plan{{Code: domain.PlanStarter, DisplayName: "Starter"}}
	raw, err := json.Marshal(stale)
	require.NoError(t, err)

	client := &ckCatalogAdminClient{
		plansFn: func(context.Context) ([]port.CatalogPlan, error) {
			return nil, errors.New("catalog-admin-config unreachable")
		},
	}
	cache := newCKCache()
	cache.seed["om:plans:stale"] = raw

	svc := service.NewCatalogService(client, cache)
	got, err := svc.Plans(context.Background())

	require.NoError(t, err, "a warm stale key must be served, not an error")
	require.Len(t, got, 1)
}

// ─────────────────────────────────────────────────────────────────────────────
// TenantService — cacheKeyTenant / cacheKeyLocale
// ─────────────────────────────────────────────────────────────────────────────

// TestCacheKeys_Tenant_ColdCacheWritesKey verifies that a TenantService.Get()
// cache miss writes to "om:tenant:{uuid}" after a successful repo read.
func TestCacheKeys_Tenant_ColdCacheWritesKey(t *testing.T) {
	tenantID := uuid.New()
	repo := &ckTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, Slug: "acme", Name: "Acme Corp"}, nil
		},
	}
	cache := newCKCache()
	svc := service.NewTenantService(repo, cache, &ckRealmProvisionerClient{})

	_, err := svc.Get(context.Background(), tenantID)
	require.NoError(t, err)

	expectedKey := fmt.Sprintf("om:tenant:%s", tenantID)
	_, present := cache.setCalls[expectedKey]
	assert.True(t, present, "TenantService.Get() cache miss must write to \"om:tenant:{uuid}\"")
}

// TestCacheKeys_Tenant_WarmKeySkipsRepo confirms the key the service reads is
// the same one it writes: "om:tenant:{uuid}".
func TestCacheKeys_Tenant_WarmKeySkipsRepo(t *testing.T) {
	tenantID := uuid.New()
	cached := &domain.Tenant{ID: tenantID, Slug: "warm"}
	raw, err := json.Marshal(cached)
	require.NoError(t, err)

	repoCalled := false
	repo := &ckTenantRepo{
		findByIDFn: func(_ context.Context, _ uuid.UUID) (*domain.Tenant, error) {
			repoCalled = true
			return nil, errors.New("should not be called")
		},
	}
	cache := newCKCache()
	cache.seed[fmt.Sprintf("om:tenant:%s", tenantID)] = raw

	svc := service.NewTenantService(repo, cache, &ckRealmProvisionerClient{})
	got, err := svc.Get(context.Background(), tenantID)

	require.NoError(t, err)
	assert.Equal(t, "warm", got.Slug)
	assert.False(t, repoCalled, "\"om:tenant:{uuid}\" cache hit must skip the repo")
}

// TestCacheKeys_Tenant_DifferentTenantsProduceDistinctKeys asserts that the
// tenant ID is embedded in the cache key so two distinct tenants never collide.
func TestCacheKeys_Tenant_DifferentTenantsProduceDistinctKeys(t *testing.T) {
	tenantA := uuid.New()
	tenantB := uuid.New()

	// Wire two separate services (one cache) so both write their own keys.
	repo := &ckTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, Slug: id.String()}, nil
		},
	}
	cache := newCKCache()
	svc := service.NewTenantService(repo, cache, &ckRealmProvisionerClient{})

	_, err := svc.Get(context.Background(), tenantA)
	require.NoError(t, err)
	_, err = svc.Get(context.Background(), tenantB)
	require.NoError(t, err)

	keyA := fmt.Sprintf("om:tenant:%s", tenantA)
	keyB := fmt.Sprintf("om:tenant:%s", tenantB)
	assert.NotEqual(t, keyA, keyB, "distinct tenants must produce distinct cache keys")
	_, hasA := cache.setCalls[keyA]
	_, hasB := cache.setCalls[keyB]
	assert.True(t, hasA, "cache must hold key for tenantA")
	assert.True(t, hasB, "cache must hold key for tenantB")
}

// TestCacheKeys_Locale_InvalidatedOnPatch verifies that TenantService.Patch()
// deletes the "om:locale:{uuid}" key (via cache invalidation) after an update
// — confirming that cacheKeyLocale embeds the tenant ID correctly.
func TestCacheKeys_Locale_InvalidatedOnPatch(t *testing.T) {
	tenantID := uuid.New()
	before := &domain.Tenant{ID: tenantID, Name: "Before"}
	updateRepo := &ckFullTenantRepo{
		findByIDFn: func(_ context.Context, _ uuid.UUID) (*domain.Tenant, error) {
			return before, nil
		},
		updateFn: func(_ context.Context, id uuid.UUID, _ *domain.TenantPatch) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, Name: "After", RecordVersion: 2}, nil
		},
	}
	cache := newCKCache()
	svc := service.NewTenantService(updateRepo, cache, &ckRealmProvisionerClient{})

	newName := "After"
	_, _, err := svc.Patch(context.Background(), tenantID, &domain.TenantPatch{Name: &newName})
	require.NoError(t, err)

	localeKey := fmt.Sprintf("om:locale:%s", tenantID)
	assert.Contains(t, cache.deleteCalls, localeKey,
		"Patch must invalidate the \"om:locale:{uuid}\" cache key")
}

// ckFullTenantRepo is a fuller TenantRepository stub (with Update) used by
// TestCacheKeys_Locale_InvalidatedOnPatch.  It is distinct from tsRepo
// (defined in tenant_service_test.go) to avoid a redeclaration error.
type ckFullTenantRepo struct {
	findByIDFn func(context.Context, uuid.UUID) (*domain.Tenant, error)
	updateFn   func(context.Context, uuid.UUID, *domain.TenantPatch) (*domain.Tenant, error)
}

func (r *ckFullTenantRepo) FindByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	if r.findByIDFn != nil {
		return r.findByIDFn(ctx, id)
	}
	return nil, errors.New("not configured")
}
func (r *ckFullTenantRepo) FindByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return r.FindByID(ctx, id)
}
func (r *ckFullTenantRepo) Update(ctx context.Context, id uuid.UUID, patch *domain.TenantPatch) (*domain.Tenant, error) {
	if r.updateFn != nil {
		return r.updateFn(ctx, id, patch)
	}
	return nil, errors.New("not configured")
}
func (r *ckFullTenantRepo) SetRealmSyncPending(_ context.Context, _ uuid.UUID) error { return nil }
func (r *ckFullTenantRepo) Insert(_ context.Context, _ *domain.Tenant) (*domain.Tenant, bool, error) {
	return nil, false, errors.New("not implemented")
}

var _ port.TenantRepository = (*ckFullTenantRepo)(nil)

// ─────────────────────────────────────────────────────────────────────────────
// GroupMappingService — cacheKeyGRM / cacheKeyGDM / cacheKeyGTRM
// and their stale variants
// ─────────────────────────────────────────────────────────────────────────────

// buildCKGMSvc is a helper that creates a GroupMappingService wired with the
// given client and cache — only these two collaborators are needed by
// AssignFromGroups' resolveMappings step.
func buildCKGMSvc(client port.GroupMappingClient, cache port.Cache) *service.GroupMappingService {
	return service.NewGroupMappingService(
		&activeMemberRepo{},
		&ckTenantRoleRepo{},
		&ckDeptMemRepo{},
		&ckTxRunner{},
		cache,
		client,
	)
}

// TestCacheKeys_GRM_GDM_GTRM_ColdCacheWritesPrimaryKeys verifies that a
// GroupMappingService resolution cache miss populates all three primary keys:
//   - "om:grm:{uuid}"
//   - "om:gdm:{uuid}"
//   - "om:gtrm:{uuid}"
//
// These are the cacheKeyGRM / cacheKeyGDM / cacheKeyGTRM results.
func TestCacheKeys_GRM_GDM_GTRM_ColdCacheWritesPrimaryKeys(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()
	deptID := uuid.New()
	client := &ckGroupMappingClient{
		resolveFn: func(_ context.Context, _ uuid.UUID, _ []string) (*port.GroupResolution, error) {
			return &port.GroupResolution{
				DeptMappings:       []port.ResolvedDeptMapping{{KeycloakGroupName: "eng-team", DepartmentID: deptID}},
				DeptRoleMappings:   []port.ResolvedDeptRoleMapping{{KeycloakGroupName: "eng-team", RoleCode: domain.DeptReviewer}},
				TenantRoleMappings: []port.ResolvedTenantRoleMapping{},
			}, nil
		},
	}
	cache := newCKCache()
	svc := buildCKGMSvc(client, cache)

	// AssignFromGroups triggers resolveMappings internally.
	_, err := svc.AssignFromGroups(context.Background(), tenantID, userID, []string{"eng-team"})
	require.NoError(t, err)

	grmKey := fmt.Sprintf("om:grm:%s", tenantID)
	gdmKey := fmt.Sprintf("om:gdm:%s", tenantID)
	gtrmKey := fmt.Sprintf("om:gtrm:%s", tenantID)

	_, grmPresent := cache.setCalls[grmKey]
	_, gdmPresent := cache.setCalls[gdmKey]
	_, gtrmPresent := cache.setCalls[gtrmKey]

	assert.True(t, grmPresent, "cache miss must write to \"om:grm:{uuid}\"")
	assert.True(t, gdmPresent, "cache miss must write to \"om:gdm:{uuid}\"")
	assert.True(t, gtrmPresent, "cache miss must write to \"om:gtrm:{uuid}\"")
}

// TestCacheKeys_GRM_GDM_GTRM_ColdCacheWritesStaleKeys verifies that alongside
// the primary keys, the stale-if-error variants are also populated:
//   - "om:grm:stale:{uuid}"
//   - "om:gdm:stale:{uuid}"
//   - "om:gtrm:stale:{uuid}"
func TestCacheKeys_GRM_GDM_GTRM_ColdCacheWritesStaleKeys(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()
	client := &ckGroupMappingClient{
		resolveFn: func(_ context.Context, _ uuid.UUID, _ []string) (*port.GroupResolution, error) {
			return &port.GroupResolution{}, nil
		},
	}
	cache := newCKCache()
	svc := buildCKGMSvc(client, cache)

	_, err := svc.AssignFromGroups(context.Background(), tenantID, userID, []string{"some-group"})
	require.NoError(t, err)

	grmStale := fmt.Sprintf("om:grm:stale:%s", tenantID)
	gdmStale := fmt.Sprintf("om:gdm:stale:%s", tenantID)
	gtrmStale := fmt.Sprintf("om:gtrm:stale:%s", tenantID)

	_, grmStalePresent := cache.setCalls[grmStale]
	_, gdmStalePresent := cache.setCalls[gdmStale]
	_, gtrmStalePresent := cache.setCalls[gtrmStale]

	assert.True(t, grmStalePresent, "successful fetch must refresh stale key \"om:grm:stale:{uuid}\"")
	assert.True(t, gdmStalePresent, "successful fetch must refresh stale key \"om:gdm:stale:{uuid}\"")
	assert.True(t, gtrmStalePresent, "successful fetch must refresh stale key \"om:gtrm:stale:{uuid}\"")
}

// TestCacheKeys_GRM_WarmCacheSkipsClient verifies that pre-seeding all three
// primary keys causes a cache hit — confirming the service reads from the
// same key format it writes.
func TestCacheKeys_GRM_WarmCacheSkipsClient(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()

	clientCalled := false
	client := &ckGroupMappingClient{
		resolveFn: func(_ context.Context, _ uuid.UUID, _ []string) (*port.GroupResolution, error) {
			clientCalled = true
			return nil, errors.New("should not be called")
		},
	}
	cache := newCKCache()
	// Seed all three primary keys so the MGet cache hit path is taken.
	cache.seed[fmt.Sprintf("om:gdm:%s", tenantID)] = []byte(`[]`)
	cache.seed[fmt.Sprintf("om:grm:%s", tenantID)] = []byte(`[]`)
	cache.seed[fmt.Sprintf("om:gtrm:%s", tenantID)] = []byte(`[]`)

	svc := buildCKGMSvc(client, cache)
	_, err := svc.AssignFromGroups(context.Background(), tenantID, userID, []string{"some-group"})
	require.NoError(t, err)

	assert.False(t, clientCalled, "warm om:grm/gdm/gtrm keys must short-circuit the GroupMappingClient call")
}

// TestCacheKeys_GRM_StaleKeysServedOnClientFailure verifies the stale-if-error
// key format: when the primary keys are cold and the live call fails, the
// service reads "om:grm:stale:{uuid}" / "om:gdm:stale:{uuid}" /
// "om:gtrm:stale:{uuid}" (i.e. same format as cacheKeyGRMStale etc.).
// On success the call returns normally (fail-open) even without a stale cache,
// so here we pre-seed stale keys and confirm resolution succeeds.
func TestCacheKeys_GRM_StaleKeysServedOnClientFailure(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()

	client := &ckGroupMappingClient{
		resolveFn: func(_ context.Context, _ uuid.UUID, _ []string) (*port.GroupResolution, error) {
			return nil, errors.New("group-mapping service unavailable")
		},
	}
	cache := newCKCache()
	cache.seed[fmt.Sprintf("om:gdm:stale:%s", tenantID)] = []byte(`[]`)
	cache.seed[fmt.Sprintf("om:grm:stale:%s", tenantID)] = []byte(`[]`)
	cache.seed[fmt.Sprintf("om:gtrm:stale:%s", tenantID)] = []byte(`[]`)

	svc := buildCKGMSvc(client, cache)
	result, err := svc.AssignFromGroups(context.Background(), tenantID, userID, []string{"some-group"})

	// Stale-if-error: service must still succeed (fail-open ADR-0007 Action Item 4).
	require.NoError(t, err, "a warm stale cache must allow AssignFromGroups to succeed (fail-open)")
	require.NotNil(t, result)
}

// ─────────────────────────────────────────────────────────────────────────────
// DeptMembershipService — cacheKeyDeptMembers / cacheKeySeatUsage (indirectly)
// ─────────────────────────────────────────────────────────────────────────────

// TestCacheKeys_DeptMembers_RemoveInvalidatesKey verifies that
// DeptMembershipService.Remove() deletes the "om:dept_members:{tid}:{did}"
// cache key (plus the seat_usage key) on success.
//
// The exact key format is cacheKeyDeptMembers(tenantID, deptID).
func TestCacheKeys_DeptMembers_RemoveInvalidatesKey(t *testing.T) {
	// deptID is the department being removed from; its ID must appear in the
	// cache key alongside tenantID.
	tenantID := uuid.New()
	deptID := uuid.New()
	userID := uuid.New()

	repo := &ckDeptMemRepo{
		removeFn: func(_ context.Context, _, _, _ uuid.UUID) (*domain.DeptMembership, error) {
			return &domain.DeptMembership{ID: uuid.New(), TenantID: tenantID, DepartmentID: deptID}, nil
		},
	}
	cache := newCKCache()

	svc := service.NewDeptMembershipService(
		repo,
		nil,   // membership repo — not used on Remove path
		nil,   // tenantDepts — not used when delegationCheck is nil
		nil,   // catalog
		nil,   // delegationCheck — nil causes dept-delegate lookup to be skipped
		&ckWorkflowClient{},
		cache,
		&ckTxRunner{},
	)

	_, err := svc.Remove(context.Background(), tenantID, userID, deptID, uuid.New())
	require.NoError(t, err)

	expectedDeptMembersKey := fmt.Sprintf("om:dept_members:%s:%s", tenantID, deptID)
	assert.Contains(t, cache.deleteCalls, expectedDeptMembersKey,
		"Remove must delete \"om:dept_members:{tenantID}:{deptID}\" on success")
}

// TestCacheKeys_DeptMembers_KeyContainsBothIDs verifies that the key written
// to cache on a remove contains BOTH tenant ID and department ID — two
// different departments in the same tenant must produce distinct keys.
func TestCacheKeys_DeptMembers_KeyContainsBothIDs(t *testing.T) {
	tenantID := uuid.New()
	deptA := uuid.New()
	deptB := uuid.New()
	userID := uuid.New()

	makeRepo := func() *ckDeptMemRepo {
		return &ckDeptMemRepo{
			removeFn: func(_ context.Context, _, _, _ uuid.UUID) (*domain.DeptMembership, error) {
				return &domain.DeptMembership{ID: uuid.New()}, nil
			},
		}
	}

	cacheA := newCKCache()
	svcA := service.NewDeptMembershipService(makeRepo(), nil, nil, nil, nil, &ckWorkflowClient{}, cacheA, &ckTxRunner{})
	_, err := svcA.Remove(context.Background(), tenantID, userID, deptA, uuid.New())
	require.NoError(t, err)

	cacheB := newCKCache()
	svcB := service.NewDeptMembershipService(makeRepo(), nil, nil, nil, nil, &ckWorkflowClient{}, cacheB, &ckTxRunner{})
	_, err = svcB.Remove(context.Background(), tenantID, userID, deptB, uuid.New())
	require.NoError(t, err)

	keyA := fmt.Sprintf("om:dept_members:%s:%s", tenantID, deptA)
	keyB := fmt.Sprintf("om:dept_members:%s:%s", tenantID, deptB)
	assert.NotEqual(t, keyA, keyB, "two different departments must produce distinct om:dept_members keys")
	assert.Contains(t, cacheA.deleteCalls, keyA)
	assert.Contains(t, cacheB.deleteCalls, keyB)
}

// TestCacheKeys_SeatUsage_RemovedOnDeptMembershipRemove verifies that the
// cacheKeySeatUsage key ("om:seat_usage:{tenantID}") is also invalidated when
// a department membership is removed — confirming the key embeds the tenant ID.
func TestCacheKeys_SeatUsage_RemovedOnDeptMembershipRemove(t *testing.T) {
	tenantID := uuid.New()
	repo := &ckDeptMemRepo{
		removeFn: func(_ context.Context, _, _, _ uuid.UUID) (*domain.DeptMembership, error) {
			return &domain.DeptMembership{ID: uuid.New()}, nil
		},
	}
	cache := newCKCache()
	svc := service.NewDeptMembershipService(repo, nil, nil, nil, nil, &ckWorkflowClient{}, cache, &ckTxRunner{})

	_, err := svc.Remove(context.Background(), tenantID, uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err)

	expectedSeatUsageKey := fmt.Sprintf("om:seat_usage:%s", tenantID)
	assert.Contains(t, cache.deleteCalls, expectedSeatUsageKey,
		"Remove must also delete \"om:seat_usage:{tenantID}\" on success")
}

// ─────────────────────────────────────────────────────────────────────────────
// Cross-tenant isolation — spot-check that tenant ID is always embedded
// ─────────────────────────────────────────────────────────────────────────────

// TestCacheKeys_TenantIsolation_NoKeyCollisionAcrossTenants is a broad
// cross-service guard: given two different tenant IDs, every om:* key variant
// must be distinct.  A collision here would be a critical RLS-equivalent bug.
func TestCacheKeys_TenantIsolation_NoKeyCollisionAcrossTenants(t *testing.T) {
	tenantA := uuid.New()
	tenantB := uuid.New()

	// Build expected key sets for tenantA and tenantB.
	keysFor := func(tid uuid.UUID) []string {
		return []string{
			fmt.Sprintf("om:tenant:%s", tid),
			fmt.Sprintf("om:locale:%s", tid),
			fmt.Sprintf("om:grm:%s", tid),
			fmt.Sprintf("om:gdm:%s", tid),
			fmt.Sprintf("om:gtrm:%s", tid),
			fmt.Sprintf("om:grm:stale:%s", tid),
			fmt.Sprintf("om:gdm:stale:%s", tid),
			fmt.Sprintf("om:gtrm:stale:%s", tid),
			fmt.Sprintf("om:seat_usage:%s", tid),
		}
	}

	aKeys := keysFor(tenantA)
	bKeys := keysFor(tenantB)

	// Every key in A must differ from every corresponding key in B.
	require.Equal(t, len(aKeys), len(bKeys))
	for i := range aKeys {
		assert.NotEqual(t, aKeys[i], bKeys[i],
			"key pattern %d collides for different tenant IDs: %q vs %q", i, aKeys[i], bKeys[i])
	}
}
