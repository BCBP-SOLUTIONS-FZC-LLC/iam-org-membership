// Unit tests for the om:seat_usage read-through cache behind SeatUsage
// (P-27 / I-11, CACHE-5): a miss reads Postgres and fills the key with a
// 30 s TTL, a hit is served without touching Postgres, and a membership
// write evicts the key so the next read is fresh.
package unit_test

import (
	"context"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mapCache is an in-memory port.Cache that keeps values and records TTLs.
type mapCache struct {
	store map[string][]byte
	ttls  map[string]time.Duration
}

func newMapCache() *mapCache {
	return &mapCache{store: map[string][]byte{}, ttls: map[string]time.Duration{}}
}

func (c *mapCache) Get(_ context.Context, key string) ([]byte, error) { return c.store[key], nil }
func (c *mapCache) MGet(_ context.Context, keys []string) ([][]byte, error) {
	out := make([][]byte, len(keys))
	for i, k := range keys {
		out[i] = c.store[k]
	}
	return out, nil
}
func (c *mapCache) Set(_ context.Context, key string, val []byte, ttl time.Duration) error {
	c.store[key], c.ttls[key] = val, ttl
	return nil
}
func (c *mapCache) SetNX(ctx context.Context, key string, val []byte, ttl time.Duration) (bool, error) {
	if _, ok := c.store[key]; ok {
		return false, nil
	}
	return true, c.Set(ctx, key, val, ttl)
}
func (c *mapCache) Delete(_ context.Context, keys ...string) error {
	for _, k := range keys {
		delete(c.store, k)
	}
	return nil
}
func (c *mapCache) Health(context.Context) error { return nil }
func (c *mapCache) Close() error                 { return nil }

var _ port.Cache = (*mapCache)(nil)

// seatCacheMemRepo counts CountActive calls (each one is a Postgres read)
// and accepts the reactivate SetStatus used as the membership write.
type seatCacheMemRepo struct {
	fakeMemRepoFull
	active int
	reads  int
}

func (r *seatCacheMemRepo) CountActive(context.Context, uuid.UUID) (int, error) {
	r.reads++
	return r.active, nil
}

func (r *seatCacheMemRepo) SetStatus(_ context.Context, tenantID, userID uuid.UUID, st domain.MembershipStatus, ver int64) (*domain.TenantMembership, error) {
	return &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID, Status: st, RecordVersion: ver + 1}, nil
}

func TestMembership_SeatUsage_CacheMissHitAndEvictionOnMembershipWrite(t *testing.T) {
	ctx := context.Background()
	tenantID := uuid.New()
	key := "om:seat_usage:" + tenantID.String()

	mem := &seatCacheMemRepo{active: 3}
	tenants := &fakeTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, LicensedSeats: 10}, nil
		},
	}
	inv := &fakeInviteRepo{countPendingFn: func(context.Context, uuid.UUID) (int, error) { return 2, nil }}
	cache := newMapCache()
	svc := service.NewMembershipService(mem, noOwnerRoleRepo{}, nil, tenants, inv, cache,
		&fakeRPClient{}, nil, &passthroughTxRunner{}, nil)

	// Miss: reads Postgres and fills the key with the 30 s TTL.
	got, err := svc.SeatUsage(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, 3, got.ActiveUsers)
	assert.Equal(t, 1, mem.reads)
	require.Contains(t, cache.store, key, "a miss must fill om:seat_usage")
	assert.Equal(t, 30*time.Second, cache.ttls[key])

	// Hit: Postgres changed, but the cached value is served without a read.
	mem.active = 4
	got, err = svc.SeatUsage(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, 3, got.ActiveUsers, "a hit returns the cached value")
	assert.Equal(t, 1, mem.reads, "a hit must not read Postgres")

	// Eviction: a membership write deletes the key, so the next read is fresh.
	_, err = svc.SetStatus(ctx, tenantID, uuid.New(), domain.MembershipActive, 1)
	require.NoError(t, err)
	assert.NotContains(t, cache.store, key, "a membership write must evict om:seat_usage")

	got, err = svc.SeatUsage(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, 4, got.ActiveUsers)
	assert.Equal(t, 2, mem.reads)
	assert.Equal(t, 2, got.PendingInvitations)
	assert.Equal(t, 10, got.LicensedSeats)
}
