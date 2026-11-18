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

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
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

// bug9AddOwner inserts one more tenant_owner whose membership has the given status.
func bug9AddOwner(t *testing.T, ctx context.Context, fx *testFixtures, tenantID uuid.UUID, status string) {
	t.Helper()
	userID := uuid.New()
	var memID uuid.UUID
	require.NoError(t, fx.rawPool.QueryRow(ctx, `
		INSERT INTO tenant_memberships (tenant_id, user_id, status) VALUES ($1, $2, $3::membership_status) RETURNING id`,
		tenantID, userID, status).Scan(&memID))
	_, err := fx.rawPool.Exec(ctx, `
		INSERT INTO tenant_roles (tenant_id, user_id, tenant_membership_id, role_code, granted_by)
		VALUES ($1, $2, $3, 'tenant_owner', $2)`, tenantID, userID, memID)
	require.NoError(t, err)
}

// bug9SuspendOwner runs P-7 SetStatus(suspended) on the user's current record_version and returns the status after the call and the error.
func bug9SuspendOwner(t *testing.T, ctx context.Context, fx *testFixtures, tenantID, userID uuid.UUID) (string, error) {
	t.Helper()
	var rv int64
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT record_version FROM tenant_memberships WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID).Scan(&rv))
	_, err := fx.Membership.SetStatus(withTenant(ctx, tenantID), tenantID, userID, domain.MembershipSuspended, rv)
	var status string
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT status::text FROM tenant_memberships WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID).Scan(&status))
	return status, err
}

// BUG-9, TM-8: the only active owner cannot be suspended (no other owners at all).
func TestBUG9_SuspendOwner_OneActive_Refused(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerA := seedTenantWithOwner(t, ctx, fx, "bug9-one")

	status, err := bug9SuspendOwner(t, ctx, fx, tenantID, ownerA)
	assert.ErrorIs(t, err, domain.ErrLastOwnerRemoval, "TM-8: last active owner")
	assert.Equal(t, "active", status, "the refused suspend changes nothing")
}

// BUG-9, TM-8 (the regression case): 1 active + 2 suspended owners; suspended owners do not count, so the active one cannot be suspended.
func TestBUG9_SuspendOwner_OneActiveTwoSuspended_Refused(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerA := seedTenantWithOwner(t, ctx, fx, "bug9-one-two")
	bug9AddOwner(t, ctx, fx, tenantID, "suspended")
	bug9AddOwner(t, ctx, fx, tenantID, "suspended")

	status, err := bug9SuspendOwner(t, ctx, fx, tenantID, ownerA)
	assert.ErrorIs(t, err, domain.ErrLastOwnerRemoval, "TM-8: A is the last ACTIVE owner")
	assert.Equal(t, "active", status, "the refused suspend changes nothing")
}

// BUG-9, TM-8: with 2 active owners, suspending one succeeds.
func TestBUG9_SuspendOwner_TwoActive_Succeeds(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerA := seedTenantWithOwner(t, ctx, fx, "bug9-two")
	bug9AddOwner(t, ctx, fx, tenantID, "active")

	status, err := bug9SuspendOwner(t, ctx, fx, tenantID, ownerA)
	require.NoError(t, err)
	assert.Equal(t, "suspended", status)
}

// BUG-9, TM-8: with 2 active + 2 suspended owners, suspending an active one succeeds.
func TestBUG9_SuspendOwner_TwoActiveTwoSuspended_Succeeds(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerA := seedTenantWithOwner(t, ctx, fx, "bug9-two-two")
	bug9AddOwner(t, ctx, fx, tenantID, "active")
	bug9AddOwner(t, ctx, fx, tenantID, "suspended")
	bug9AddOwner(t, ctx, fx, tenantID, "suspended")

	status, err := bug9SuspendOwner(t, ctx, fx, tenantID, ownerA)
	require.NoError(t, err)
	assert.Equal(t, "suspended", status)
}

// dec2SuspendViaI4 runs I-4 SetMembershipStatus(suspended) on the user's current record_version and returns the new status, whether ownerless_since is set, and the error.
func dec2SuspendViaI4(t *testing.T, ctx context.Context, fx *testFixtures, tenantID, userID uuid.UUID) (string, bool, error) {
	t.Helper()
	var rv int64
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT record_version FROM tenant_memberships WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID).Scan(&rv))
	_, err := fx.Provisioning.SetMembershipStatus(withTenant(ctx, tenantID), tenantID, userID, domain.MembershipSuspended, rv)
	var status string
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT status::text FROM tenant_memberships WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID).Scan(&status))
	var ownerless bool
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT ownerless_since IS NOT NULL FROM tenants WHERE id = $1`, tenantID).Scan(&ownerless))
	return status, ownerless, err
}

// DEC-2, TM-12: I-4 suspending the last active owner succeeds, sets ownerless_since and counts one escalation.
// Not parallel: it reads the shared escalation counter.
func TestDEC2_I4SuspendLastActiveOwner_SetsOwnerless(t *testing.T) {
	phase4TestMetricsInit()
	require.NotNil(t, metrics.TenantOwnerlessEscalated)
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerA := seedTenantWithOwner(t, ctx, fx, "dec2-last")
	before := counterValue(metrics.TenantOwnerlessEscalated.WithLabelValues("user_suspended"))

	status, ownerless, err := dec2SuspendViaI4(t, ctx, fx, tenantID, ownerA)
	require.NoError(t, err, "I-4 is never refused: the IdP already disabled the user")
	assert.Equal(t, "suspended", status)
	assert.True(t, ownerless, "TM-12: 0 active owners left → ownerless_since set")
	after := counterValue(metrics.TenantOwnerlessEscalated.WithLabelValues("user_suspended"))
	assert.Equal(t, before+1, after, "one escalation counted")
}

// DEC-2, TM-12: owner A active + owner B suspended; I-4 suspends A → no active owner left → ownerless_since set.
func TestDEC2_I4SuspendActiveOwner_OtherOwnerSuspended_SetsOwnerless(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerA := seedTenantWithOwner(t, ctx, fx, "dec2-one-susp")
	bug9AddOwner(t, ctx, fx, tenantID, "suspended")

	status, ownerless, err := dec2SuspendViaI4(t, ctx, fx, tenantID, ownerA)
	require.NoError(t, err)
	assert.Equal(t, "suspended", status)
	assert.True(t, ownerless, "the suspended owner B does not count")
}

// DEC-2: two active owners; I-4 suspends one → an active owner remains, ownerless_since stays NULL.
func TestDEC2_I4SuspendOneOfTwoActiveOwners_NoOwnerless(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerA := seedTenantWithOwner(t, ctx, fx, "dec2-two")
	bug9AddOwner(t, ctx, fx, tenantID, "active")

	status, ownerless, err := dec2SuspendViaI4(t, ctx, fx, tenantID, ownerA)
	require.NoError(t, err)
	assert.Equal(t, "suspended", status)
	assert.False(t, ownerless)
}

// DEC-2: I-4 suspends a non-owner member → no escalation, ownerless_since stays NULL.
func TestDEC2_I4SuspendNonOwner_NoOwnerless(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "dec2-member")
	userID := uuid.New()
	_, err := fx.Invitation.AddFromRegister(withSystemAndTenant(ctx, tenantID), tenantID, userID, uuid.Nil, "")
	require.NoError(t, err)

	status, ownerless, err := dec2SuspendViaI4(t, ctx, fx, tenantID, userID)
	require.NoError(t, err)
	assert.Equal(t, "suspended", status)
	assert.False(t, ownerless)
}

// CD-54 (LLD §4.2 tenants, SEAT-1/SEAT-3): a P-8 removal frees the seat, so an I-3 plain add that was refused at the cap now succeeds.
func TestCD54_RemovalFreesSeat_ThenI3AddAtCapSucceeds(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerID := seedTenantWithOwner(t, ctx, fx, "cd54-remove")
	userID := uuid.New()
	sys := withSystemAndTenant(ctx, tenantID)
	_, err := fx.Invitation.AddFromRegister(sys, tenantID, userID, uuid.Nil, "")
	require.NoError(t, err)
	// 2 active members of 2 licensed seats: exactly at the cap.
	_, err = fx.rawPool.Exec(ctx, `UPDATE tenants SET licensed_seats = 2 WHERE id = $1`, tenantID)
	require.NoError(t, err)

	newUserID := uuid.New()
	_, err = fx.Invitation.AddFromRegister(sys, tenantID, newUserID, uuid.Nil, "")
	require.ErrorIs(t, err, domain.ErrSeatLimitReached, "SEAT-1: at the cap the I-3 plain add is refused")

	require.NoError(t, fx.Membership.RemoveUser(withTenant(ctx, tenantID), tenantID, userID, ownerID))

	mem, err := fx.Invitation.AddFromRegister(sys, tenantID, newUserID, uuid.Nil, "")
	require.NoError(t, err, "the removed member no longer counts, so the seat is free")
	assert.Equal(t, domain.MembershipActive, mem.Status)
	rows := membershipRows(t, ctx, fx, tenantID, newUserID)
	require.Len(t, rows, 1)
	assert.Equal(t, "active", rows[0].status)
	var active int
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM tenant_memberships WHERE tenant_id = $1 AND status = 'active' AND deleted_at IS NULL`, tenantID).Scan(&active))
	assert.Equal(t, 2, active, "usage is back to 2 of 2 seats")
}

// CD-54 (LLD §4.2 tenants, SEAT-1/SEAT-3): an I-3 plain add on a tenant at exactly its seats is refused; no row, no outbox event.
func TestCD54_I3PlainAddAtCap_RefusedNoRow(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "cd54-i3-cap")
	// 1 active (the owner) of 1 licensed seat: exactly at the cap.
	_, err := fx.rawPool.Exec(ctx, `UPDATE tenants SET licensed_seats = 1 WHERE id = $1`, tenantID)
	require.NoError(t, err)
	var outboxBefore int
	require.NoError(t, fx.rawPool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, tenantID).Scan(&outboxBefore))

	userID := uuid.New()
	_, err = fx.Invitation.AddFromRegister(withSystemAndTenant(ctx, tenantID), tenantID, userID, uuid.Nil, "")
	require.ErrorIs(t, err, domain.ErrSeatLimitReached, "SEAT-1: I-3 plain add at the cap is refused")

	assert.Empty(t, membershipRows(t, ctx, fx, tenantID, userID), "no tenant_memberships row for the refused user")
	var outboxAfter int
	require.NoError(t, fx.rawPool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, tenantID).Scan(&outboxAfter))
	assert.Equal(t, outboxBefore, outboxAfter, "no outbox row is written for a refused add")
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
