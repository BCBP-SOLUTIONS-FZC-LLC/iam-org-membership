package valkey

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
	// MaxRetries -1 (disabled) is normalised by go-redis to 0 retries; the
	// go-redis default would be 3. DialerRetries 1 = a single dial attempt
	// (go-redis default 5, 100 ms apart) — no retry storm with Valkey down.
	assert.Equal(t, 0, opts.MaxRetries, "command retries must be disabled")
	assert.Equal(t, 1, opts.DialerRetries, "only one dial attempt per command")
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

// TestVC_CACHE_012_GetReturnsErrorOnServerDown covers the error branch in
// Get() at line 55 — `return nil, err` when the server is unavailable and the
// error is NOT redis.Nil (i.e., a genuine transport failure).
func TestVC_CACHE_012_GetReturnsErrorOnServerDown(t *testing.T) {
	c, mr := newTestCache(t)
	ctx := context.Background()

	// Store a key while the server is up.
	require.NoError(t, c.Set(ctx, "key1", []byte("val"), time.Minute))

	// Tear down the server → next call returns a non-redis.Nil error.
	mr.Close()

	got, err := c.Get(ctx, "key1")
	assert.Error(t, err, "transport error must be propagated by Get()")
	assert.Nil(t, got, "nil must be returned alongside the error")
}

// TestVC_CACHE_013_MGetReturnsErrorOnServerDown covers the error branch in
// MGet() at lines 63-65 — `return nil, err` when the server is unavailable.
func TestVC_CACHE_013_MGetReturnsErrorOnServerDown(t *testing.T) {
	c, mr := newTestCache(t)
	ctx := context.Background()

	// Tear down the server before the MGet call.
	mr.Close()

	vals, err := c.MGet(ctx, []string{"k1", "k2"})
	assert.Error(t, err, "transport error must be propagated by MGet()")
	assert.Nil(t, vals, "nil must be returned alongside the error")
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
		{"departments", c.DepartmentsKey(), "om:departments"},
		{"departments-stale", c.DepartmentsStaleKey(), "om:departments:stale"},
		{"plans-stale", c.PlansStaleKey(), "om:plans:stale"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, tc.got, "%s key builder must match §6.1 exactly", tc.name)
	}
}

// ── Valkey outage: fail fast, count and log lost invalidations (Fix 1) ──

var registerMetricsOnce sync.Once

// ensureMetrics initialises gincommon's metric identity and registers the
// service metrics once per test process (same order as cmd/server).
func ensureMetrics() {
	registerMetricsOnce.Do(func() {
		_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
			ServiceName: "iam-org-membership", BuildVersion: "test",
			Domain: "iam", Environment: "test",
		})
		metrics.Register("test")
	})
}

// warnCapture is a port.Logger that records Warn calls.
type warnCapture struct {
	mu    sync.Mutex
	warns []map[string]any
	msgs  []string
}

func (l *warnCapture) Debug(string, map[string]any) {}
func (l *warnCapture) Info(string, map[string]any)  {}
func (l *warnCapture) Error(string, map[string]any) {}
func (l *warnCapture) Warn(msg string, f map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, msg)
	l.warns = append(l.warns, f)
}

// downCache returns a Cache pointed at a Valkey that is no longer listening.
func downCache(t *testing.T) *Cache {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	addr := mr.Addr()
	mr.Close()
	c := New(addr)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestVC_CACHE_FIX1_DeleteFailure_CountsPerKeyTypeAndLogsOnce — a failed
// DEL increments cache_invalidation_failed_total once per key type and
// cache_errors_total{op="del"} once, logs exactly one WARN, and still
// returns the error unchanged to the caller.
func TestVC_CACHE_FIX1_DeleteFailure_CountsPerKeyTypeAndLogsOnce(t *testing.T) {
	ensureMetrics()
	log := &warnCapture{}
	c := downCache(t).WithLogger(log)

	tenantBefore := testutil.ToFloat64(metrics.CacheInvalidationFailed.WithLabelValues("tenant"))
	localeBefore := testutil.ToFloat64(metrics.CacheInvalidationFailed.WithLabelValues("locale"))
	delBefore := testutil.ToFloat64(metrics.CacheErrors.WithLabelValues("del"))

	tid := uuid.New()
	err := c.Delete(context.Background(), "om:tenant:"+tid.String(), "om:locale:"+tid.String())
	require.Error(t, err, "the DEL error is still returned to the caller")

	assert.Equal(t, tenantBefore+1, testutil.ToFloat64(metrics.CacheInvalidationFailed.WithLabelValues("tenant")))
	assert.Equal(t, localeBefore+1, testutil.ToFloat64(metrics.CacheInvalidationFailed.WithLabelValues("locale")))
	assert.Equal(t, delBefore+1, testutil.ToFloat64(metrics.CacheErrors.WithLabelValues("del")))

	require.Len(t, log.warns, 1, "one WARN per failed DEL call")
	assert.Contains(t, log.msgs[0], "cache invalidation failed")
	assert.Equal(t, "tenant,locale", log.warns[0]["key_types"])
	assert.Contains(t, log.warns[0]["keys"], tid.String())
}

// TestVC_CACHE_FIX1_DeleteSuccess_NoCountNoLog — a healthy DEL records
// nothing.
func TestVC_CACHE_FIX1_DeleteSuccess_NoCountNoLog(t *testing.T) {
	ensureMetrics()
	log := &warnCapture{}
	c, _ := newTestCache(t)
	c.WithLogger(log)

	before := testutil.ToFloat64(metrics.CacheInvalidationFailed.WithLabelValues("tenant"))
	require.NoError(t, c.Delete(context.Background(), "om:tenant:"+uuid.NewString()))
	assert.Equal(t, before, testutil.ToFloat64(metrics.CacheInvalidationFailed.WithLabelValues("tenant")))
	assert.Empty(t, log.warns)
}

// TestVC_CACHE_FIX1_GetFailure_CountedMissIsNot — a failed GET counts as a
// cache error; a plain miss does not.
func TestVC_CACHE_FIX1_GetFailure_CountedMissIsNot(t *testing.T) {
	ensureMetrics()

	healthy, _ := newTestCache(t)
	before := testutil.ToFloat64(metrics.CacheErrors.WithLabelValues("get"))
	v, err := healthy.Get(context.Background(), "om:tenant:"+uuid.NewString())
	require.NoError(t, err)
	assert.Nil(t, v)
	assert.Equal(t, before, testutil.ToFloat64(metrics.CacheErrors.WithLabelValues("get")), "a miss is not an error")

	_, err = downCache(t).Get(context.Background(), "om:tenant:"+uuid.NewString())
	require.Error(t, err)
	assert.Equal(t, before+1, testutil.ToFloat64(metrics.CacheErrors.WithLabelValues("get")))
}

// TestVC_CACHE_FIX1_DownValkeyFailsFast — with Valkey not listening each
// command makes one dial attempt and no retries. Before Fix 1 the go-redis
// defaults (4 attempts × 5 dials) made each call take ~1.7 s.
func TestVC_CACHE_FIX1_DownValkeyFailsFast(t *testing.T) {
	ensureMetrics()
	c := downCache(t)
	start := time.Now()
	for i := 0; i < 3; i++ {
		_, _ = c.Get(context.Background(), "om:tenant:"+uuid.NewString())
		_ = c.Set(context.Background(), "om:tenant:"+uuid.NewString(), []byte("x"), time.Minute)
	}
	assert.Less(t, time.Since(start), 500*time.Millisecond, "six calls against a down Valkey must not retry")
}

// TestVC_CACHE_FIX1_KeyType — the key_type label is the om:{type} family,
// bounded to known types.
func TestVC_CACHE_FIX1_KeyType(t *testing.T) {
	id := uuid.NewString()
	cases := map[string]string{
		"om:tenant:" + id:                 "tenant",
		"om:memberships:" + id + ":" + id: "memberships",
		"om:members:" + id + ":50":        "members",
		"om:seat_usage:" + id:             "seat_usage",
		"om:departments":                  "departments",
		"om:departments:stale":            "departments",
		"om:plans:gen:starter":            "plans",
		"om:unknown:" + id:                "other",
		"hello":                           "other",
	}
	for key, want := range cases {
		assert.Equal(t, want, keyType(key), key)
	}
}

// TestVC_CACHE_FIX1B_DeleteFailure_LogsAtMostTenKeys — a failed DEL of 12
// keys logs the first 10 plus "(+2 more)"; the counter still sees every
// key type.
func TestVC_CACHE_FIX1B_DeleteFailure_LogsAtMostTenKeys(t *testing.T) {
	ensureMetrics()
	log := &warnCapture{}
	c := downCache(t).WithLogger(log)

	keys := make([]string, 12)
	for i := range keys {
		keys[i] = "om:tenant:" + uuid.NewString()
	}
	require.Error(t, c.Delete(context.Background(), keys...))

	require.Len(t, log.warns, 1)
	got, ok := log.warns[0]["keys"].(string)
	require.True(t, ok)
	assert.True(t, strings.HasSuffix(got, " (+2 more)"), got)
	for _, k := range keys[:10] {
		assert.Contains(t, got, k)
	}
	for _, k := range keys[10:] {
		assert.NotContains(t, got, k)
	}
	assert.Equal(t, "tenant", log.warns[0]["key_types"])

	assert.Equal(t, strings.Join(keys[:10], ","), logKeys(keys[:10]), "10 keys: no suffix")
}
