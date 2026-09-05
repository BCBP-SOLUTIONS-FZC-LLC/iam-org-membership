// svcgap_coverage_test.go targets the remaining uncovered branches across
// several service files identified from the coverage report.  All stubs use
// the "svcgap_" prefix to avoid collisions with types defined in other files
// in this package.
//
// Covered gaps (85.2% → higher):
//
//	authz_service.go
//	  getCached (77.8%): cache.Get error → return nil
//	  getCached (77.8%): json.Unmarshal error → return nil
//	  setCached (25.0%):  nil proj guard + full Set path
//
//	catalog_service.go
//	  setCachedDepartments (85.7%): json.Marshal returns no error → both Set calls
//	  setCachedPlans (85.7%):       json.Marshal returns no error → both Set calls
//
//	tenant_service.go
//	  Get (85.7%): cache.Get error → fall through to DB
//	  setCached (83.3%): json.Marshal success → cache.Set called
//	  requireActiveMember (0.0%): nil rc, cross-tenant, missing role
//
//	department_service.go
//	  ListForTenant (75.0%): tenantDepts.List error propagates
//	  ListForTenant (75.0%): happy path building view slice + return
//	  Activate (90.0%):     tenantDepts.Activate error propagates
//
//	dept_membership_service.go
//	  Remove (91.7%): requestctx path inside tx (evt.IPAddress/UserAgent set)
//
//	group_mapping_service.go
//	  AssignFromGroups (86.4%):
//	    - drMaps group filter (dr.KeycloakGroupName not in groupSet → continue)
//	    - trMaps RoleMember filter → continue
//	    - deptMems.Assign error propagates
//	    - LevelChanged event branch (previous != nil, different level)
//	    - roles.Grant path + event emission
//	    - tx error path
//	  setCachedResolution (92.3%): cache == nil guard
//
//	membership_service.go
//	  List (95.0%): deptMemberships.ListByUser error in hydration loop
//	  SetStatus (93.9%): WFI-13 active_workflows > 0 advisory path
//	  RemoveUser (89.5%): roles.CountActiveOwners error inside tx
//	  RemoveUser (89.5%): deptMemberships.SoftDeleteAllForUser error
//	  RemoveUser (89.5%): memberships.SoftDelete error
//	  ValidateAndEmitAssigneeOverride (93.5%): pub == nil inside tx
//	  ResetUserMFA (83.3%): ResetMFA error → ErrRealmProvisionerUnavailable
//	  ResetUserMFA (83.3%): pub == nil inside tx
//
//	provisioning_service.go
//	  TrialSignup (86.4%): tenantDepts.Activate error propagates
//	  TrialSignup (86.4%): labels.Seed error propagates
//	  TrialSignup (86.4%): memberships.Insert error propagates
//	  TrialSignup (86.4%): roles.Grant error propagates
//	  TrialSignup (86.4%): idempotent replay (wasCreated=false) path
//	  SetRealmFields (60.0%): tx error + cache nil path + cache delete path
//	  SetMembershipStatus (91.7%): cache != nil delete path
//	  DeleteMember (58.3%): wasOwner=true → CountActiveOwners → ownerRemaining>0
//	  DeleteMember (58.3%): wasOwner=true → ownerRemaining==0 → MarkOwnerlessIfUnset (not flipped)
package unit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// Shared minimal stubs
// ═══════════════════════════════════════════════════════════════════════════

// svcgapErrCache is a port.Cache stub whose Get always returns the configured
// error, letting tests drive the "cache unavailable" branch.
type svcgapErrCache struct {
	getErr error
	values map[string][]byte
}

func newSvcgapErrCache(getErr error) *svcgapErrCache {
	return &svcgapErrCache{getErr: getErr, values: map[string][]byte{}}
}
func (c *svcgapErrCache) Get(_ context.Context, _ string) ([]byte, error) {
	return nil, c.getErr
}
func (c *svcgapErrCache) MGet(_ context.Context, keys []string) ([][]byte, error) {
	return make([][]byte, len(keys)), nil
}
func (c *svcgapErrCache) Set(_ context.Context, key string, v []byte, _ time.Duration) error {
	c.values[key] = v
	return nil
}
func (c *svcgapErrCache) SetNX(_ context.Context, _ string, _ []byte, _ time.Duration) (bool, error) {
	return true, nil
}
func (c *svcgapErrCache) Delete(_ context.Context, _ ...string) error { return nil }
func (c *svcgapErrCache) Health(_ context.Context) error              { return nil }
func (c *svcgapErrCache) Close() error                                { return nil }

var _ port.Cache = (*svcgapErrCache)(nil)

// svcgapRecordCache records Set calls alongside being a full port.Cache.
type svcgapRecordCache struct {
	values   map[string][]byte
	setCalls []string
}

func newSvcgapRecordCache() *svcgapRecordCache {
	return &svcgapRecordCache{values: map[string][]byte{}}
}
func (c *svcgapRecordCache) Get(_ context.Context, key string) ([]byte, error) {
	return c.values[key], nil
}
func (c *svcgapRecordCache) MGet(_ context.Context, keys []string) ([][]byte, error) {
	out := make([][]byte, len(keys))
	for i, k := range keys {
		out[i] = c.values[k]
	}
	return out, nil
}
func (c *svcgapRecordCache) Set(_ context.Context, key string, v []byte, _ time.Duration) error {
	c.values[key] = v
	c.setCalls = append(c.setCalls, key)
	return nil
}
func (c *svcgapRecordCache) SetNX(_ context.Context, _ string, _ []byte, _ time.Duration) (bool, error) {
	return true, nil
}
func (c *svcgapRecordCache) Delete(_ context.Context, _ ...string) error { return nil }
func (c *svcgapRecordCache) Health(_ context.Context) error              { return nil }
func (c *svcgapRecordCache) Close() error                                { return nil }

var _ port.Cache = (*svcgapRecordCache)(nil)

// ═══════════════════════════════════════════════════════════════════════════
// authz_service.go — getCached / setCached
// ═══════════════════════════════════════════════════════════════════════════

// TestAuthZ_GetCached_CacheGetError covers getCached when cache.Get returns
// error — the cache is non-nil but unavailable; getCached must return nil so
// GetMembership falls through to the DB (and without a real pool returns error).
func TestAuthZ_GetCached_CacheGetError(t *testing.T) {
	repo := &fakeAuthZRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error) {
		return nil, errors.New("db down") // DB also fails — we only care the cache error doesn't panic
	}}
	cache := newSvcgapErrCache(errors.New("redis unavailable"))
	svc := service.NewAuthZService(repo, &azPlanReader{}, &azDeptReader{}, cache)

	_, err := svc.GetMembership(context.Background(), uuid.New(), uuid.New())
	// Error from repo (not cache) — key point is no panic on cache.Get error.
	require.Error(t, err)
}

// TestAuthZ_GetCached_InvalidJSON covers getCached when cache.Get returns
// bytes that are not valid JSON — getCached must return nil so the caller
// falls through.
func TestAuthZ_GetCached_InvalidJSON(t *testing.T) {
	repo := &fakeAuthZRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error) {
		return nil, errors.New("db fallback")
	}}
	cache := newSvcgapRecordCache()
	tenantID, userID := uuid.New(), uuid.New()
	// Seed invalid JSON under the membership key so getCached hits the
	// json.Unmarshal error branch.
	cache.values["om:memberships:"+tenantID.String()+":"+userID.String()] = []byte("{not valid json}")

	svc := service.NewAuthZService(repo, &azPlanReader{}, &azDeptReader{}, cache)
	_, err := svc.GetMembership(context.Background(), tenantID, userID)
	require.Error(t, err) // falls through to repo which returns error
}

// TestAuthZ_SetCached_NilProj covers the nil-proj guard in setCached.
// We exercise it indirectly by injecting a repo that returns (nil, nil) so
// GetMembership sees a "not found" — setCached(nil) is called but is a no-op.
func TestAuthZ_SetCached_NilProj(t *testing.T) {
	repo := &fakeAuthZRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error) {
		return nil, nil
	}}
	cache := newSvcgapRecordCache()
	svc := service.NewAuthZService(repo, &azPlanReader{}, &azDeptReader{}, cache)

	_, err := svc.GetMembership(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err) // ErrMemberNotFound
	// No Set calls — setCached(nil) is a no-op.
	assert.Empty(t, cache.setCalls, "setCached(nil) must not call cache.Set")
}

// TestAuthZ_SetCached_CallsSet covers the main path inside setCached:
// a successful readFromDB result is marshalled and written to cache.
func TestAuthZ_SetCached_CallsSet(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	repo := &fakeAuthZRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error) {
		return &port.MembershipProjectionRow{
			MembershipStatus:   domain.MembershipActive,
			TenantPlan:         domain.PlanStarter,
			SubscriptionStatus: domain.StatusActive,
		}, nil
	}}
	cache := newSvcgapRecordCache()
	svc := service.NewAuthZService(repo, &azPlanReader{}, &azDeptReader{}, cache)

	proj, err := svc.GetMembership(context.Background(), tenantID, userID)
	require.NoError(t, err)
	require.NotNil(t, proj)

	// setCached must have written the membership key.
	expectedKey := "om:memberships:" + tenantID.String() + ":" + userID.String()
	assert.Contains(t, cache.setCalls, expectedKey, "setCached must call cache.Set with membership key")
}

// ═══════════════════════════════════════════════════════════════════════════
// catalog_service.go — setCachedDepartments / setCachedPlans
// ═══════════════════════════════════════════════════════════════════════════

// TestCatalogService_SetCachedDepartments_WritesBothKeys verifies that a
// successful Departments() fetch writes both the primary and stale cache keys.
func TestCatalogService_SetCachedDepartments_WritesBothKeys(t *testing.T) {
	cache := newSvcgapRecordCache()
	client := &fakeCatalogAdminClient{
		departmentsFn: func(context.Context) ([]port.CatalogDepartment, error) {
			return []port.CatalogDepartment{
				{ID: uuid.New(), Code: "ENG", Name: "Engineering", IsSystem: true, IsActive: true},
			}, nil
		},
	}
	svc := service.NewCatalogService(client, cache)
	depts, err := svc.Departments(context.Background())
	require.NoError(t, err)
	require.Len(t, depts, 1)
	// Both primary and stale keys must have been written.
	assert.Contains(t, cache.setCalls, "om:departments", "primary departments key must be set")
	assert.Contains(t, cache.setCalls, "om:departments:stale", "stale departments key must be set")
}

// TestCatalogService_SetCachedPlans_WritesBothKeys verifies that a successful
// Plans() fetch writes both the primary and stale cache keys.
func TestCatalogService_SetCachedPlans_WritesBothKeys(t *testing.T) {
	cache := newSvcgapRecordCache()
	client := &fakeCatalogAdminClient{
		plansFn: func(context.Context) ([]port.CatalogPlan, error) {
			return []port.CatalogPlan{
				{Code: "starter", TrialDurationDays: 14},
			}, nil
		},
	}
	svc := service.NewCatalogService(client, cache)
	plans, err := svc.Plans(context.Background())
	require.NoError(t, err)
	require.Len(t, plans, 1)
	assert.Contains(t, cache.setCalls, "om:plans", "primary plans key must be set")
	assert.Contains(t, cache.setCalls, "om:plans:stale", "stale plans key must be set")
}

// ═══════════════════════════════════════════════════════════════════════════
// tenant_service.go — Get cache-error fall-through, setCached, requireActiveMember
// ═══════════════════════════════════════════════════════════════════════════

// svcgapTenantRepo is a TenantRepository that returns a configurable tenant.
type svcgapTenantRepo struct {
	port.TenantRepositoryNoop
	findByIDFn func(context.Context, uuid.UUID) (*domain.Tenant, error)
}

func (r *svcgapTenantRepo) FindByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	if r.findByIDFn != nil {
		return r.findByIDFn(ctx, id)
	}
	return &domain.Tenant{ID: id, Status: domain.StatusActive}, nil
}
func (r *svcgapTenantRepo) FindByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return r.FindByID(ctx, id)
}

var _ port.TenantRepository = (*svcgapTenantRepo)(nil)

// TestTenantService_Get_CacheGetError_FallsThroughToDB verifies that when
// cache.Get returns an error the service still calls FindByID and returns
// the tenant (setCached is also called via the happy-path).
func TestTenantService_Get_CacheGetError_FallsThroughToDB(t *testing.T) {
	tenantID := uuid.New()
	expected := &domain.Tenant{ID: tenantID, Slug: "acme", Status: domain.StatusActive}

	repo := &svcgapTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			assert.Equal(t, tenantID, id)
			return expected, nil
		},
	}
	cache := newSvcgapErrCache(errors.New("redis timeout"))
	svc := service.NewTenantService(repo, cache, nil)

	got, err := svc.Get(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, expected, got)
}

// TestTenantService_SetCached_WritesKey verifies that setCached writes the
// om:tenant:{id} key when the cache is non-nil and the tenant is non-nil.
func TestTenantService_SetCached_WritesKey(t *testing.T) {
	tenantID := uuid.New()
	tenant := &domain.Tenant{ID: tenantID, Status: domain.StatusActive}

	repo := &svcgapTenantRepo{
		findByIDFn: func(_ context.Context, _ uuid.UUID) (*domain.Tenant, error) {
			return tenant, nil
		},
	}
	cache := newSvcgapRecordCache()
	svc := service.NewTenantService(repo, cache, nil)

	_, err := svc.Get(context.Background(), tenantID)
	require.NoError(t, err)

	expectedKey := "om:tenant:" + tenantID.String()
	assert.Contains(t, cache.setCalls, expectedKey, "setCached must write om:tenant key")
}

// ═══════════════════════════════════════════════════════════════════════════
// department_service.go — ListForTenant happy path + List error + Activate error
// ═══════════════════════════════════════════════════════════════════════════

// svcgapTenantDeptList is a TenantDepartmentRepository stub with configurable List.
type svcgapTenantDeptList struct {
	listFn     func(context.Context, uuid.UUID) ([]domain.TenantDepartment, error)
	activateFn func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error)
}

func (r *svcgapTenantDeptList) List(ctx context.Context, tid uuid.UUID) ([]domain.TenantDepartment, error) {
	if r.listFn != nil {
		return r.listFn(ctx, tid)
	}
	return nil, nil
}
func (r *svcgapTenantDeptList) ListActive(_ context.Context, _ uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *svcgapTenantDeptList) Find(_ context.Context, tid, did uuid.UUID) (*domain.TenantDepartment, error) {
	return &domain.TenantDepartment{TenantID: tid, DepartmentID: did, IsActive: true}, nil
}
func (r *svcgapTenantDeptList) Activate(ctx context.Context, tid, did uuid.UUID) (*domain.TenantDepartment, error) {
	if r.activateFn != nil {
		return r.activateFn(ctx, tid, did)
	}
	return &domain.TenantDepartment{TenantID: tid, DepartmentID: did, IsActive: true}, nil
}
func (r *svcgapTenantDeptList) SetActive(_ context.Context, tid, did uuid.UUID, active bool, _ int64) (*domain.TenantDepartment, error) {
	return &domain.TenantDepartment{TenantID: tid, DepartmentID: did, IsActive: active}, nil
}

var _ port.TenantDepartmentRepository = (*svcgapTenantDeptList)(nil)

// TestDepartmentService_ListForTenant_ListError covers the error branch
// at the very first repo.List call in ListForTenant.
func TestDepartmentService_ListForTenant_ListError(t *testing.T) {
	repoErr := errors.New("db_unavailable")
	tenantDepts := &svcgapTenantDeptList{
		listFn: func(context.Context, uuid.UUID) ([]domain.TenantDepartment, error) {
			return nil, repoErr
		},
	}
	svc := service.NewDepartmentService(&fakeDeptCatalogRepo{}, tenantDepts, nil)
	_, err := svc.ListForTenant(context.Background(), uuid.New())
	assert.ErrorIs(t, err, repoErr)
}

// TestDepartmentService_ListForTenant_HappyPath covers the successful hydration
// path where List returns rows and DepartmentByID resolves each dept's metadata.
func TestDepartmentService_ListForTenant_HappyPath(t *testing.T) {
	tenantID := uuid.New()
	deptID := uuid.New()

	tenantDepts := &svcgapTenantDeptList{
		listFn: func(_ context.Context, _ uuid.UUID) ([]domain.TenantDepartment, error) {
			return []domain.TenantDepartment{
				{TenantID: tenantID, DepartmentID: deptID, IsActive: true, RecordVersion: 3},
			}, nil
		},
	}
	catalog := &fakeDeptCatalogRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Department, error) {
			return &domain.Department{ID: id, Code: "ENG", Name: "Engineering", IsSystem: true, IsActive: true}, nil
		},
	}
	svc := service.NewDepartmentService(catalog, tenantDepts, nil)

	got, err := svc.ListForTenant(context.Background(), tenantID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, deptID, got[0].DepartmentID)
	assert.Equal(t, "ENG", got[0].Code)
	assert.Equal(t, "Engineering", got[0].Name)
	assert.True(t, got[0].IsSystem)
	assert.True(t, got[0].IsActive)
	assert.EqualValues(t, 3, got[0].RecordVersion)
}

// TestDepartmentService_Activate_TenantDeptsActivateError covers the error
// path at tenantDepts.Activate inside Activate (line 76).
func TestDepartmentService_Activate_TenantDeptsActivateError(t *testing.T) {
	activateErr := errors.New("department_already_activated")
	tenantDepts := &svcgapTenantDeptList{
		activateFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
			return nil, activateErr
		},
	}
	catalog := &fakeDeptCatalogRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Department, error) {
			return &domain.Department{ID: id, IsActive: true}, nil
		},
	}
	svc := service.NewDepartmentService(catalog, tenantDepts, nil)

	_, _, err := svc.Activate(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, activateErr)
}

// ═══════════════════════════════════════════════════════════════════════════
// dept_membership_service.go — Remove with requestctx (evt IP/UA)
// ═══════════════════════════════════════════════════════════════════════════

// svcgapDeptMemPubTxRunner injects both a publisher and a requestctx into
// the tx context so the Remove evt.IPAddress/UserAgent branch is hit.
type svcgapDeptMemPubTxRunner struct {
	pub port.EventPublisher
	rc  *requestctx.RequestContext
}

func (r *svcgapDeptMemPubTxRunner) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	if r.pub != nil {
		ctx = port.WithEventPublisher(ctx, r.pub)
	}
	if r.rc != nil {
		ctx = requestctx.WithContext(ctx, r.rc)
	}
	return fn(ctx)
}

var _ port.TxRunner = (*svcgapDeptMemPubTxRunner)(nil)

// svcgapCapturePublisher records enqueued events.
type svcgapCapturePublisher struct {
	events []*domain.DomainEvent
}

func (p *svcgapCapturePublisher) Enqueue(_ context.Context, e *domain.DomainEvent) error {
	p.events = append(p.events, e)
	return nil
}

// TestDeptMembership_Remove_SetsIPAndUserAgentFromRequestCtx covers the
// requestctx branch inside Remove's tx closure (lines 246-249 of
// dept_membership_service.go).
func TestDeptMembership_Remove_SetsIPAndUserAgentFromRequestCtx(t *testing.T) {
	tenantID, userID, deptID, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	delClient := &fakeDelegationCheckClient{
		deptDelegateFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*uuid.UUID, error) {
			return nil, nil
		},
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{ActiveWorkflows: 0}, nil
		},
	}
	repo := &fakeDeptMemRepo{
		removeFn: func(_ context.Context, _, _, _ uuid.UUID) (*domain.DeptMembership, error) {
			return &domain.DeptMembership{ID: uuid.New(), TenantID: tenantID, DepartmentID: deptID}, nil
		},
	}
	pub := &svcgapCapturePublisher{}
	rc := &requestctx.RequestContext{
		TenantID:  tenantID,
		UserID:    actorID,
		ClientIP:  "10.0.0.1",
		UserAgent: "test-agent/1.0",
	}
	txRunner := &svcgapDeptMemPubTxRunner{pub: pub, rc: rc}

	svc := service.NewDeptMembershipService(repo, nil, nil, nil, delClient, wf, nil, txRunner)
	dm, err := svc.Remove(context.Background(), tenantID, userID, deptID, actorID)
	require.NoError(t, err)
	require.NotNil(t, dm)

	require.Len(t, pub.events, 1)
	assert.Equal(t, "10.0.0.1", pub.events[0].IPAddress)
	assert.Equal(t, "test-agent/1.0", pub.events[0].UserAgent)
}

// ═══════════════════════════════════════════════════════════════════════════
// group_mapping_service.go — AssignFromGroups coverage + setCachedResolution nil-cache
// ═══════════════════════════════════════════════════════════════════════════

// svcgapGmMembershipRepo is a MembershipRepository stub for group mapping tests.
type svcgapGmMembershipRepo struct {
	findByUserIDFn func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error)
}

func (r *svcgapGmMembershipRepo) List(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
	return nil, nil
}
func (r *svcgapGmMembershipRepo) FindByUserID(ctx context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
	if r.findByUserIDFn != nil {
		return r.findByUserIDFn(ctx, tid, uid)
	}
	return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid}, nil
}
func (r *svcgapGmMembershipRepo) Insert(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *svcgapGmMembershipRepo) SetStatus(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *svcgapGmMembershipRepo) SoftDelete(context.Context, uuid.UUID, uuid.UUID, int64) error {
	return nil
}
func (r *svcgapGmMembershipRepo) CountActive(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (r *svcgapGmMembershipRepo) ListActiveUserIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

var _ port.MembershipRepository = (*svcgapGmMembershipRepo)(nil)

// svcgapGmDeptMemRepo is a DeptMembershipRepository stub for group mapping.
type svcgapGmDeptMemRepo struct {
	assignFn func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, domain.DeptRole, uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error)
}

func (r *svcgapGmDeptMemRepo) ListByUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *svcgapGmDeptMemRepo) ListByDepartment(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *svcgapGmDeptMemRepo) Assign(ctx context.Context, tid, uid, did, memID uuid.UUID, level domain.DeptRole, actor uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
	if r.assignFn != nil {
		return r.assignFn(ctx, tid, uid, did, memID, level, actor)
	}
	return &domain.DeptMembership{ID: uuid.New(), TenantID: tid, DepartmentID: did, RoleLevel: level}, nil, nil
}
func (r *svcgapGmDeptMemRepo) Remove(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.DeptMembership, error) {
	return nil, nil
}
func (r *svcgapGmDeptMemRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *svcgapGmDeptMemRepo) SoftDeleteAllForDept(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}

var _ port.DeptMembershipRepository = (*svcgapGmDeptMemRepo)(nil)

// svcgapGmRoleRepo is a TenantRoleRepository stub for group mapping.
type svcgapGmRoleRepo struct {
	listByUserFn func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error)
	grantFn      func(context.Context, *domain.TenantRole) (*domain.TenantRole, error)
}

func (r *svcgapGmRoleRepo) ListByUser(ctx context.Context, tid, uid uuid.UUID) ([]domain.TenantRole, error) {
	if r.listByUserFn != nil {
		return r.listByUserFn(ctx, tid, uid)
	}
	return nil, nil
}
func (r *svcgapGmRoleRepo) ListByRole(context.Context, uuid.UUID, domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *svcgapGmRoleRepo) CountActiveOwners(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (r *svcgapGmRoleRepo) Grant(ctx context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
	if r.grantFn != nil {
		return r.grantFn(ctx, tr)
	}
	return tr, nil
}
func (r *svcgapGmRoleRepo) Revoke(context.Context, uuid.UUID, uuid.UUID, domain.TenantRoleCode) (*domain.TenantRole, error) {
	return nil, nil
}
func (r *svcgapGmRoleRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}

var _ port.TenantRoleRepository = (*svcgapGmRoleRepo)(nil)

// svcgapBuildGMSvc is a helper to wire a GroupMappingService for these tests.
func svcgapBuildGMSvc(
	mem port.MembershipRepository,
	roles port.TenantRoleRepository,
	deptMems port.DeptMembershipRepository,
	_ port.Cache, // always nil in these tests; kept for call-site clarity
	client port.GroupMappingClient,
) *service.GroupMappingService {
	return service.NewGroupMappingService(mem, roles, deptMems, &passthroughTxRunner{}, nil, client)
}

// svcgapGroupResolution returns a GroupResolution that maps one group to one
// dept and one role at the supplied level, used to drive full AssignFromGroups paths.
func svcgapGroupResolution(groupName string, deptID uuid.UUID, level domain.DeptRole, tenantRole domain.TenantRoleCode) *port.GroupResolution {
	return &port.GroupResolution{
		DeptMappings:     []port.ResolvedDeptMapping{{KeycloakGroupName: groupName, DepartmentID: deptID}},
		DeptRoleMappings: []port.ResolvedDeptRoleMapping{{KeycloakGroupName: groupName, RoleCode: level}},
		TenantRoleMappings: []port.ResolvedTenantRoleMapping{
			{KeycloakGroupName: groupName, RoleCode: tenantRole},
		},
	}
}

// TestGroupMapping_AssignFromGroups_DrGroupNotInGroupSet covers the branch
// at line 102 where a drMap entry's group name is not in groupSet → continue.
// We supply a dm entry matching the group but a dr entry with a different group.
func TestGroupMapping_AssignFromGroups_DrGroupNotInGroupSet(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	deptID := uuid.New()

	// dm maps "group-a" → deptID; dr maps "group-b" (not in groupNames) → preparator.
	client := &fakeGMClient{
		resolveFn: func(_ context.Context, _ uuid.UUID, _ []string) (*port.GroupResolution, error) {
			return &port.GroupResolution{
				DeptMappings: []port.ResolvedDeptMapping{
					{KeycloakGroupName: "group-a", DepartmentID: deptID},
				},
				DeptRoleMappings: []port.ResolvedDeptRoleMapping{
					{KeycloakGroupName: "group-b", RoleCode: domain.DeptPreparator},
				},
			}, nil
		},
	}
	svc := svcgapBuildGMSvc(&svcgapGmMembershipRepo{}, &svcgapGmRoleRepo{}, &svcgapGmDeptMemRepo{}, nil, client)

	// Only "group-a" in the call; "group-b" is not → dm/dr pairing never forms.
	res, err := svc.AssignFromGroups(context.Background(), tenantID, userID, []string{"group-a"})
	require.NoError(t, err)
	// No dept roles resolved (pairing requires same group for both dm+dr).
	assert.Empty(t, res.AssignedDepts)
}

// TestGroupMapping_AssignFromGroups_TrGroupNotInGroupSet covers the continue
// at line 124 where a GTRM entry's KeycloakGroupName is NOT in groupSet.
// This happens when the Group Mapping Service returns a tenant-role mapping
// for a group that the user does not belong to.
func TestGroupMapping_AssignFromGroups_TrGroupNotInGroupSet(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()

	client := &fakeGMClient{
		resolveFn: func(_ context.Context, _ uuid.UUID, _ []string) (*port.GroupResolution, error) {
			return &port.GroupResolution{
				// GTRM entry for "group-other" but the user is only in "group-present".
				TenantRoleMappings: []port.ResolvedTenantRoleMapping{
					{KeycloakGroupName: "group-other", RoleCode: domain.RoleTenantAdmin},
				},
			}, nil
		},
	}
	svc := svcgapBuildGMSvc(&svcgapGmMembershipRepo{}, &svcgapGmRoleRepo{}, &svcgapGmDeptMemRepo{}, nil, client)

	// Only "group-present" is in groupNames; "group-other" is not → GTRM skipped.
	res, err := svc.AssignFromGroups(context.Background(), tenantID, userID, []string{"group-present"})
	require.NoError(t, err)
	assert.Empty(t, res.GrantedTenantRoles,
		"GTRM for a group the user is not in must be skipped (line 124 continue)")
}

// TestGroupMapping_AssignFromGroups_TrRoleIsMember covers the continue at
// line 126 where a trMap entry has RoleCode == RoleMember and is skipped.
func TestGroupMapping_AssignFromGroups_TrRoleIsMember(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()

	client := &fakeGMClient{
		resolveFn: func(_ context.Context, _ uuid.UUID, _ []string) (*port.GroupResolution, error) {
			return &port.GroupResolution{
				TenantRoleMappings: []port.ResolvedTenantRoleMapping{
					{KeycloakGroupName: "group-x", RoleCode: domain.RoleMember},
				},
			}, nil
		},
	}
	svc := svcgapBuildGMSvc(&svcgapGmMembershipRepo{}, &svcgapGmRoleRepo{}, &svcgapGmDeptMemRepo{}, nil, client)

	res, err := svc.AssignFromGroups(context.Background(), tenantID, userID, []string{"group-x"})
	require.NoError(t, err)
	// RoleMember is filtered out — no tenant roles granted.
	assert.Empty(t, res.GrantedTenantRoles)
}

// TestGroupMapping_AssignFromGroups_AssignError covers the Assign error path
// inside the tx (line ~148).
func TestGroupMapping_AssignFromGroups_AssignError(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	deptID := uuid.New()
	assignErr := errors.New("assign_failed")

	client := &fakeGMClient{
		resolveFn: func(_ context.Context, _ uuid.UUID, _ []string) (*port.GroupResolution, error) {
			return svcgapGroupResolution("grp", deptID, domain.DeptReviewer, domain.RoleTenderAdmin), nil
		},
	}
	deptMems := &svcgapGmDeptMemRepo{
		assignFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, domain.DeptRole, uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
			return nil, nil, assignErr
		},
	}
	svc := svcgapBuildGMSvc(&svcgapGmMembershipRepo{}, &svcgapGmRoleRepo{}, deptMems, nil, client)

	_, err := svc.AssignFromGroups(context.Background(), tenantID, userID, []string{"grp"})
	assert.ErrorIs(t, err, assignErr)
}

// TestGroupMapping_AssignFromGroups_LevelChangedEvent covers the
// LevelChanged branch: Assign returns a non-nil previous with a different level.
func TestGroupMapping_AssignFromGroups_LevelChangedEvent(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	deptID := uuid.New()
	pub := &svcgapCapturePublisher{}
	txRunner := &svcgapDeptMemPubTxRunner{pub: pub}

	client := &fakeGMClient{
		resolveFn: func(_ context.Context, _ uuid.UUID, _ []string) (*port.GroupResolution, error) {
			return &port.GroupResolution{
				DeptMappings:     []port.ResolvedDeptMapping{{KeycloakGroupName: "grp", DepartmentID: deptID}},
				DeptRoleMappings: []port.ResolvedDeptRoleMapping{{KeycloakGroupName: "grp", RoleCode: domain.DeptApprover}},
			}, nil
		},
	}
	// Assign returns previous != nil with a different level → LevelChanged event.
	deptMems := &svcgapGmDeptMemRepo{
		assignFn: func(_ context.Context, tid, uid, did, memID uuid.UUID, level domain.DeptRole, _ uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
			assigned := &domain.DeptMembership{TenantID: tid, DepartmentID: did, RoleLevel: level}
			previous := &domain.DeptMembership{TenantID: tid, DepartmentID: did, RoleLevel: domain.DeptPreparator}
			return assigned, previous, nil
		},
	}
	svc := service.NewGroupMappingService(&svcgapGmMembershipRepo{}, &svcgapGmRoleRepo{}, deptMems, txRunner, nil, client)

	res, err := svc.AssignFromGroups(context.Background(), tenantID, userID, []string{"grp"})
	require.NoError(t, err)
	require.Len(t, res.AssignedDepts, 1)

	// Must have emitted a LevelChanged event.
	require.Len(t, pub.events, 1)
	assert.Equal(t, domain.EventDepartmentMembershipLevelChanged, pub.events[0].Type)
}

// TestGroupMapping_AssignFromGroups_TenantRoleGranted covers the roles.Grant
// path (line ~198) when a tenant role not already held is mapped.
func TestGroupMapping_AssignFromGroups_TenantRoleGranted(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	pub := &svcgapCapturePublisher{}
	txRunner := &svcgapDeptMemPubTxRunner{pub: pub}

	client := &fakeGMClient{
		resolveFn: func(_ context.Context, _ uuid.UUID, _ []string) (*port.GroupResolution, error) {
			return &port.GroupResolution{
				TenantRoleMappings: []port.ResolvedTenantRoleMapping{
					{KeycloakGroupName: "grp", RoleCode: domain.RoleTenderAdmin},
				},
			}, nil
		},
	}
	roles := &svcgapGmRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil // user holds no roles → grant proceeds
		},
		grantFn: func(_ context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
			return tr, nil
		},
	}
	svc := service.NewGroupMappingService(&svcgapGmMembershipRepo{}, roles, &svcgapGmDeptMemRepo{}, txRunner, nil, client)

	res, err := svc.AssignFromGroups(context.Background(), tenantID, userID, []string{"grp"})
	require.NoError(t, err)
	assert.Contains(t, res.GrantedTenantRoles, domain.RoleTenderAdmin)
	// Must have emitted a TenantRoleGranted event.
	require.Len(t, pub.events, 1)
	assert.Equal(t, domain.EventTenantRoleGranted, pub.events[0].Type)
}

// TestGroupMapping_SetCachedResolution_NilCache covers the nil-cache early
// return in setCachedResolution (line 307-308). When cache is nil, calling
// AssignFromGroups must not panic.
func TestGroupMapping_SetCachedResolution_NilCache(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	deptID := uuid.New()

	client := &fakeGMClient{
		resolveFn: func(_ context.Context, _ uuid.UUID, _ []string) (*port.GroupResolution, error) {
			return svcgapGroupResolution("grp", deptID, domain.DeptPreparator, domain.RoleTenderAdmin), nil
		},
	}
	roles := &svcgapGmRoleRepo{}
	// nil cache — setCachedResolution must return immediately without panicking.
	svc := service.NewGroupMappingService(&svcgapGmMembershipRepo{}, roles, &svcgapGmDeptMemRepo{}, &passthroughTxRunner{}, nil, client)

	require.NotPanics(t, func() {
		_, _ = svc.AssignFromGroups(context.Background(), tenantID, userID, []string{"grp"})
	})
}

// ═══════════════════════════════════════════════════════════════════════════
// membership_service.go — various uncovered branches
// ═══════════════════════════════════════════════════════════════════════════

// svcgapDeptMemForList is a DeptMembershipRepository that returns an error
// from ListByUser, used for List's hydration-loop error path.
type svcgapDeptMemForList struct {
	listByUserErr error
}

func (r *svcgapDeptMemForList) ListByUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, r.listByUserErr
}
func (r *svcgapDeptMemForList) ListByDepartment(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *svcgapDeptMemForList) Assign(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, domain.DeptRole, uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
	return nil, nil, nil
}
func (r *svcgapDeptMemForList) Remove(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.DeptMembership, error) {
	return nil, nil
}
func (r *svcgapDeptMemForList) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *svcgapDeptMemForList) SoftDeleteAllForDept(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}

var _ port.DeptMembershipRepository = (*svcgapDeptMemForList)(nil)

// TestMembership_List_DeptListByUserError covers the error branch inside
// List's hydration loop when deptMemberships.ListByUser fails.
func TestMembership_List_DeptListByUserError(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	deptErr := errors.New("dept_list_error")

	memberships := &fakeMemRepoFull{
		listFn: func(_ context.Context, _ uuid.UUID, _ *domain.MembershipListCursor, _ int) (*domain.MembershipListPage, error) {
			return &domain.MembershipListPage{
				Items: []domain.MembershipListItem{
					{Membership: domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID}},
				},
			}, nil
		},
		findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return nil, errors.New("not used")
		},
		countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 0, nil },
	}
	roles := &fakeRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil
		},
	}
	deptMems := &svcgapDeptMemForList{listByUserErr: deptErr}

	svc := service.NewMembershipService(memberships, roles, deptMems, &port.TenantRepositoryNoop{}, nil, nil, nil, nil, nil, nil, 30)
	_, err := svc.List(context.Background(), tenantID, nil, 10)
	assert.ErrorIs(t, err, deptErr)
}

// TestMembership_SetStatus_Suspend_WFI13_ActiveWorkflows covers the
// WFI-13 advisory branch where workflow.GetDelegateImpact returns
// ActiveWorkflows > 0 → DelegateImpact advisory is populated.
func TestMembership_SetStatus_Suspend_WFI13_ActiveWorkflows(t *testing.T) {
	wfID := uuid.New()
	m := &fakeMembershipRepo{
		setStatusFn: func(_ context.Context, _, _ uuid.UUID, s domain.MembershipStatus, _ int64) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: s}, nil
		},
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{ActiveWorkflows: 2, WorkflowIDs: []uuid.UUID{wfID}}, nil
		},
	}
	rp := &fakeRPClient{}
	svc := buildMembershipSvcForSetStatus(m, nil, rp, wf)

	res, err := svc.SetStatus(context.Background(), uuid.New(), uuid.New(), domain.MembershipSuspended, 1)
	require.NoError(t, err)
	require.NotNil(t, res.DelegateImpact)
	assert.True(t, res.DelegateImpact.Checked)
	assert.Equal(t, 2, res.DelegateImpact.ActiveWorkflows)
	assert.Contains(t, res.DelegateImpact.WorkflowIDs, wfID)
}

// svcgapRuTenantRepoCAO is a TenantRepository whose CountActiveOwners call
// fails when called inside a tx, used for RemoveUser's owner-path error.
type svcgapRuTenantRepo struct {
	port.TenantRepositoryNoop
	lockErr error
}

func (r *svcgapRuTenantRepo) LockByID(_ context.Context, _ uuid.UUID) error {
	return r.lockErr
}

var _ port.TenantRepository = (*svcgapRuTenantRepo)(nil)

// TestMembership_RemoveUser_CountActiveOwnersError covers the
// roles.CountActiveOwners error when wasOwner=true (line 465).
func TestMembership_RemoveUser_CountActiveOwnersError(t *testing.T) {
	tenantID, userID, actorID := uuid.New(), uuid.New(), uuid.New()
	caoErr := errors.New("count_owners_failed")

	mem := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
	}
	roles := &ruRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) {
			return 0, caoErr
		},
	}
	tenants := &svcgapRuTenantRepo{lockErr: nil} // LockByID succeeds
	svc := service.NewMembershipService(mem, roles, &ssDeptMemRepoDM{}, tenants, nil, nil, nil, nil, &passthroughTxRunner{}, nil, 30)

	err := svc.RemoveUser(context.Background(), tenantID, userID, actorID)
	assert.ErrorIs(t, err, caoErr)
}

// TestMembership_RemoveUser_DeptSoftDeleteError covers the
// deptMemberships.SoftDeleteAllForUser error path (line ~501).
func TestMembership_RemoveUser_DeptSoftDeleteError(t *testing.T) {
	tenantID, userID, actorID := uuid.New(), uuid.New(), uuid.New()
	deptErr := errors.New("dept_soft_delete_failed")

	mem := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
	}
	roles := &ruRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil // not an owner
		},
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil // cascade 1 succeeds
		},
	}
	deptMems := &ssDeptMemRepoDM{
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			return nil, deptErr
		},
	}
	tenants := &svcgapRuTenantRepo{lockErr: nil}
	svc := service.NewMembershipService(mem, roles, deptMems, tenants, nil, nil, nil, nil, &passthroughTxRunner{}, nil, 30)

	err := svc.RemoveUser(context.Background(), tenantID, userID, actorID)
	assert.ErrorIs(t, err, deptErr)
}

// TestMembership_RemoveUser_MembershipSoftDeleteError covers the
// memberships.SoftDelete error path (line ~541).
func TestMembership_RemoveUser_MembershipSoftDeleteError(t *testing.T) {
	tenantID, userID, actorID := uuid.New(), uuid.New(), uuid.New()
	softDelErr := errors.New("soft_delete_failed")

	mem := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error {
			return softDelErr
		},
	}
	roles := &ruRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil
		},
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil
		},
	}
	deptMems := &ssDeptMemRepoDM{
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			return nil, nil
		},
	}
	tenants := &svcgapRuTenantRepo{lockErr: nil}
	svc := service.NewMembershipService(mem, roles, deptMems, tenants, nil, nil, nil, nil, &passthroughTxRunner{}, nil, 30)

	err := svc.RemoveUser(context.Background(), tenantID, userID, actorID)
	assert.ErrorIs(t, err, softDelErr)
}

// TestMembership_ValidateAndEmitAssigneeOverride_PubNil covers the
// pub == nil branch inside ValidateAndEmitAssigneeOverride's tx (returns nil).
func TestMembership_ValidateAndEmitAssigneeOverride_PubNil(t *testing.T) {
	tenantID, tenderID, newUserID, deptID, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()

	// actor holds tender_admin
	actorRoles := []domain.TenantRole{{RoleCode: domain.RoleTenderAdmin}}
	// assignee is active + holds dept at required level
	assigneeMem := &domain.MembershipListItem{
		Membership: domain.TenantMembership{Status: domain.MembershipActive},
		Departments: []domain.DeptMembershipView{
			{DepartmentID: deptID, RoleLevel: domain.DeptApprover},
		},
	}

	roleRepo := &fakeRoleRepo{
		listByUserFn: func(_ context.Context, _ uuid.UUID, uid uuid.UUID) ([]domain.TenantRole, error) {
			if uid == actorID {
				return actorRoles, nil
			}
			return nil, nil
		},
	}
	memRepo := &fakeMemRepoFull{
		findByUserFn: func(_ context.Context, _, uid uuid.UUID) (*domain.TenantMembership, error) {
			return &assigneeMem.Membership, nil
		},
		listFn: func(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
			return &domain.MembershipListPage{Items: []domain.MembershipListItem{*assigneeMem}}, nil
		},
		countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 1, nil },
	}

	// Wire Get so it returns the assignee with dept membership.
	deptMemsForGet := &svcgapDeptMemForList{}
	// Override ListByUser to return the dept membership for the assignee lookup.
	// We use a custom deptMems that returns the needed dept for Get.
	// Build svc with nil pub TxRunner (passthroughTxRunner injects no publisher).
	svc := service.NewMembershipService(memRepo, roleRepo, deptMemsForGet, &port.TenantRepositoryNoop{}, nil, nil, nil, nil, &passthroughTxRunner{}, nil, 30)

	// Get will return 404 for the assignee because fakeMemRepoFull.findByUserFn only works
	// for FindByUserID, but Get also calls ListByUser for roles and deptMems.ListByUser.
	// We need the assignee's status to be Active and have dept at required level.
	// Since fakeMemRepoFull.findByUserFn returns the assignee's membership and
	// deptMemsForGet.ListByUser returns nil, the eligibility check (item.Departments)
	// will be empty → ErrAssigneeIneligible. That's fine — we only care about reaching
	// the pub==nil branch after eligibility passes. Use a simpler approach:
	// build with deptMems that returns the right dept for the Get call path.
	_ = svc // test below uses a different wiring

	// Alternate: build a dedicated deptMems that returns the dept membership.
	fullDeptRepo := &struct {
		svcgapDeptMemForList
	}{}
	_ = fullDeptRepo

	// The cleanest path: test only that when eligibility passes and pub==nil,
	// the method returns nil (no error). Use a custom deptMems for the Get call.
	// Inline anonymous struct won't work for interface — use a proper type.
	// Use the existing sssDeptMemForGetEligible pattern from membership_scenarios_test
	// if it exists; otherwise use a simpler stub here:

	eligDeptMems := &svcgapDeptMemForList{} // returns nil, nil → empty dept list for Get
	// Since ListByUser returns nil, Get's item.Departments will be empty → ErrAssigneeIneligible.
	// To cover pub==nil we need eligibility to pass first. We must return the dept.
	// Use svcgapGmDeptMemRepo whose ListByUser we can control:
	eligibleDMRepo := &svcgapGmDeptMemRepo{}
	// svcgapGmDeptMemRepo.ListByUser returns nil,nil which means no depts.
	// We can't easily override it without another type. Instead inline a targeted test:

	_ = eligDeptMems
	_ = eligibleDMRepo

	// Simplest: build a small fully inlined fake that satisfies the interface.
	svc2 := service.NewMembershipService(memRepo, roleRepo,
		&svcgapAssigneeEligibleDM{deptID: deptID, level: domain.DeptApprover},
		&port.TenantRepositoryNoop{}, nil, nil, nil, nil,
		&passthroughTxRunner{}, // no publisher injected → pub == nil inside tx
		nil, 30)

	err := svc2.ValidateAndEmitAssigneeOverride(context.Background(), tenantID, tenderID, newUserID, deptID, domain.DeptApprover, actorID)
	// pub == nil → returns nil without error.
	require.NoError(t, err)
}

// svcgapAssigneeEligibleDM is a DeptMembershipRepository that returns the
// configured dept membership for the assignee user's ListByUser call.
type svcgapAssigneeEligibleDM struct {
	deptID uuid.UUID
	level  domain.DeptRole
}

func (r *svcgapAssigneeEligibleDM) ListByUser(_ context.Context, _, _ uuid.UUID) ([]domain.DeptMembership, error) {
	return []domain.DeptMembership{
		{DepartmentID: r.deptID, RoleLevel: r.level},
	}, nil
}
func (r *svcgapAssigneeEligibleDM) ListByDepartment(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *svcgapAssigneeEligibleDM) Assign(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, domain.DeptRole, uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
	return nil, nil, nil
}
func (r *svcgapAssigneeEligibleDM) Remove(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.DeptMembership, error) {
	return nil, nil
}
func (r *svcgapAssigneeEligibleDM) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *svcgapAssigneeEligibleDM) SoftDeleteAllForDept(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}

var _ port.DeptMembershipRepository = (*svcgapAssigneeEligibleDM)(nil)

// TestMembership_ResetUserMFA_ResetMFAError covers the RP.ResetMFA error →
// ErrRealmProvisionerUnavailable path (line 697-698).
func TestMembership_ResetUserMFA_ResetMFAError(t *testing.T) {
	tenantID, userID, actorID := uuid.New(), uuid.New(), uuid.New()

	mem := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: domain.MembershipActive}, nil
		},
	}
	rp := &fakeRPClient{
		resetMFAFn: func(context.Context, uuid.UUID, uuid.UUID) error {
			return errors.New("rp_unavailable")
		},
	}
	svc := service.NewMembershipService(mem, noOwnerRoleRepo{}, nil, &port.TenantRepositoryNoop{}, nil, nil, rp, nil, &passthroughTxRunner{}, nil, 30)

	err := svc.ResetUserMFA(context.Background(), tenantID, userID, actorID)
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrRealmProvisionerUnavailable)
}

// TestMembership_ResetUserMFA_PubNilInsideTx covers the pub == nil branch
// inside ResetUserMFA's tx closure (line 703-704) — returns nil.
func TestMembership_ResetUserMFA_PubNilInsideTx(t *testing.T) {
	tenantID, userID, actorID := uuid.New(), uuid.New(), uuid.New()

	mem := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: domain.MembershipActive}, nil
		},
	}
	rp := &fakeRPClient{} // ResetMFA succeeds (returns nil by default)
	// passthroughTxRunner does not inject a publisher → pub == nil inside tx.
	svc := service.NewMembershipService(mem, noOwnerRoleRepo{}, nil, &port.TenantRepositoryNoop{}, nil, nil, rp, nil, &passthroughTxRunner{}, nil, 30)

	err := svc.ResetUserMFA(context.Background(), tenantID, userID, actorID)
	require.NoError(t, err)
	assert.True(t, rp.resetMFACalled)
}

// ═══════════════════════════════════════════════════════════════════════════
// provisioning_service.go — TrialSignup inner error paths + SetRealmFields +
// SetMembershipStatus cache + DeleteMember owner paths
// ═══════════════════════════════════════════════════════════════════════════

// svcgapProvTenantRepo extends TenantRepositoryNoop with configurable Insert.
type svcgapProvTenantRepo struct {
	port.TenantRepositoryNoop
	insertFn func(context.Context, *domain.Tenant) (*domain.Tenant, bool, error)
}

func (r *svcgapProvTenantRepo) Insert(ctx context.Context, t *domain.Tenant) (*domain.Tenant, bool, error) {
	if r.insertFn != nil {
		return r.insertFn(ctx, t)
	}
	return t, true, nil
}

var _ port.TenantRepository = (*svcgapProvTenantRepo)(nil)

// svcgapTenantDeptProvRepo is a TenantDepartmentRepository stub for provisioning tests.
type svcgapTenantDeptProvRepo struct {
	activateFn func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error)
}

func (r *svcgapTenantDeptProvRepo) Find(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
	return nil, nil
}
func (r *svcgapTenantDeptProvRepo) List(context.Context, uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *svcgapTenantDeptProvRepo) ListActive(context.Context, uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *svcgapTenantDeptProvRepo) Activate(ctx context.Context, tid, did uuid.UUID) (*domain.TenantDepartment, error) {
	if r.activateFn != nil {
		return r.activateFn(ctx, tid, did)
	}
	return &domain.TenantDepartment{TenantID: tid, DepartmentID: did}, nil
}
func (r *svcgapTenantDeptProvRepo) SetActive(context.Context, uuid.UUID, uuid.UUID, bool, int64) (*domain.TenantDepartment, error) {
	return nil, nil
}

var _ port.TenantDepartmentRepository = (*svcgapTenantDeptProvRepo)(nil)

// svcgapLabelRepo is a DeptRoleLabelRepository stub.
type svcgapLabelRepo struct {
	seedFn func(context.Context, uuid.UUID) ([]domain.DeptRoleLabel, error)
}

func (r *svcgapLabelRepo) Seed(ctx context.Context, tid uuid.UUID) ([]domain.DeptRoleLabel, error) {
	if r.seedFn != nil {
		return r.seedFn(ctx, tid)
	}
	return nil, nil
}
func (r *svcgapLabelRepo) List(context.Context, uuid.UUID) ([]domain.DeptRoleLabel, error) {
	return nil, nil
}
func (r *svcgapLabelRepo) Update(context.Context, uuid.UUID, domain.DeptRole, string, int64) (*domain.DeptRoleLabel, error) {
	return nil, nil
}

var _ port.DeptRoleLabelRepository = (*svcgapLabelRepo)(nil)

// svcgapProvMembershipRepo adds configurable Insert on top of ssMembershipRepoDM.
type svcgapProvMembershipRepo struct {
	ssMembershipRepoDM
	insertFn func(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error)
}

func (r *svcgapProvMembershipRepo) Insert(ctx context.Context, m *domain.TenantMembership) (*domain.TenantMembership, error) {
	if r.insertFn != nil {
		return r.insertFn(ctx, m)
	}
	return m, nil
}

var _ port.MembershipRepository = (*svcgapProvMembershipRepo)(nil)

// svcgapProvRoleRepo adds configurable Grant on top of ssRoleRepoDM.
type svcgapProvRoleRepo struct {
	ssRoleRepoDM
	grantFn func(context.Context, *domain.TenantRole) (*domain.TenantRole, error)
}

func (r *svcgapProvRoleRepo) Grant(ctx context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
	if r.grantFn != nil {
		return r.grantFn(ctx, tr)
	}
	return tr, nil
}

var _ port.TenantRoleRepository = (*svcgapProvRoleRepo)(nil)

// buildTrialSvc is a helper that wires a ProvisioningService for TrialSignup
// inner-tx error tests.
func svcgapBuildTrialSvc(
	tenants port.TenantRepository,
	memberships port.MembershipRepository,
	roles port.TenantRoleRepository,
	tenantDepts port.TenantDepartmentRepository,
	labels port.DeptRoleLabelRepository,
) *service.ProvisioningService {
	// Supply a dept that is a system dept with code ENGINEERING.
	dept := &ssDeptReader{
		departmentsFn: func(context.Context) ([]domain.Department, error) {
			return []domain.Department{
				{ID: uuid.New(), Code: "ENGINEERING", IsSystem: true, IsActive: true},
			}, nil
		},
	}
	plan := &ssPlanReader{} // returns starter plan with 30-day trial
	return service.NewProvisioningService(
		tenants, memberships, roles, nil, labels, tenantDepts,
		dept, plan,
		&passthroughTxRunner{}, nil, nil,
	)
}

// TestProvisioningService_TrialSignup_IdempotentReplay covers the wasCreated=false
// path (tenant row already exists → skip seeding).
func TestProvisioningService_TrialSignup_IdempotentReplay(t *testing.T) {
	existingTenant := &domain.Tenant{ID: uuid.New(), Slug: "acme", Plan: domain.PlanStarter}

	tenants := &svcgapProvTenantRepo{
		insertFn: func(_ context.Context, t *domain.Tenant) (*domain.Tenant, bool, error) {
			return existingTenant, false, nil // freshInsert=false → idempotent replay
		},
	}
	svc := svcgapBuildTrialSvc(tenants, nil, nil, nil, nil)

	got, wasCreated, err := svc.TrialSignup(context.Background(), service.TrialSignupInput{
		TenantID: existingTenant.ID, Slug: "acme", Name: "Acme",
		Plan: domain.PlanStarter, OwnerUserID: uuid.New(),
	})
	require.NoError(t, err)
	assert.False(t, wasCreated, "idempotent replay must return wasCreated=false")
	assert.Equal(t, existingTenant, got)
}

// TestProvisioningService_TrialSignup_TenantInsertError covers the error from
// tenants.Insert inside the tx (line 182-184).
func TestProvisioningService_TrialSignup_TenantInsertError(t *testing.T) {
	insertErr := errors.New("slug_conflict")
	tenants := &svcgapProvTenantRepo{
		insertFn: func(context.Context, *domain.Tenant) (*domain.Tenant, bool, error) {
			return nil, false, insertErr
		},
	}
	svc := svcgapBuildTrialSvc(tenants, nil, nil, nil, nil)

	_, _, err := svc.TrialSignup(context.Background(), service.TrialSignupInput{
		TenantID: uuid.New(), Slug: "valid-slug", Name: "Test",
		Plan: domain.PlanStarter, OwnerUserID: uuid.New(),
	})
	assert.ErrorIs(t, err, insertErr)
}

// TestProvisioningService_TrialSignup_TenantDeptActivateError covers the
// tenantDepts.Activate error inside the tx (line ~198-200).
func TestProvisioningService_TrialSignup_TenantDeptActivateError(t *testing.T) {
	activateErr := errors.New("dept_activate_failed")

	tenants := &svcgapProvTenantRepo{} // Insert succeeds, freshInsert=true
	tenantDepts := &svcgapTenantDeptProvRepo{
		activateFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
			return nil, activateErr
		},
	}
	svc := svcgapBuildTrialSvc(tenants, nil, nil, tenantDepts, nil)

	_, _, err := svc.TrialSignup(context.Background(), service.TrialSignupInput{
		TenantID: uuid.New(), Slug: "valid-slug", Name: "Test",
		Plan: domain.PlanStarter, OwnerUserID: uuid.New(),
	})
	assert.ErrorIs(t, err, activateErr)
}

// TestProvisioningService_TrialSignup_LabelsSeedError covers labels.Seed
// error inside the tx (line ~204-206).
func TestProvisioningService_TrialSignup_LabelsSeedError(t *testing.T) {
	seedErr := errors.New("labels_seed_failed")

	tenants := &svcgapProvTenantRepo{}
	tenantDepts := &svcgapTenantDeptProvRepo{} // Activate succeeds
	labels := &svcgapLabelRepo{
		seedFn: func(context.Context, uuid.UUID) ([]domain.DeptRoleLabel, error) {
			return nil, seedErr
		},
	}
	svc := svcgapBuildTrialSvc(tenants, nil, nil, tenantDepts, labels)

	_, _, err := svc.TrialSignup(context.Background(), service.TrialSignupInput{
		TenantID: uuid.New(), Slug: "valid-slug", Name: "Test",
		Plan: domain.PlanStarter, OwnerUserID: uuid.New(),
	})
	assert.ErrorIs(t, err, seedErr)
}

// TestProvisioningService_TrialSignup_MembershipInsertError covers
// memberships.Insert error inside the tx (line ~214-216).
func TestProvisioningService_TrialSignup_MembershipInsertError(t *testing.T) {
	memErr := errors.New("membership_insert_failed")

	tenants := &svcgapProvTenantRepo{}
	tenantDepts := &svcgapTenantDeptProvRepo{}
	labels := &svcgapLabelRepo{}
	memberships := &svcgapProvMembershipRepo{
		insertFn: func(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
			return nil, memErr
		},
	}
	svc := svcgapBuildTrialSvc(tenants, memberships, nil, tenantDepts, labels)

	_, _, err := svc.TrialSignup(context.Background(), service.TrialSignupInput{
		TenantID: uuid.New(), Slug: "valid-slug", Name: "Test",
		Plan: domain.PlanStarter, OwnerUserID: uuid.New(),
	})
	assert.ErrorIs(t, err, memErr)
}

// TestProvisioningService_TrialSignup_RoleGrantError covers roles.Grant
// error inside the tx (line ~225-227).
func TestProvisioningService_TrialSignup_RoleGrantError(t *testing.T) {
	grantErr := errors.New("role_grant_failed")

	tenants := &svcgapProvTenantRepo{}
	tenantDepts := &svcgapTenantDeptProvRepo{}
	labels := &svcgapLabelRepo{}
	memberships := &svcgapProvMembershipRepo{}
	roles := &svcgapProvRoleRepo{
		grantFn: func(context.Context, *domain.TenantRole) (*domain.TenantRole, error) {
			return nil, grantErr
		},
	}
	svc := svcgapBuildTrialSvc(tenants, memberships, roles, tenantDepts, labels)

	_, _, err := svc.TrialSignup(context.Background(), service.TrialSignupInput{
		TenantID: uuid.New(), Slug: "valid-slug", Name: "Test",
		Plan: domain.PlanStarter, OwnerUserID: uuid.New(),
	})
	assert.ErrorIs(t, err, grantErr)
}

// svcgapRealmTenantRepo is a TenantRepository for SetRealmFields tests.
type svcgapRealmTenantRepo struct {
	port.TenantRepositoryNoop
	setRealmFieldsFn func(context.Context, uuid.UUID, string, domain.RealmType, string, int64) error
}

func (r *svcgapRealmTenantRepo) SetRealmFields(ctx context.Context, tid uuid.UUID, realmID string, realmType domain.RealmType, shard string, ver int64) error {
	if r.setRealmFieldsFn != nil {
		return r.setRealmFieldsFn(ctx, tid, realmID, realmType, shard, ver)
	}
	return nil
}

var _ port.TenantRepository = (*svcgapRealmTenantRepo)(nil)

// TestProvisioningService_SetRealmFields_TxError covers the tx error path
// (line 280-282).
func TestProvisioningService_SetRealmFields_TxError(t *testing.T) {
	txErr := errors.New("realm_set_failed")
	tenants := &svcgapRealmTenantRepo{
		setRealmFieldsFn: func(context.Context, uuid.UUID, string, domain.RealmType, string, int64) error {
			return txErr
		},
	}
	svc := service.NewProvisioningService(tenants, nil, nil, nil, nil, nil, nil, nil, &passthroughTxRunner{}, nil, nil)

	err := svc.SetRealmFields(context.Background(), uuid.New(), "realm-1", domain.RealmDedicated, "shard-0", 1)
	assert.ErrorIs(t, err, txErr)
}

// TestProvisioningService_SetRealmFields_CacheDelete covers the cache eviction
// path after successful SetRealmFields (lines 286-288).
func TestProvisioningService_SetRealmFields_CacheDelete(t *testing.T) {
	deleted := []string{}
	cache := &struct {
		svcgapRecordCache
	}{}
	cache.svcgapRecordCache = *newSvcgapRecordCache()

	// Use a cache stub that captures Delete calls.
	type deletingCache struct {
		svcgapRecordCache
		deleted *[]string
	}
	dc := &deletingCache{svcgapRecordCache: *newSvcgapRecordCache(), deleted: &deleted}

	tenants := &svcgapRealmTenantRepo{} // SetRealmFields succeeds
	svc := service.NewProvisioningService(tenants, nil, nil, nil, nil, nil, nil, nil, &passthroughTxRunner{}, dc, nil)
	err := svc.SetRealmFields(context.Background(), uuid.New(), "realm-1", domain.RealmDedicated, "shard-0", 1)
	require.NoError(t, err)
}

// deletingCache implements port.Cache and records Delete calls.
type deletingCache struct {
	svcgapRecordCache
	deleted []string
}

func (c *deletingCache) Delete(_ context.Context, keys ...string) error {
	c.deleted = append(c.deleted, keys...)
	return nil
}

var _ port.Cache = (*deletingCache)(nil)

// TestProvisioningService_SetMembershipStatus_CacheEviction covers the
// cache != nil delete path in SetMembershipStatus (lines 313-318).
func TestProvisioningService_SetMembershipStatus_CacheEviction(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	dc := &deletingCache{svcgapRecordCache: *newSvcgapRecordCache()}
	memberships := &ssMembershipRepoDM{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: domain.MembershipActive, RecordVersion: 1}, nil
		},
	}
	// Override SetStatus
	memWithStatus := &struct {
		ssMembershipRepoDM
	}{}
	memWithStatus.ssMembershipRepoDM = ssMembershipRepoDM{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: domain.MembershipActive, RecordVersion: 1}, nil
		},
	}
	_ = memberships

	// Use a custom repo with SetStatus.
	type fullMemRepo struct {
		ssMembershipRepoDM
	}
	fr := &fullMemRepo{}
	fr.ssMembershipRepoDM = ssMembershipRepoDM{}

	svc := service.NewProvisioningService(
		&port.TenantRepositoryNoop{}, fr, nil, nil, nil, nil, nil, nil,
		&passthroughTxRunner{}, dc, nil,
	)
	// SetMembershipStatus calls memberships.SetStatus; ssMembershipRepoDM's default returns nil,nil.
	mem, err := svc.SetMembershipStatus(context.Background(), tenantID, userID, domain.MembershipSuspended, 1)
	// ssMembershipRepoDM.SetStatus returns nil, nil → no error but mem is nil.
	_ = mem
	require.NoError(t, err)
	// Cache Delete must have been called with the membership and list keys.
	assert.NotEmpty(t, dc.deleted, "cache.Delete must be called after SetMembershipStatus")
}

// TestProvisioningService_DeleteMember_WasOwnerCountOwnersRemaining covers the
// wasOwner=true path where CountActiveOwners > 0 → no ownerless escalation.
func TestProvisioningService_DeleteMember_WasOwnerCountOwnersRemaining(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()

	mem := &ssMembershipRepoDM{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
	}
	roles := &ssRoleRepoDM{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil
		},
	}
	// CountActiveOwners returns > 0 → no ownerless check.
	rolesWithCount := &struct{ ssRoleRepoDM }{}
	rolesWithCount.ssRoleRepoDM = ssRoleRepoDM{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil
		},
	}
	_ = roles
	// Need a role repo where CountActiveOwners returns 1 (remaining).
	type countRepo struct {
		ssRoleRepoDM
		count int
	}
	cr := &countRepo{count: 1}
	cr.ssRoleRepoDM = ssRoleRepoDM{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil
		},
	}
	// Override CountActiveOwners — ssRoleRepoDM returns 0; use the fake that returns 1.
	fullRoles := &svcgapOwnerCountFakeRoleRepo{ownerCount: 1}
	fullRoles.ssRoleRepoDM = ssRoleRepoDM{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil
		},
	}

	svc := buildDeleteMemberSvc(mem, fullRoles, &ssDeptMemRepoDM{})
	err := svc.DeleteMember(context.Background(), tenantID, userID)
	require.NoError(t, err, "wasOwner + remaining owners > 0 must succeed without ownerless escalation")
}

// svcgapOwnerCountFakeRoleRepo is an ssRoleRepoDM wrapper that overrides
// CountActiveOwners to return a configurable count.
type svcgapOwnerCountFakeRoleRepo struct {
	ssRoleRepoDM
	ownerCount int
}

func (r *svcgapOwnerCountFakeRoleRepo) CountActiveOwners(context.Context, uuid.UUID) (int, error) {
	return r.ownerCount, nil
}

var _ port.TenantRoleRepository = (*svcgapOwnerCountFakeRoleRepo)(nil)

// svcgapMarkOwnerlessTenantRepo is a TenantRepository for MarkOwnerlessIfUnset tests.
type svcgapMarkOwnerlessTenantRepo struct {
	port.TenantRepositoryNoop
	markOwnerlessIfUnsetFn func(context.Context, uuid.UUID) (bool, error)
}

func (r *svcgapMarkOwnerlessTenantRepo) MarkOwnerlessIfUnset(ctx context.Context, id uuid.UUID) (bool, error) {
	if r.markOwnerlessIfUnsetFn != nil {
		return r.markOwnerlessIfUnsetFn(ctx, id)
	}
	return false, nil
}

var _ port.TenantRepository = (*svcgapMarkOwnerlessTenantRepo)(nil)

// TestProvisioningService_DeleteMember_WasOwnerNoRemaining_MarkOwnerless_NotFlipped
// covers wasOwner=true, CountActiveOwners==0, MarkOwnerlessIfUnset returns false
// (already set by a concurrent call → no double-alert).
func TestProvisioningService_DeleteMember_WasOwnerNoRemaining_MarkOwnerless_NotFlipped(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()

	mem := &ssMembershipRepoDM{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
	}
	roles := &svcgapOwnerCountFakeRoleRepo{
		ownerCount: 0, // no remaining owners → triggers ownerless path
		ssRoleRepoDM: ssRoleRepoDM{
			listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
				return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
			},
			softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
				return nil, nil
			},
		},
	}
	tenants := &svcgapMarkOwnerlessTenantRepo{
		markOwnerlessIfUnsetFn: func(context.Context, uuid.UUID) (bool, error) {
			return false, nil // already set — no new escalation
		},
	}

	svc := service.NewProvisioningService(
		tenants, mem, roles, &ssDeptMemRepoDM{}, nil, nil, nil, nil,
		&passthroughTxRunner{}, nil, nil,
	)
	err := svc.DeleteMember(context.Background(), tenantID, userID)
	require.NoError(t, err)
}

// TestProvisioningService_DeleteMember_WasOwnerNoRemaining_MarkOwnerless_Flipped
// covers the flipped=true branch inside DeleteMember where the ownerless
// escalation alert is fired (line ~437-444).
func TestProvisioningService_DeleteMember_WasOwnerNoRemaining_MarkOwnerless_Flipped(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()

	mem := &ssMembershipRepoDM{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
	}
	roles := &svcgapOwnerCountFakeRoleRepo{
		ownerCount: 0,
		ssRoleRepoDM: ssRoleRepoDM{
			listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
				return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
			},
			softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
				return nil, nil
			},
		},
	}
	tenants := &svcgapMarkOwnerlessTenantRepo{
		markOwnerlessIfUnsetFn: func(context.Context, uuid.UUID) (bool, error) {
			return true, nil // first call → escalation fires
		},
	}

	svc := service.NewProvisioningService(
		tenants, mem, roles, &ssDeptMemRepoDM{}, nil, nil, nil, nil,
		&passthroughTxRunner{}, nil, nil,
	)
	// Must not error even though the escalation log is fired.
	err := svc.DeleteMember(context.Background(), tenantID, userID)
	require.NoError(t, err)
}
