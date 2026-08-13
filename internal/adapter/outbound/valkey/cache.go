// Package valkey implements port.Cache backed by Valkey (Redis-compatible).
// Cache is advisory-only (CACHE-2/CACHE-9) — every miss, timeout, or outage
// must fall through to Postgres; /readyz stays ready while Postgres is
// healthy even if the cache is down.
package valkey

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Cache implements port.Cache.
type Cache struct {
	client *redis.Client
}

var _ port.Cache = (*Cache)(nil)

// New creates a Cache from addr. addr may be plain host:port or a full URL
// (redis://user:pass@host or rediss://... for TLS — required in production).
//
// Timeouts are tight by default (50 ms read/write, 100 ms dial): a slow
// Valkey must degrade to a fast miss rather than stall the request until the
// DB-backed fallback is no longer within SLO.
func New(addr string) *Cache {
	opts, err := redis.ParseURL(addr)
	if err != nil {
		opts = &redis.Options{Addr: addr}
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 100 * time.Millisecond
	}
	if opts.ReadTimeout == 0 {
		opts.ReadTimeout = 50 * time.Millisecond
	}
	if opts.WriteTimeout == 0 {
		opts.WriteTimeout = 50 * time.Millisecond
	}
	return &Cache{client: redis.NewClient(opts)}
}

// Get returns (nil, nil) on cache miss.
func (c *Cache) Get(ctx context.Context, key string) ([]byte, error) {
	val, err := c.client.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	return val, nil
}

// MGet returns a same-length slice with nil entries for misses.
func (c *Cache) MGet(ctx context.Context, keys []string) ([][]byte, error) {
	vals, err := c.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := make([][]byte, len(vals))
	for i, v := range vals {
		if v == nil {
			continue
		}
		if s, ok := v.(string); ok {
			out[i] = []byte(s)
		}
	}
	return out, nil
}

func (c *Cache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return c.client.Set(ctx, key, value, ttl).Err()
}

func (c *Cache) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	return c.client.SetNX(ctx, key, value, ttl).Result()
}

func (c *Cache) Delete(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return c.client.Del(ctx, keys...).Err()
}

func (c *Cache) Health(ctx context.Context) error {
	return c.client.Ping(ctx).Err()
}

func (c *Cache) Close() error {
	return c.client.Close()
}

// ─── Key builders (§6.1) ─────────────────────────────────────────────────
//
// All keys are tenant-scoped and prefixed "om:" to mirror the RLS boundary
// (CACHE-1). Business services in Phase 2+ use these helpers so cache
// invalidation and reads share a single naming source of truth.

// MembershipsKey builds om:memberships:{tenant}:{user} for the I-8 hot path.
func (c *Cache) MembershipsKey(tenantID, userID uuid.UUID) string {
	return fmt.Sprintf("om:memberships:%s:%s", tenantID, userID)
}

// TenantKey builds om:tenant:{tenant}.
func (c *Cache) TenantKey(tenantID uuid.UUID) string {
	return fmt.Sprintf("om:tenant:%s", tenantID)
}

// MembersPageKey builds om:members:{tenant}:{limit} — CACHE-10 restricts
// caching to page-1 cursorless at limit=50 only.
func (c *Cache) MembersPageKey(tenantID uuid.UUID, limit int) string {
	return fmt.Sprintf("om:members:%s:%d", tenantID, limit)
}

// DeptMembersKey builds om:dept_members:{tenant}:{dept}.
func (c *Cache) DeptMembersKey(tenantID, deptID uuid.UUID) string {
	return fmt.Sprintf("om:dept_members:%s:%s", tenantID, deptID)
}

// LocaleKey builds om:locale:{tenant}.
func (c *Cache) LocaleKey(tenantID uuid.UUID) string {
	return fmt.Sprintf("om:locale:%s", tenantID)
}

// RolesKey builds om:roles:{tenant} for the dept-role-labels catalog.
func (c *Cache) RolesKey(tenantID uuid.UUID) string {
	return fmt.Sprintf("om:roles:%s", tenantID)
}

// GroupRoleMappingsKey builds om:grm:{tenant}.
func (c *Cache) GroupRoleMappingsKey(tenantID uuid.UUID) string {
	return fmt.Sprintf("om:grm:%s", tenantID)
}

// GroupDeptMappingsKey builds om:gdm:{tenant}.
func (c *Cache) GroupDeptMappingsKey(tenantID uuid.UUID) string {
	return fmt.Sprintf("om:gdm:%s", tenantID)
}

// SeatUsageKey builds om:seat_usage:{tenant} — 30s TTL (CACHE-5); SEAT-1
// always re-reads Postgres under FOR UPDATE regardless of cache state.
func (c *Cache) SeatUsageKey(tenantID uuid.UUID) string {
	return fmt.Sprintf("om:seat_usage:%s", tenantID)
}

// PlansKey builds om:plans (global, not tenant-scoped) — the plans catalog
// is operator-editable via O-6 and evicted on PATCH.
func (c *Cache) PlansKey() string {
	return "om:plans"
}

// DepartmentsKey builds om:departments (global, not tenant-scoped) — the
// read-through cache populated from catalog-admin-config's CAT-I1 bulk
// endpoint (migration-runbook Phase 2).
func (c *Cache) DepartmentsKey() string {
	return "om:departments"
}

// DepartmentsStaleKey builds om:departments:stale — the 24h stale-if-error
// fallback (CAT-D4), served only when DepartmentsKey has expired and the
// live call to catalog-admin-config also fails.
func (c *Cache) DepartmentsStaleKey() string {
	return "om:departments:stale"
}

// PlansStaleKey builds om:plans:stale — the 24h stale-if-error fallback
// (CAT-D4) for the plans catalog.
func (c *Cache) PlansStaleKey() string {
	return "om:plans:stale"
}
