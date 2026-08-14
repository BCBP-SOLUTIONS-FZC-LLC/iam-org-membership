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
)

// ── fakes local to this file ────────────────────────────────────────────

type fakeGroupMappingClient struct {
	resolveFn func(ctx context.Context, tenantID uuid.UUID, groups []string) (*port.GroupResolution, error)
	calls     int
}

func (f *fakeGroupMappingClient) ResolveGroups(ctx context.Context, tenantID uuid.UUID, groups []string) (*port.GroupResolution, error) {
	f.calls++
	if f.resolveFn != nil {
		return f.resolveFn(ctx, tenantID, groups)
	}
	return nil, errors.New("not used")
}

var _ port.GroupMappingClient = (*fakeGroupMappingClient)(nil)

// gmCache is a minimal in-memory port.Cache for these tests — same shape
// as catalog_service_test.go's csCache.
type gmCache struct {
	values map[string][]byte
}

func newGMCache() *gmCache { return &gmCache{values: map[string][]byte{}} }

func (c *gmCache) Get(_ context.Context, key string) ([]byte, error) { return c.values[key], nil }
func (c *gmCache) MGet(_ context.Context, keys []string) ([][]byte, error) {
	out := make([][]byte, len(keys))
	for i, k := range keys {
		out[i] = c.values[k]
	}
	return out, nil
}
func (c *gmCache) Set(_ context.Context, key string, value []byte, _ time.Duration) error {
	c.values[key] = value
	return nil
}
func (c *gmCache) SetNX(_ context.Context, key string, value []byte, _ time.Duration) (bool, error) {
	if _, ok := c.values[key]; ok {
		return false, nil
	}
	c.values[key] = value
	return true, nil
}
func (c *gmCache) Delete(_ context.Context, keys ...string) error {
	for _, k := range keys {
		delete(c.values, k)
	}
	return nil
}
func (c *gmCache) Health(_ context.Context) error { return nil }
func (c *gmCache) Close() error                   { return nil }

var _ port.Cache = (*gmCache)(nil)

func buildResolveSvc(client port.GroupMappingClient, cache port.Cache) *GroupMappingService {
	return NewGroupMappingService(nil, nil, nil, nil, cache, client)
}

// ── resolveMappings — cache miss populates primary + stale ─────────────

func TestResolveMappings_CacheMissPopulatesPrimaryAndStale(t *testing.T) {
	tenantID := uuid.New()
	deptID := uuid.New()
	client := &fakeGroupMappingClient{
		resolveFn: func(context.Context, uuid.UUID, []string) (*port.GroupResolution, error) {
			return &port.GroupResolution{
				DeptMappings:       []port.ResolvedDeptMapping{{KeycloakGroupName: "eng-team", DepartmentID: deptID}},
				DeptRoleMappings:   []port.ResolvedDeptRoleMapping{{KeycloakGroupName: "eng-team", RoleCode: domain.DeptReviewer}},
				TenantRoleMappings: []port.ResolvedTenantRoleMapping{{KeycloakGroupName: "admins", RoleCode: domain.RoleTenantAdmin}},
			}, nil
		},
	}
	cache := newGMCache()
	svc := buildResolveSvc(client, cache)

	dm, dr, tr := svc.resolveMappings(context.Background(), tenantID, []string{"eng-team", "admins"})
	assert.Len(t, dm, 1)
	assert.Len(t, dr, 1)
	assert.Len(t, tr, 1)
	assert.Equal(t, 1, client.calls)

	assert.NotEmpty(t, cache.values[cacheKeyGDM(tenantID)])
	assert.NotEmpty(t, cache.values[cacheKeyGRM(tenantID)])
	assert.NotEmpty(t, cache.values[cacheKeyGTRM(tenantID)])
	assert.NotEmpty(t, cache.values[cacheKeyGDMStale(tenantID)])
	assert.NotEmpty(t, cache.values[cacheKeyGRMStale(tenantID)])
	assert.NotEmpty(t, cache.values[cacheKeyGTRMStale(tenantID)])
}

// ── resolveMappings — warm primary cache skips the client entirely ─────

func TestResolveMappings_CacheHitSkipsClient(t *testing.T) {
	tenantID := uuid.New()
	client := &fakeGroupMappingClient{}
	cache := newGMCache()
	cache.values[cacheKeyGDM(tenantID)] = []byte(`[]`)
	cache.values[cacheKeyGRM(tenantID)] = []byte(`[{"TenantID":"` + tenantID.String() + `","KeycloakGroupName":"eng-team","RoleCode":"reviewer"}]`)
	cache.values[cacheKeyGTRM(tenantID)] = []byte(`[]`)
	svc := buildResolveSvc(client, cache)

	dm, dr, tr := svc.resolveMappings(context.Background(), tenantID, []string{"eng-team"})
	assert.Empty(t, dm)
	assert.Len(t, dr, 1)
	assert.Empty(t, tr)
	assert.Equal(t, 0, client.calls, "warm primary cache must short-circuit the client entirely")
}

// ── resolveMappings — partial cache hit is treated as a full miss ──────

func TestResolveMappings_PartialCacheHitIsTreatedAsMiss(t *testing.T) {
	tenantID := uuid.New()
	client := &fakeGroupMappingClient{
		resolveFn: func(context.Context, uuid.UUID, []string) (*port.GroupResolution, error) {
			return &port.GroupResolution{}, nil
		},
	}
	cache := newGMCache()
	// Only GDM is warm — GRM/GTRM are missing.
	cache.values[cacheKeyGDM(tenantID)] = []byte(`[]`)
	svc := buildResolveSvc(client, cache)

	svc.resolveMappings(context.Background(), tenantID, []string{"eng-team"})
	assert.Equal(t, 1, client.calls, "a partial cache hit must fall through to the client, not serve a mixed result")
}

// ── resolveMappings — client fails, serves stale ────────────────────────

func TestResolveMappings_ClientFailsServesStale(t *testing.T) {
	tenantID := uuid.New()
	client := &fakeGroupMappingClient{
		resolveFn: func(context.Context, uuid.UUID, []string) (*port.GroupResolution, error) {
			return nil, errors.New("group-mapping-jit-config unreachable")
		},
	}
	cache := newGMCache()
	cache.values[cacheKeyGDMStale(tenantID)] = []byte(`[{"TenantID":"` + tenantID.String() + `","KeycloakGroupName":"eng-team","DepartmentID":"` + uuid.New().String() + `"}]`)
	cache.values[cacheKeyGRMStale(tenantID)] = []byte(`[]`)
	cache.values[cacheKeyGTRMStale(tenantID)] = []byte(`[]`)
	svc := buildResolveSvc(client, cache)

	dm, dr, tr := svc.resolveMappings(context.Background(), tenantID, []string{"eng-team"})
	assert.Len(t, dm, 1, "a warm stale-if-error key must be served, not an empty resolution")
	assert.Empty(t, dr)
	assert.Empty(t, tr)
}

// ── resolveMappings — client fails, no cache at all → fail open ────────

func TestResolveMappings_ClientFailsAndNoCache_FailsOpenWithEmptyResolution(t *testing.T) {
	client := &fakeGroupMappingClient{
		resolveFn: func(context.Context, uuid.UUID, []string) (*port.GroupResolution, error) {
			return nil, errors.New("group-mapping-jit-config unreachable")
		},
	}
	svc := buildResolveSvc(client, newGMCache())

	dm, dr, tr := svc.resolveMappings(context.Background(), uuid.New(), []string{"eng-team"})
	assert.Empty(t, dm, "ADR-0007 Action Item 4: must fail open, never error, so I-10 still returns 200")
	assert.Empty(t, dr)
	assert.Empty(t, tr)
}

// ── resolveMappings — nil client, no cache → fail open ──────────────────

func TestResolveMappings_NilClientAndNoCache_FailsOpen(t *testing.T) {
	svc := buildResolveSvc(nil, nil)
	dm, dr, tr := svc.resolveMappings(context.Background(), uuid.New(), []string{"eng-team"})
	assert.Empty(t, dm)
	assert.Empty(t, dr)
	assert.Empty(t, tr)
}

// ── resolveMappings — nil cache always calls the client ─────────────────

func TestResolveMappings_NilCache_AlwaysCallsClient(t *testing.T) {
	tenantID := uuid.New()
	client := &fakeGroupMappingClient{
		resolveFn: func(context.Context, uuid.UUID, []string) (*port.GroupResolution, error) {
			return &port.GroupResolution{}, nil
		},
	}
	svc := buildResolveSvc(client, nil)

	svc.resolveMappings(context.Background(), tenantID, []string{"eng-team"})
	svc.resolveMappings(context.Background(), tenantID, []string{"eng-team"})
	assert.Equal(t, 2, client.calls, "with no cache, every call must reach the client")
}
