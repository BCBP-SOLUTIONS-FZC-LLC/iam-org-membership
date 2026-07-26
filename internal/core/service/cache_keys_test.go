package service

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// Cache-key builders mirror the om:* keyspace in LLD §6.1. These tests
// lock the exact key format so an accidental prefix rename fails loudly
// instead of silently invalidating a live cache.

func TestCacheKey_Tenant(t *testing.T) {
	tid := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	assert.Equal(t, "om:tenant:11111111-1111-1111-1111-111111111111", cacheKeyTenant(tid))
}

func TestCacheKey_Locale(t *testing.T) {
	tid := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	assert.Equal(t, "om:locale:22222222-2222-2222-2222-222222222222", cacheKeyLocale(tid))
}

func TestCacheKey_Members(t *testing.T) {
	tid := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	assert.Equal(t, "om:members:33333333-3333-3333-3333-333333333333:50", cacheKeyMembers(tid, 50))
}

func TestCacheKey_MembersEncodesLimit(t *testing.T) {
	// Different limit values must produce different keys — different page
	// sizes are cached independently.
	tid := uuid.New()
	assert.NotEqual(t, cacheKeyMembers(tid, 50), cacheKeyMembers(tid, 100))
	assert.Contains(t, cacheKeyMembers(tid, 100), ":100")
}

func TestCacheKey_Roles(t *testing.T) {
	tid := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	assert.Equal(t, "om:roles:44444444-4444-4444-4444-444444444444", cacheKeyRoles(tid))
}

func TestCacheKey_GRM(t *testing.T) {
	tid := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	assert.Equal(t, "om:grm:55555555-5555-5555-5555-555555555555", cacheKeyGRM(tid))
}

func TestCacheKey_GDM(t *testing.T) {
	tid := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	assert.Equal(t, "om:gdm:66666666-6666-6666-6666-666666666666", cacheKeyGDM(tid))
}

func TestCacheKey_DeptMembers(t *testing.T) {
	tid := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	did := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	assert.Equal(t,
		"om:dept_members:77777777-7777-7777-7777-777777777777:88888888-8888-8888-8888-888888888888",
		cacheKeyDeptMembers(tid, did))
}

func TestCacheKey_SeatUsage(t *testing.T) {
	tid := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	assert.Equal(t, "om:seat_usage:99999999-9999-9999-9999-999999999999", cacheKeySeatUsage(tid))
}

func TestCacheKey_Memberships(t *testing.T) {
	tid := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	uid := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	// This key is the AuthZ Enrichment I-8 hot-path lookup — if the prefix
	// drifts the live cache silently misses forever until manual flush.
	assert.Equal(t,
		"om:memberships:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		cacheKeyMemberships(tid, uid))
}

// Different tenant IDs must yield different keys — a trivial but critical
// guarantee for cache isolation.
func TestCacheKey_DifferentTenantsProduceDistinctKeys(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	assert.NotEqual(t, cacheKeyTenant(a), cacheKeyTenant(b))
	assert.NotEqual(t, cacheKeyRoles(a), cacheKeyRoles(b))
	assert.NotEqual(t, cacheKeySeatUsage(a), cacheKeySeatUsage(b))
}
