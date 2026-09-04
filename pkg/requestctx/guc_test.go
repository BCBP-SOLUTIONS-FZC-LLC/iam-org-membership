package requestctx

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	pgdomain "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

func TestWithSystemTenant_BindsTenantAndSystemPrincipal(t *testing.T) {
	tenantID := uuid.New()
	ctx := WithSystemTenant(context.Background(), tenantID)

	g, ok := pgcommon.GUCSetFromContext(ctx)
	assert.True(t, ok)
	assert.Equal(t, tenantID.String(), g.TenantID)
	assert.Equal(t, systemPrincipal, g.UserID)
}

func TestWithSystemTenant_OverridesExistingCallerUserID(t *testing.T) {
	// A caller-bound GUCSet (e.g. from an authenticated request) must be
	// overridden to iam-system, not merged/preserved — RLS-5.
	g := pgdomain.GUCSet{TenantID: uuid.New().String(), UserID: "some-caller"}
	ctx := pgcommon.WithGUCSet(context.Background(), g)

	tenantID := uuid.New()
	ctx = WithSystemTenant(ctx, tenantID)

	got, ok := pgcommon.GUCSetFromContext(ctx)
	assert.True(t, ok)
	assert.Equal(t, tenantID.String(), got.TenantID)
	assert.Equal(t, systemPrincipal, got.UserID)
}

func TestWithTenant_BindsTenantLeavesUserIDUntouched(t *testing.T) {
	g := pgdomain.GUCSet{TenantID: uuid.New().String(), UserID: "operator-1"}
	ctx := pgcommon.WithGUCSet(context.Background(), g)

	tenantID := uuid.New()
	ctx = WithTenant(ctx, tenantID)

	got, ok := pgcommon.GUCSetFromContext(ctx)
	assert.True(t, ok)
	assert.Equal(t, tenantID.String(), got.TenantID)
	assert.Equal(t, "operator-1", got.UserID, "WithTenant must not touch UserID")
}

func TestWithTenant_NoExistingGUCSetStillBindsTenant(t *testing.T) {
	tenantID := uuid.New()
	ctx := WithTenant(context.Background(), tenantID)

	got, ok := pgcommon.GUCSetFromContext(ctx)
	assert.True(t, ok)
	assert.Equal(t, tenantID.String(), got.TenantID)
}
