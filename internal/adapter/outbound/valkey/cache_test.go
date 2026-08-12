package valkey

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 18 · 0%-units sweep — valkey/cache.go was at 0% direct coverage.
// Tests run against miniredis (pure-Go in-memory Redis stand-in) so they
// stay in the fast unit tier — no Docker, no network.

// newTestCache spins up an in-memory Redis and returns a Cache pointed at
// it, plus a cleanup func. Aggressive default timeouts (50 ms) are
// preserved so a real production timeout regression would surface here.
func newTestCache(t *testing.T) (*Cache, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	c := New(mr.Addr())
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

// TestVC_CACHE_001_NewParsesAddr — plain "host:port" and full
// "redis://..." both construct a working client.
func TestVC_CACHE_001_NewParsesAddr(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	// Plain addr.
	c1 := New(mr.Addr())
	require.NoError(t, c1.Health(context.Background()))
	_ = c1.Close()

	// redis:// URL form.
	c2 := New("redis://" + mr.Addr())
	require.NoError(t, c2.Health(context.Background()))
	_ = c2.Close()
}

// TestVC_CACHE_002_TimeoutsHaveTightDefaults — the cache is
// advisory-only; a slow Valkey must degrade fast rather than stall the
// request. Regression guard for the 50 ms defaults.
func TestVC_CACHE_002_TimeoutsHaveTightDefaults(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	c := New(mr.Addr())
	defer func() { _ = c.Close() }()

	opts := c.client.Options()
	assert.Equal(t, 100*time.Millisecond, opts.DialTimeout, "DialTimeout default must stay tight")
	assert.Equal(t, 50*time.Millisecond, opts.ReadTimeout, "ReadTimeout default must stay tight")
	assert.Equal(t, 50*time.Millisecond, opts.WriteTimeout, "WriteTimeout default must stay tight")
}

// TestVC_CACHE_003_GetSetRoundtrip — basic write-then-read.
func TestVC_CACHE_003_GetSetRoundtrip(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	require.NoError(t, c.Set(ctx, "hello", []byte("world"), time.Minute))
	got, err := c.Get(ctx, "hello")
	require.NoError(t, err)
	assert.Equal(t, []byte("world"), got)
}

// TestVC_CACHE_004_GetMissReturnsNilNil — the port.Cache contract:
// a miss returns (nil, nil), not (nil, redis.Nil). Downstream callers
// switch on nil to fall through to Postgres.
func TestVC_CACHE_004_GetMissReturnsNilNil(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	got, err := c.Get(ctx, "never-set")
	require.NoError(t, err, "miss must NOT surface redis.Nil")
	assert.Nil(t, got, "miss must return nil bytes")
}

// TestVC_CACHE_005_MGetHandlesMissesAndHits — MGet returns a
// same-length slice; missing keys sit as nil entries.
func TestVC_CACHE_005_MGetHandlesMissesAndHits(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	require.NoError(t, c.Set(ctx, "k1", []byte("v1"), time.Minute))
	require.NoError(t, c.Set(ctx, "k3", []byte("v3"), time.Minute))

	vals, err := c.MGet(ctx, []string{"k1", "k2-miss", "k3"})
	require.NoError(t, err)
	require.Len(t, vals, 3)
	assert.Equal(t, []byte("v1"), vals[0])
	assert.Nil(t, vals[1], "miss must be a nil entry")
	assert.Equal(t, []byte("v3"), vals[2])
}

// TestVC_CACHE_006_SetNXFirstWinsSecondFalses — SETNX (set-if-not-
// exists) semantics preserved end-to-end.
func TestVC_CACHE_006_SetNXFirstWinsSecondFalses(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	ok1, err := c.SetNX(ctx, "lease", []byte("first"), time.Minute)
	require.NoError(t, err)
	assert.True(t, ok1, "first SetNX must win")

	ok2, err := c.SetNX(ctx, "lease", []byte("second"), time.Minute)
	require.NoError(t, err)
	assert.False(t, ok2, "second SetNX on existing key must not win")

	got, err := c.Get(ctx, "lease")
	require.NoError(t, err)
	assert.Equal(t, []byte("first"), got, "value must be unchanged by the losing SetNX")
}

// TestVC_CACHE_007_DeleteEmptyKeysIsNoOp — Delete() with no args
// short-circuits without hitting the client (regression guard for a
// change that would silently DEL nothing but still spend an RTT).
func TestVC_CACHE_007_DeleteEmptyKeysIsNoOp(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	assert.NoError(t, c.Delete(ctx), "Delete with zero keys must be a no-op, not an error")
}

// TestVC_CACHE_008_DeleteMultipleKeys — variadic Delete removes all
// listed keys in one call.
func TestVC_CACHE_008_DeleteMultipleKeys(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	for _, k := range []string{"a", "b", "c"} {
		require.NoError(t, c.Set(ctx, k, []byte(k), time.Minute))
	}
	require.NoError(t, c.Delete(ctx, "a", "b", "c"))

	for _, k := range []string{"a", "b", "c"} {
		got, err := c.Get(ctx, k)
		require.NoError(t, err)
		assert.Nil(t, got, "key %q must be gone after Delete", k)
	}
}

// TestVC_CACHE_009_TTLHonored — Set with TTL, fast-forward miniredis
// clock past TTL, key must have expired.
func TestVC_CACHE_009_TTLHonored(t *testing.T) {
	c, mr := newTestCache(t)
	ctx := context.Background()

	require.NoError(t, c.Set(ctx, "short", []byte("bye"), 30*time.Second))

	// Immediately visible.
	got, err := c.Get(ctx, "short")
	require.NoError(t, err)
	assert.Equal(t, []byte("bye"), got)

	// Fast-forward miniredis's internal clock past TTL.
	mr.FastForward(31 * time.Second)

	got, err = c.Get(ctx, "short")
	require.NoError(t, err)
	assert.Nil(t, got, "key must be expired after TTL")
}

// TestVC_CACHE_010_HealthWhenServerDown — Health returns an error
// once the underlying server is torn down. Drives /readyz to report
// cache=down.
func TestVC_CACHE_010_HealthWhenServerDown(t *testing.T) {
	c, mr := newTestCache(t)
	ctx := context.Background()

	require.NoError(t, c.Health(ctx), "healthy on a live server")
	mr.Close()

	assert.Error(t, c.Health(ctx), "unhealthy after server torn down")
}

// TestVC_CACHE_011_KeyBuildersFormatCorrectly — every cache-key
// builder returns the exact `om:...` shape §6.1 mandates. Renames here
// would silently invalidate every cached entry.
func TestVC_CACHE_011_KeyBuildersFormatCorrectly(t *testing.T) {
	c, _ := newTestCache(t)
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	user := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	dept := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	cases := []struct {
		name, got, want string
	}{
		{"memberships", c.MembershipsKey(tenant, user), "om:memberships:11111111-1111-1111-1111-111111111111:22222222-2222-2222-2222-222222222222"},
		{"tenant", c.TenantKey(tenant), "om:tenant:11111111-1111-1111-1111-111111111111"},
		{"members-page-50", c.MembersPageKey(tenant, 50), "om:members:11111111-1111-1111-1111-111111111111:50"},
		{"dept-members", c.DeptMembersKey(tenant, dept), "om:dept_members:11111111-1111-1111-1111-111111111111:33333333-3333-3333-3333-333333333333"},
		{"locale", c.LocaleKey(tenant), "om:locale:11111111-1111-1111-1111-111111111111"},
		{"roles", c.RolesKey(tenant), "om:roles:11111111-1111-1111-1111-111111111111"},
		{"grm", c.GroupRoleMappingsKey(tenant), "om:grm:11111111-1111-1111-1111-111111111111"},
		{"gdm", c.GroupDeptMappingsKey(tenant), "om:gdm:11111111-1111-1111-1111-111111111111"},
		{"seat-usage", c.SeatUsageKey(tenant), "om:seat_usage:11111111-1111-1111-1111-111111111111"},
		{"plans", c.PlansKey(), "om:plans"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, tc.got, "%s key builder must match §6.1 exactly", tc.name)
	}
}
