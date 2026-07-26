package requestctx

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// ── WithContext + FromContext round-trip ──────────────────────────────

func TestWithContext_RoundTrip(t *testing.T) {
	rc := &RequestContext{UserID: uuid.New(), TenantID: uuid.New()}
	ctx := WithContext(context.Background(), rc)
	got, ok := FromContext(ctx)
	assert.True(t, ok)
	assert.Same(t, rc, got)
}

func TestFromContext_MissingReturnsFalse(t *testing.T) {
	got, ok := FromContext(context.Background())
	assert.False(t, ok)
	assert.Nil(t, got)
}

func TestFromContext_TypedNilPointerReturnsFalse(t *testing.T) {
	// Guard: a nil *RequestContext stored under the key must not surface
	// as (nil, true) — callers would nil-panic on rc.Roles.
	var rc *RequestContext
	ctx := WithContext(context.Background(), rc)
	got, ok := FromContext(ctx)
	assert.False(t, ok, "typed-nil must be treated as absent")
	assert.Nil(t, got)
}

// ── HasRole ────────────────────────────────────────────────────────────

func TestHasRole_MatchesGrantedRole(t *testing.T) {
	rc := &RequestContext{Roles: []string{"tenant_owner", "tender_admin"}}
	assert.True(t, rc.HasRole("tenant_owner"))
	assert.True(t, rc.HasRole("tender_admin"))
}

func TestHasRole_MissingRoleReturnsFalse(t *testing.T) {
	rc := &RequestContext{Roles: []string{"tenant_admin"}}
	assert.False(t, rc.HasRole("platform_operator"))
}

func TestHasRole_EmptyRolesReturnsFalse(t *testing.T) {
	rc := &RequestContext{}
	assert.False(t, rc.HasRole("any"))
}

// ── IsAdmin — AUTH-2 gate for tenant admin mutations ──────────────────

func TestIsAdmin_TrueForTenantOwner(t *testing.T) {
	rc := &RequestContext{Roles: []string{"tenant_owner"}}
	assert.True(t, rc.IsAdmin())
}

func TestIsAdmin_TrueForTenantAdmin(t *testing.T) {
	rc := &RequestContext{Roles: []string{"tenant_admin"}}
	assert.True(t, rc.IsAdmin())
}

func TestIsAdmin_FalseForTenderAdmin(t *testing.T) {
	// tender_admin gates tender ACL (AUTH-3), not the tenant-admin surface.
	rc := &RequestContext{Roles: []string{"tender_admin"}}
	assert.False(t, rc.IsAdmin(),
		"tender_admin must NOT satisfy IsAdmin (AUTH-2 vs AUTH-3)")
}

func TestIsAdmin_FalseForMember(t *testing.T) {
	rc := &RequestContext{Roles: []string{"member"}}
	assert.False(t, rc.IsAdmin())
}

// ── IsOperator — platform_operator gate (AUTH-6) ──────────────────────

func TestIsOperator_TrueForPlatformOperator(t *testing.T) {
	rc := &RequestContext{Roles: []string{"platform_operator"}}
	assert.True(t, rc.IsOperator())
}

func TestIsOperator_FalseForTenantOwner(t *testing.T) {
	rc := &RequestContext{Roles: []string{"tenant_owner"}}
	assert.False(t, rc.IsOperator(),
		"tenant_owner is tenant-scoped; platform-operator is a distinct role")
}

// ── IsSystem — the reserved iam-system principal (RLS-5, IAPI-2) ──────

func TestIsSystem_TrueForIamSystemRole(t *testing.T) {
	rc := &RequestContext{Roles: []string{"iam-system"}}
	assert.True(t, rc.IsSystem())
}

func TestIsSystem_FalseForRegularUser(t *testing.T) {
	rc := &RequestContext{Roles: []string{"tenant_owner", "tenant_admin"}}
	assert.False(t, rc.IsSystem(),
		"regular tenant roles must not impersonate iam-system")
}

func TestIsSystem_FalseWhenNoRoles(t *testing.T) {
	rc := &RequestContext{}
	assert.False(t, rc.IsSystem())
}
