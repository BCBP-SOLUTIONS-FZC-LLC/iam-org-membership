// Phase 19 — tenant_service.go coverage: cache-hit / cache-miss / cache-error
// / T-10 mfa_freshness range / T-15 realm-sync deferred / invalidate.
package unit_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── LOCAL fakes ────────────────────────────────────────────────────────

type tsCache struct {
	get map[string][]byte
	set map[string][]byte
	del []string
}

func newTSCache() *tsCache { return &tsCache{get: map[string][]byte{}, set: map[string][]byte{}} }

func (c *tsCache) Get(_ context.Context, key string) ([]byte, error) {
	if v, ok := c.get[key]; ok {
		return v, nil
	}
	return nil, nil
}
func (c *tsCache) MGet(context.Context, []string) ([][]byte, error) { return nil, nil }
func (c *tsCache) Set(_ context.Context, key string, v []byte, _ time.Duration) error {
	c.set[key] = v
	return nil
}
func (c *tsCache) SetNX(context.Context, string, []byte, time.Duration) (bool, error) {
	return true, nil
}
func (c *tsCache) Delete(_ context.Context, keys ...string) error {
	c.del = append(c.del, keys...)
	return nil
}
func (c *tsCache) Health(context.Context) error { return nil }
func (c *tsCache) Close() error                 { return nil }

var _ port.Cache = (*tsCache)(nil)

type tsRP struct {
	patchRealmConfigFn func(context.Context, uuid.UUID, port.RealmConfigPatch) error
}

func (r *tsRP) CreateInvitedUser(context.Context, port.CreateInvitedUserRequest) (*port.CreateInvitedUserResponse, error) {
	return nil, nil
}
func (r *tsRP) DeleteUser(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (r *tsRP) PatchRealmConfig(ctx context.Context, tid uuid.UUID, p port.RealmConfigPatch) error {
	if r.patchRealmConfigFn != nil {
		return r.patchRealmConfigFn(ctx, tid, p)
	}
	return nil
}
func (r *tsRP) RevokeUserSessions(context.Context, uuid.UUID, uuid.UUID) error { return nil }

var _ port.RealmProvisionerClient = (*tsRP)(nil)

// ── Get: cache hit vs miss ─────────────────────────────────────────────

func TestTenantService_Get_CacheHit_SkipsRepo(t *testing.T) {
	tenantID := uuid.New()
	cached := &domain.Tenant{ID: tenantID, Slug: "acme"}
	raw, _ := json.Marshal(cached)
	cache := newTSCache()
	cache.get["om:tenant:"+tenantID.String()] = raw
	repoCalled := false
	repo := &fakeTenantRepo{findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
		repoCalled = true
		return nil, errors.New("should not be called")
	}}

	svc := service.NewTenantService(repo, cache, &tsRP{})
	got, err := svc.Get(context.Background(), tenantID)

	require.NoError(t, err)
	assert.Equal(t, "acme", got.Slug)
	assert.False(t, repoCalled, "cache hit must skip repo")
}

func TestTenantService_Get_CacheMiss_FallsThroughToRepo(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeTenantRepo{findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
		return &domain.Tenant{ID: id, Slug: "acme", Name: "Acme"}, nil
	}}
	cache := newTSCache()

	svc := service.NewTenantService(repo, cache, &tsRP{})
	got, err := svc.Get(context.Background(), tenantID)

	require.NoError(t, err)
	assert.Equal(t, "acme", got.Slug)
	// setCached should have populated the cache on the miss.
	_, ok := cache.set["om:tenant:"+tenantID.String()]
	assert.True(t, ok, "cache miss should populate the cache")
}

func TestTenantService_Get_CachedCorrupt_FallsThroughToRepo(t *testing.T) {
	tenantID := uuid.New()
	cache := newTSCache()
	cache.get["om:tenant:"+tenantID.String()] = []byte("not-json")
	repo := &fakeTenantRepo{findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
		return &domain.Tenant{ID: id, Slug: "acme"}, nil
	}}

	svc := service.NewTenantService(repo, cache, &tsRP{})
	got, err := svc.Get(context.Background(), tenantID)

	require.NoError(t, err)
	assert.Equal(t, "acme", got.Slug)
}

func TestTenantService_Get_NilCache_FallsThroughToRepo(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeTenantRepo{findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
		return &domain.Tenant{ID: id, Slug: "acme"}, nil
	}}
	svc := service.NewTenantService(repo, nil, &tsRP{})
	got, err := svc.Get(context.Background(), tenantID)

	require.NoError(t, err)
	assert.Equal(t, "acme", got.Slug)
}

// ── Patch validation branches (T-10, locale) ─────────────────────────

func TestTenantService_Patch_NilPatch_ValidationError(t *testing.T) {
	svc := service.NewTenantService(&fakeTenantRepo{}, nil, &tsRP{})
	_, _, err := svc.Patch(context.Background(), uuid.New(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrValidation)
}

func TestTenantService_Patch_T10MFATooLow_ValidationError(t *testing.T) {
	svc := service.NewTenantService(&fakeTenantRepo{}, nil, &tsRP{})
	v := 30
	_, _, err := svc.Patch(context.Background(), uuid.New(), &domain.TenantPatch{MFAFreshnessSeconds: &v})
	require.Error(t, err)
}

func TestTenantService_Patch_T10MFATooHigh_ValidationError(t *testing.T) {
	svc := service.NewTenantService(&fakeTenantRepo{}, nil, &tsRP{})
	v := 3600
	_, _, err := svc.Patch(context.Background(), uuid.New(), &domain.TenantPatch{MFAFreshnessSeconds: &v})
	require.Error(t, err)
}

func TestTenantService_Patch_EmptyLocale_ValidationError(t *testing.T) {
	svc := service.NewTenantService(&fakeTenantRepo{}, nil, &tsRP{})
	empty := ""
	_, _, err := svc.Patch(context.Background(), uuid.New(), &domain.TenantPatch{DefaultLocale: &empty})
	require.Error(t, err)
}

// ── Patch T-15 realm sync deferred ────────────────────────────────────

// tsRepo is a fuller TenantRepository fake with Update behavior.
type tsRepo struct {
	findByIDFn func(context.Context, uuid.UUID) (*domain.Tenant, error)
	updateFn   func(context.Context, uuid.UUID, *domain.TenantPatch) (*domain.Tenant, error)
}

func (r *tsRepo) FindByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	if r.findByIDFn != nil {
		return r.findByIDFn(ctx, id)
	}
	return nil, errors.New("not implemented")
}
func (r *tsRepo) FindByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) { return r.FindByID(ctx, id) }
func (r *tsRepo) Update(ctx context.Context, id uuid.UUID, patch *domain.TenantPatch) (*domain.Tenant, error) {
	if r.updateFn != nil {
		return r.updateFn(ctx, id, patch)
	}
	return nil, errors.New("not implemented")
}
func (r *tsRepo) SetRealmSyncPending(context.Context, uuid.UUID) error { return nil }
func (r *tsRepo) Insert(context.Context, *domain.Tenant) (*domain.Tenant, bool, error) {
	return nil, false, nil
}

var _ port.TenantRepository = (*tsRepo)(nil)

func TestTenantService_Patch_LocalAccountsChange_RPFails_DeferredSync(t *testing.T) {
	tenantID := uuid.New()
	before := &domain.Tenant{ID: tenantID, LocalAccountsEnabled: false}
	repo := &tsRepo{
		findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) { return before, nil },
		updateFn: func(_ context.Context, id uuid.UUID, patch *domain.TenantPatch) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, LocalAccountsEnabled: true, RecordVersion: 2}, nil
		},
	}
	rp := &tsRP{patchRealmConfigFn: func(context.Context, uuid.UUID, port.RealmConfigPatch) error {
		return errors.New("RP down")
	}}
	svc := service.NewTenantService(repo, nil, rp)

	on := true
	_, deferred, err := svc.Patch(context.Background(), tenantID, &domain.TenantPatch{LocalAccountsEnabled: &on})
	require.NoError(t, err)
	assert.True(t, deferred, "RP error → realm_sync_pending → caller returns 202")
}

func TestTenantService_Patch_LocalAccountsUnchanged_NoRPCall(t *testing.T) {
	tenantID := uuid.New()
	before := &domain.Tenant{ID: tenantID, LocalAccountsEnabled: true}
	repo := &tsRepo{
		findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) { return before, nil },
		updateFn: func(_ context.Context, id uuid.UUID, patch *domain.TenantPatch) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, LocalAccountsEnabled: true, RecordVersion: 2}, nil
		},
	}
	rpCalled := false
	rp := &tsRP{patchRealmConfigFn: func(context.Context, uuid.UUID, port.RealmConfigPatch) error {
		rpCalled = true
		return nil
	}}
	svc := service.NewTenantService(repo, nil, rp)

	on := true
	_, deferred, err := svc.Patch(context.Background(), tenantID, &domain.TenantPatch{LocalAccountsEnabled: &on})
	require.NoError(t, err)
	assert.False(t, deferred)
	assert.False(t, rpCalled, "unchanged local_accounts_enabled should not trigger RP")
}

// ── setCached with nil / marshal-error / nil-cache branches ────────────

func TestTenantService_SetCached_NilCache_NoOp(t *testing.T) {
	// Just prove Get with a nil cache produces a repo call — indirectly
	// covers the nil-cache guard in setCached.
	repo := &fakeTenantRepo{findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
		return &domain.Tenant{ID: id}, nil
	}}
	svc := service.NewTenantService(repo, nil, &tsRP{})
	_, err := svc.Get(context.Background(), uuid.New())
	require.NoError(t, err)
}
