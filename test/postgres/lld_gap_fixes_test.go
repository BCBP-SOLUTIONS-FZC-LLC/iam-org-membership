//go:build integration

// Real-database tests for the code-vs-LLD fixes of 2026-11-11 (LLD rev 2.52):
// BUG-3 / BUG-4 in PgBouncer (simple-protocol) mode, GAP-16 removal keeps
// the row as 'left' and a re-add reactivates it, GAP-22 level change in
// place, GAP-24 concurrent duplicate I-3, BUG-6 concurrent last-owner
// removals, GAP-18 / GAP-7 seat-overage re-evaluation.
package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// simpleProtocolPool is the app pool as PgBouncer mode builds it
// (QueryExecModeSimpleProtocol), the mode every shipped config runs in.
func simpleProtocolPool(t *testing.T) (*pgcommon.Pool, *testFixtures) {
	t.Helper()
	appPool, rawPool, _ := setupTestDBWithAppConfig(t, func(c *pgcommon.Config) { c.PGBouncerMode = true })
	return appPool, &testFixtures{rawPool: rawPool}
}

// BUG-3: Park writes the jsonb envelope in simple-protocol mode (it failed
// with 22P02 when the []byte went out as bytea).
func TestLLDFix_BUG3_ParkWorksInSimpleProtocol(t *testing.T) {
	t.Parallel()
	appPool, fx := simpleProtocolPool(t)
	ctx := context.Background()
	tenantID := uuid.New() // parked events are for a tenant with no row yet
	env, err := json.Marshal(map[string]any{"id": uuid.NewString(), "type": "TenantConverted"})
	require.NoError(t, err)

	repo := pgadapter.NewParkedTenantEventRepository(appPool)
	tctx := withTenant(ctx, tenantID)
	require.NoError(t, repo.Park(tctx, port.ParkedTenantEvent{
		EventID: uuid.New(), TenantID: tenantID, EventType: "TenantConverted",
		OccurredAt: time.Now().UTC(), Envelope: env,
	}))
	got, err := repo.ListForTenant(tctx, tenantID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.JSONEq(t, string(env), string(got[0].Envelope))
	_ = fx
}

// BUG-4: the P-4 batch lookups (user_id = ANY($2)) work in simple-protocol mode.
func TestLLDFix_BUG4_ListByUsersWorksInSimpleProtocol(t *testing.T) {
	t.Parallel()
	appPool, fx := simpleProtocolPool(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, fx.rawPool, "bug4-simple")
	userID := uuid.New()
	var memID uuid.UUID
	require.NoError(t, fx.rawPool.QueryRow(ctx, `
		INSERT INTO tenant_memberships (tenant_id, user_id, status) VALUES ($1, $2, 'active') RETURNING id`,
		tenantID, userID).Scan(&memID))
	_, err := fx.rawPool.Exec(ctx, `
		INSERT INTO tenant_roles (tenant_id, user_id, tenant_membership_id, role_code, granted_by)
		VALUES ($1, $2, $3, 'tenant_admin', $2)`, tenantID, userID, memID)
	require.NoError(t, err)

	tctx := withTenant(ctx, tenantID)
	roles, err := pgadapter.NewTenantRoleRepository(appPool).ListByUsers(tctx, tenantID, []uuid.UUID{userID, uuid.New()})
	require.NoError(t, err)
	require.Len(t, roles, 1)
	assert.Equal(t, domain.RoleTenantAdmin, roles[0].RoleCode)

	_, err = pgadapter.NewDeptMembershipRepository(appPool).ListByUsers(tctx, tenantID, []uuid.UUID{userID})
	require.NoError(t, err)
}

func membershipRows(t *testing.T, ctx context.Context, fx *testFixtures, tenantID, userID uuid.UUID) []struct {
	id      uuid.UUID
	status  string
	deleted bool
} {
	t.Helper()
	rows, err := fx.rawPool.Query(ctx, `
		SELECT id, status::text, deleted_at IS NOT NULL FROM tenant_memberships
		WHERE tenant_id = $1 AND user_id = $2 ORDER BY created_at`, tenantID, userID)
	require.NoError(t, err)
	defer rows.Close()
	var out []struct {
		id      uuid.UUID
		status  string
		deleted bool
	}
	for rows.Next() {
		var r struct {
			id      uuid.UUID
			status  string
			deleted bool
		}
		require.NoError(t, rows.Scan(&r.id, &r.status, &r.deleted))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

// GAP-16, TM-1/TM-4/TM-5: P-8 keeps the row as status='left' with
// deleted_at NULL; the user is no longer a member; an I-3 re-add
// reactivates the same row (one row, same id).
func TestLLDFix_GAP16_RemovalKeepsLeftRow_ReAddReactivates(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerID := seedTenantWithOwner(t, ctx, fx, "gap16-left")
	userID := uuid.New()
	sys := withSystemAndTenant(ctx, tenantID)

	_, err := fx.Invitation.AddFromRegister(sys, tenantID, userID, uuid.Nil, "")
	require.NoError(t, err)
	before := membershipRows(t, ctx, fx, tenantID, userID)
	require.Len(t, before, 1)

	require.NoError(t, fx.Membership.RemoveUser(withTenant(ctx, tenantID), tenantID, userID, ownerID))
	after := membershipRows(t, ctx, fx, tenantID, userID)
	require.Len(t, after, 1, "TM-4: the row is retained")
	assert.Equal(t, "left", after[0].status)
	assert.False(t, after[0].deleted, "TM-5: deleted_at is reserved for the GDPR wipe")

	_, err = fx.Memberships.FindByUserID(withTenant(ctx, tenantID), tenantID, userID)
	assert.ErrorIs(t, err, domain.ErrMemberNotFound, "TM-9: a left member is not a member")
	_, err = fx.AuthZ.GetMembership(withTenant(ctx, tenantID), tenantID, userID)
	assert.Error(t, err, "I-8 does not project a left member")

	mem, err := fx.Invitation.AddFromRegister(sys, tenantID, userID, uuid.Nil, "")
	require.NoError(t, err, "re-add of a removed user succeeds")
	assert.Equal(t, domain.MembershipActive, mem.Status)
	final := membershipRows(t, ctx, fx, tenantID, userID)
	require.Len(t, final, 1, "TM-1: the existing row is reactivated, not a second row")
	assert.Equal(t, before[0].id, final[0].id)
	assert.Equal(t, "active", final[0].status)
}

// GAP-24 / PI-10: two concurrent identical I-3 adds → one succeeds, the
// other is 409 member_already_exists; exactly one row.
func TestLLDFix_GAP24_ConcurrentI3_OneCreated_OneConflict(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "gap24-conc")
	userID := uuid.New()
	sys := withSystemAndTenant(ctx, tenantID)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = fx.Invitation.AddFromRegister(sys, tenantID, userID, uuid.Nil, "")
		}(i)
	}
	wg.Wait()
	ok, conflict := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, domain.ErrMemberAlreadyExists):
			conflict++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, ok)
	assert.Equal(t, 1, conflict)
	assert.Len(t, membershipRows(t, ctx, fx, tenantID, userID), 1)
}

// GAP-22, DM-3: a P-10 level change updates the existing row (same id).
func TestLLDFix_GAP22_LevelChangeUpdatesInPlace(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "gap22-level")
	userID := uuid.New()
	var memID uuid.UUID
	require.NoError(t, fx.rawPool.QueryRow(ctx, `
		INSERT INTO tenant_memberships (tenant_id, user_id, status) VALUES ($1, $2, 'active') RETURNING id`,
		tenantID, userID).Scan(&memID))
	deptID := uuid.New()
	_, err := fx.rawPool.Exec(ctx,
		`INSERT INTO tenant_departments (tenant_id, department_id, is_active) VALUES ($1, $2, true)`, tenantID, deptID)
	require.NoError(t, err)
	tctx := withTenant(ctx, tenantID)

	first, _, err := fx.DeptMems.Assign(tctx, tenantID, userID, deptID, memID, domain.DeptReviewer, uuid.New())
	require.NoError(t, err)
	second, prev, err := fx.DeptMems.Assign(tctx, tenantID, userID, deptID, memID, domain.DeptApprover, uuid.New())
	require.NoError(t, err)
	require.NotNil(t, prev)
	assert.Equal(t, domain.DeptReviewer, prev.RoleLevel)
	assert.Equal(t, first.ID, second.ID, "same row, updated in place")
	assert.Equal(t, domain.DeptApprover, second.RoleLevel)

	var n int
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM dept_memberships WHERE tenant_id=$1 AND user_id=$2 AND department_id=$3`,
		tenantID, userID, deptID).Scan(&n))
	assert.Equal(t, 1, n, "no second row, no soft-deleted copy")
}

// BUG-6, TM-12/TM-13: removing the last two owners concurrently (I-5) leaves
// ownerless_since set — the tenant row lock serializes the owner count.
func TestLLDFix_BUG6_ConcurrentLastOwnersRemoved_OwnerlessSet(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	for round := 0; round < 5; round++ {
		tenantID, ownerA := seedTenantWithOwner(t, ctx, fx, "bug6-"+uuid.NewString()[:8])
		ownerB := uuid.New()
		var memID uuid.UUID
		require.NoError(t, fx.rawPool.QueryRow(ctx, `
			INSERT INTO tenant_memberships (tenant_id, user_id, status) VALUES ($1, $2, 'active') RETURNING id`,
			tenantID, ownerB).Scan(&memID))
		_, err := fx.rawPool.Exec(ctx, `
			INSERT INTO tenant_roles (tenant_id, user_id, tenant_membership_id, role_code, granted_by)
			VALUES ($1, $2, $3, 'tenant_owner', $2)`, tenantID, ownerB, memID)
		require.NoError(t, err)

		var wg sync.WaitGroup
		for _, u := range []uuid.UUID{ownerA, ownerB} {
			wg.Add(1)
			go func(u uuid.UUID) {
				defer wg.Done()
				assert.NoError(t, fx.Provisioning.DeleteMember(withTenant(ctx, tenantID), tenantID, u))
			}(u)
		}
		wg.Wait()
		var ownerless bool
		require.NoError(t, fx.rawPool.QueryRow(ctx,
			`SELECT ownerless_since IS NOT NULL FROM tenants WHERE id = $1`, tenantID).Scan(&ownerless))
		assert.True(t, ownerless, "round %d: ownerless_since must be set once no owner remains", round)
	}
}

// GAP-18, SEAT-5: a member removal that brings usage back under the cap
// clears overage_since and writes TenantSeatOverageResolved.
func TestLLDFix_GAP18_RemovalResolvesOverage(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerID := seedTenantWithOwner(t, ctx, fx, "gap18-remove")
	userID := uuid.New()
	sys := withSystemAndTenant(ctx, tenantID)
	_, err := fx.Invitation.AddFromRegister(sys, tenantID, userID, uuid.Nil, "")
	require.NoError(t, err)
	// 2 active members, 1 licensed seat, already in overage.
	_, err = fx.rawPool.Exec(ctx, `UPDATE tenants SET licensed_seats = 1, overage_since = now() - interval '1 day' WHERE id = $1`, tenantID)
	require.NoError(t, err)

	require.NoError(t, fx.Membership.RemoveUser(withTenant(ctx, tenantID), tenantID, userID, ownerID))

	var cleared bool
	require.NoError(t, fx.rawPool.QueryRow(ctx, `SELECT overage_since IS NULL FROM tenants WHERE id = $1`, tenantID).Scan(&cleared))
	assert.True(t, cleared, "usage 1 ≤ 1 seat → overage_since cleared")
	var events int
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantSeatOverageResolved'`, tenantID).Scan(&events))
	assert.Equal(t, 1, events)
}

// SEAT-5: an I-3 plain add on a tenant at exactly its seats is not refused
// (no seat cap on I-3), but it starts the overage: overage_since is set and
// one TenantSeatOverageStarted is written in the same transaction.
func TestLLDFix_SEAT5_I3PlainAddOverCapStartsOverage(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "seat5-i3-add")
	// 1 active (the owner) of 1 licensed seat: exactly at cap, no marker.
	_, err := fx.rawPool.Exec(ctx, `UPDATE tenants SET licensed_seats = 1, overage_since = NULL WHERE id = $1`, tenantID)
	require.NoError(t, err)

	userID := uuid.New()
	mem, err := fx.Invitation.AddFromRegister(withSystemAndTenant(ctx, tenantID), tenantID, userID, uuid.Nil, "")
	require.NoError(t, err, "I-3 plain add is not seat-capped")
	assert.Equal(t, domain.MembershipActive, mem.Status)

	var started bool
	require.NoError(t, fx.rawPool.QueryRow(ctx, `SELECT overage_since IS NOT NULL FROM tenants WHERE id = $1`, tenantID).Scan(&started))
	assert.True(t, started, "usage 2 > 1 seat → overage_since set")
	var events int
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantSeatOverageStarted'`, tenantID).Scan(&events))
	assert.Equal(t, 1, events)
}

// §15.2.2 / TM-5: an I-5 identity deletion after a P-8 removal stamps
// deleted_at on the 'left' row (it used to be a silent no-op).
func TestLLDFix_I5AfterP8_StampsDeletedAt(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerID := seedTenantWithOwner(t, ctx, fx, "i5-after-p8")
	userID := uuid.New()
	_, err := fx.Invitation.AddFromRegister(withSystemAndTenant(ctx, tenantID), tenantID, userID, uuid.Nil, "")
	require.NoError(t, err)
	require.NoError(t, fx.Membership.RemoveUser(withTenant(ctx, tenantID), tenantID, userID, ownerID))

	require.NoError(t, fx.Provisioning.DeleteMember(withTenant(ctx, tenantID), tenantID, userID))
	rows := membershipRows(t, ctx, fx, tenantID, userID)
	require.Len(t, rows, 1)
	assert.Equal(t, "left", rows[0].status)
	assert.True(t, rows[0].deleted, "identity deleted → deleted_at set")

	// A retry is still a no-op.
	require.NoError(t, fx.Provisioning.DeleteMember(withTenant(ctx, tenantID), tenantID, userID))
}

// TM-12: a USER_DELETE for an offboarded (soft-deleted) tenant is an
// idempotent no-op, not a 404.
func TestLLDFix_I5_OffboardedTenant_NoOp(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerID := seedTenantWithOwner(t, ctx, fx, "i5-offboarded")
	_, err := fx.rawPool.Exec(ctx, `UPDATE tenants SET deleted_at = now() WHERE id = $1`, tenantID)
	require.NoError(t, err)
	assert.NoError(t, fx.Provisioning.DeleteMember(withTenant(ctx, tenantID), tenantID, ownerID))
}
