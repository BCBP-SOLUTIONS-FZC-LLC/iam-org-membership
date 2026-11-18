// Package valkey implements port.Cache backed by Valkey (Redis-compatible).
// Cache is advisory-only (CACHE-2/CACHE-9) — every miss, timeout, or outage
// must fall through to Postgres; /readyz stays ready (cache "degraded")
// while Postgres is healthy even if the cache is down.
package valkey

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Cache implements port.Cache.
type Cache struct {
	client *redis.Client
	logger port.Logger // nil-safe: no WARN lines when unset
}

var _ port.Cache = (*Cache)(nil)

// Client options. Constants, not env: no other cache setting is env-driven.
//
// Timeouts are tight (50 ms read/write, 100 ms dial): a slow Valkey must
// degrade to a fast miss rather than stall the request until the DB-backed
// fallback is no longer within SLO.
//
// Retries are off. go-redis defaults to MaxRetries=3 (4 attempts per
// command) and DialerRetries=5 (5 dials, 100 ms apart, per attempt), which
// made each cache call cost ~1.7 s with Valkey down — P-3 (18 cache calls)
// took 26 s. The cache is advisory (CACHE-9): one failed attempt is just a
// miss that falls through to Postgres.
const (
	dialTimeout   = 100 * time.Millisecond
	readTimeout   = 50 * time.Millisecond
	writeTimeout  = 50 * time.Millisecond
	maxRetries    = -1 // go-redis: -1 disables command retries (0 means "default 3")
	dialerRetries = 1  // go-redis: one dial attempt, no backoff sleep (<=0 means "default 5")
)

// New creates a Cache from addr. addr may be plain host:port or a full URL
// (redis://user:pass@host or rediss://... for TLS — required in production).
// Values set explicitly in the URL win over the defaults above.
func New(addr string) *Cache {
	opts, err := redis.ParseURL(addr)
	if err != nil {
		opts = &redis.Options{Addr: addr}
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = dialTimeout
	}
	if opts.ReadTimeout == 0 {
		opts.ReadTimeout = readTimeout
	}
	if opts.WriteTimeout == 0 {
		opts.WriteTimeout = writeTimeout
	}
	if opts.MaxRetries == 0 {
		opts.MaxRetries = maxRetries
	}
	if opts.DialerRetries == 0 {
		opts.DialerRetries = dialerRetries
	}
	return &Cache{client: redis.NewClient(opts)}
}

// WithLogger sets the logger used for the failed-invalidation WARN.
func (c *Cache) WithLogger(log port.Logger) *Cache {
	c.logger = log
	return c
}

// cacheErr counts a failed cache operation (never a miss) and returns err
// unchanged, so callers keep their existing error handling.
func cacheErr(op string, err error) error {
	if err != nil {
		metrics.IncCacheError(op)
	}
	return err
}

// knownKeyTypes bounds the key_type label to the om:* families built by
// the services; anything else is reported as "other".
var knownKeyTypes = map[string]bool{
	"tenant": true, "locale": true, "members": true, "memberships": true,
	"roles": true, "grm": true, "gdm": true, "gtrm": true, "dept_members": true,
	"seat_usage": true, "departments": true, "plans": true,
}

// keyType returns the family of an om:{type}:... key ("tenant" for
// om:tenant:{id}). Keys only carry UUIDs, never personal data.
func keyType(key string) string {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) >= 2 && parts[0] == "om" && knownKeyTypes[parts[1]] {
		return parts[1]
	}
	return "other"
}

// maxLoggedKeys caps the "keys" field of the invalidation WARN so a bulk
// DEL cannot produce an unbounded log line.
const maxLoggedKeys = 10

// logKeys joins at most maxLoggedKeys keys and appends "(+N more)".
func logKeys(keys []string) string {
	if len(keys) <= maxLoggedKeys {
		return strings.Join(keys, ",")
	}
	return fmt.Sprintf("%s (+%d more)", strings.Join(keys[:maxLoggedKeys], ","), len(keys)-maxLoggedKeys)
}

// Get returns (nil, nil) on cache miss.
func (c *Cache) Get(ctx context.Context, key string) ([]byte, error) {
	val, err := c.client.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, cacheErr("get", err)
	}
	return val, nil
}

// MGet returns a same-length slice with nil entries for misses.
func (c *Cache) MGet(ctx context.Context, keys []string) ([][]byte, error) {
	vals, err := c.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, cacheErr("mget", err)
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
	return cacheErr("set", c.client.Set(ctx, key, value, ttl).Err())
}

func (c *Cache) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	ok, err := c.client.SetNX(ctx, key, value, ttl).Result()
	return ok, cacheErr("setnx", err)
}

// Delete is the single choke point for every cache invalidation. A failed
// DEL is not retried (callers ignore or only log the error): the entry is
// served stale until its TTL. It is therefore counted per key type and
// logged once here, so the lost invalidation is visible.
func (c *Cache) Delete(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	err := cacheErr("del", c.client.Del(ctx, keys...).Err())
	if err != nil {
		types := make([]string, 0, len(keys))
		seen := make(map[string]bool, len(keys))
		for _, k := range keys {
			kt := keyType(k)
			if !seen[kt] {
				seen[kt] = true
				types = append(types, kt)
				metrics.IncCacheInvalidationFailed(kt)
			}
		}
		if c.logger != nil {
			c.logger.Warn("valkey: cache invalidation failed — entries may be served stale until TTL expiry", map[string]any{
				"key_types": strings.Join(types, ","),
				"keys":      logKeys(keys),
				"error":     err.Error(),
			})
		}
	}
	return err
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
