// Package valkey — unit tests for the key-builder methods on Cache.
// These are pure string-construction methods; no Redis connection is needed.
package valkey

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// newStubCache creates a Cache with a nil redis client — sufficient for
// testing the key-builder methods which never touch the client.
func newStubCache() *Cache {
	return &Cache{client: nil}
}

func TestCache_DepartmentsKey_Constant(t *testing.T) {
	c := newStubCache()
	assert.Equal(t, "om:departments", c.DepartmentsKey(),
		"DepartmentsKey must return the constant 'om:departments'")
}

func TestCache_DepartmentsStaleKey_Constant(t *testing.T) {
	c := newStubCache()
	assert.Equal(t, "om:departments:stale", c.DepartmentsStaleKey(),
		"DepartmentsStaleKey must return 'om:departments:stale'")
}

func TestCache_PlansStaleKey_Constant(t *testing.T) {
	c := newStubCache()
	assert.Equal(t, "om:plans:stale", c.PlansStaleKey(),
		"PlansStaleKey must return 'om:plans:stale'")
}

func TestCache_PlansKey_Constant(t *testing.T) {
	c := newStubCache()
	assert.Equal(t, "om:plans", c.PlansKey(),
		"PlansKey must return the constant 'om:plans'")
}

func TestCache_MembershipsKey_IncludesBothIDs(t *testing.T) {
	c := newStubCache()
	tenantID := uuid.New()
	userID := uuid.New()

	key := c.MembershipsKey(tenantID, userID)

	assert.True(t, strings.HasPrefix(key, "om:memberships:"),
		"MembershipsKey must start with 'om:memberships:'")
	assert.Contains(t, key, tenantID.String())
	assert.Contains(t, key, userID.String())
}

func TestCache_TenantKey_IncludesTenantID(t *testing.T) {
	c := newStubCache()
	tenantID := uuid.New()

	key := c.TenantKey(tenantID)

	assert.Equal(t, fmt.Sprintf("om:tenant:%s", tenantID), key)
}

func TestCache_MembersPageKey_IncludesLimitSuffix(t *testing.T) {
	c := newStubCache()
	tenantID := uuid.New()

	key := c.MembersPageKey(tenantID, 50)

	assert.True(t, strings.HasSuffix(key, ":50"),
		"MembersPageKey must end with the limit value")
	assert.Contains(t, key, tenantID.String())
}

func TestCache_DeptMembersKey_IncludesBothIDs(t *testing.T) {
	c := newStubCache()
	tenantID := uuid.New()
	deptID := uuid.New()

	key := c.DeptMembersKey(tenantID, deptID)

	assert.True(t, strings.HasPrefix(key, "om:dept_members:"))
	assert.Contains(t, key, tenantID.String())
	assert.Contains(t, key, deptID.String())
}

func TestCache_LocaleKey_Format(t *testing.T) {
	c := newStubCache()
	tenantID := uuid.New()

	key := c.LocaleKey(tenantID)

	assert.Equal(t, fmt.Sprintf("om:locale:%s", tenantID), key)
}

func TestCache_RolesKey_Format(t *testing.T) {
	c := newStubCache()
	tenantID := uuid.New()

	key := c.RolesKey(tenantID)

	assert.Equal(t, fmt.Sprintf("om:roles:%s", tenantID), key)
}

func TestCache_GroupRoleMappingsKey_Format(t *testing.T) {
	c := newStubCache()
	tenantID := uuid.New()

	key := c.GroupRoleMappingsKey(tenantID)

	assert.Equal(t, fmt.Sprintf("om:grm:%s", tenantID), key)
}

func TestCache_GroupDeptMappingsKey_Format(t *testing.T) {
	c := newStubCache()
	tenantID := uuid.New()

	key := c.GroupDeptMappingsKey(tenantID)

	assert.Equal(t, fmt.Sprintf("om:gdm:%s", tenantID), key)
}

func TestCache_SeatUsageKey_Format(t *testing.T) {
	c := newStubCache()
	tenantID := uuid.New()

	key := c.SeatUsageKey(tenantID)

	assert.Equal(t, fmt.Sprintf("om:seat_usage:%s", tenantID), key)
}

// TestCache_KeysAreUnique verifies that different key builders for the same
// tenantID produce distinct strings (no namespace collision).
func TestCache_KeysAreUnique(t *testing.T) {
	c := newStubCache()
	tenantID := uuid.New()

	keys := []string{
		c.TenantKey(tenantID),
		c.LocaleKey(tenantID),
		c.RolesKey(tenantID),
		c.SeatUsageKey(tenantID),
		c.GroupRoleMappingsKey(tenantID),
		c.GroupDeptMappingsKey(tenantID),
		c.MembersPageKey(tenantID, 50),
	}
	seen := map[string]struct{}{}
	for _, k := range keys {
		_, exists := seen[k]
		assert.False(t, exists, "duplicate cache key: %s", k)
		seen[k] = struct{}{}
	}
}
