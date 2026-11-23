//go:build integration

// NEW-CONC-01 (EVT-10): two identical concurrent P-28 calls granting the same
// role must leave exactly one tenant_roles row AND exactly one
// TenantRoleGranted outbox row. Regression guard; whether the race shows on
// a given run depends on timing, so the test logs how many iterations the
// two calls really overlapped in (both reached Grant).
package postgres_test

import (
	"context"
	"sync"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCONC01_ConcurrentIdenticalReconcile_OneRowOneEvent(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	const iterations = 20
	doubleEvents := 0
	for i := 0; i < iterations; i++ {
		tenantID, ownerID := seedTenantWithOwner(t, ctx, fx, "conc01-"+uuid.NewString()[:8])
		member := uuid.New()
		_, err := fx.rawPool.Exec(ctx, `
			INSERT INTO tenant_memberships (tenant_id, user_id, status) VALUES ($1, $2, 'active')`, tenantID, member)
		require.NoError(t, err)

		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				<-start
				_, _, errs[g] = fx.Membership.ReconcileRoles(withSystemAndTenant(ctx, tenantID), tenantID, member,
					[]domain.TenantRoleCode{domain.RoleTenantAdmin}, ownerID)
			}(g)
		}
		close(start)
		wg.Wait()
		for g, e := range errs {
			assert.NoError(t, e, "iteration %d call %d", i, g)
		}

		var rows, events int
		require.NoError(t, fx.rawPool.QueryRow(ctx, `
			SELECT count(*) FROM tenant_roles
			WHERE tenant_id = $1 AND user_id = $2 AND role_code = 'tenant_admin' AND deleted_at IS NULL`,
			tenantID, member).Scan(&rows))
		require.NoError(t, fx.rawPool.QueryRow(ctx, `
			SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantRoleGranted'`,
			tenantID.String()).Scan(&events))
		if events > 1 {
			doubleEvents++
		}
		assert.Equal(t, 1, rows, "iteration %d: TR-2, one role row", i)
		assert.Equal(t, 1, events, "iteration %d: EVT-10, one TenantRoleGranted", i)
	}
	t.Logf("NEW-CONC-01: iterations with 2 TenantRoleGranted events: %d/%d", doubleEvents, iterations)
}
