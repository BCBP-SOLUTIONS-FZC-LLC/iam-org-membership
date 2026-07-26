//go:build integration

// Phase 15 · concurrency stress. Extends the existing concurrency_test.go
// suite (SEAT-1, TM-13, TM-11 rejoin, PI-1) with the harder races named in
// Test_cover.md Phase 15:
//
//   - P15-JIT-001 — two concurrent JIT-style membership adds for the same
//     (tenant, user) → uq_tm_active_user permits exactly one active row.
//   - P15-ACCEPT-001 — two concurrent invitation accepts for the same
//     invitation → status transition guard permits one.
//   - P15-DEL-CREATE-001 — two concurrent delegation creates by the same
//     delegator → DEL-1 (one active per delegator) enforced by app FOR UPDATE.
//   - P15-DEL-CANCEL-001 — two concurrent cancels of the same delegation
//     with the same record_version → CONC-1 optimistic lock permits one.
//   - P15-B15-EXT-001 — two concurrent dept-membership assigns with
//     different levels → uq_dm_active_membership rejects one.
//   - P15-REC-001 — invitation-expiry reconciler running concurrently with
//     fresh invite inserts must not misclassify a not-yet-expired invite.
//   - P15-OUTBOX-001 — two outbox-runner-style claim queries → SKIP LOCKED
//     guarantees disjoint batches (no duplicate publish under horizontal
//     scale).
//
// Full test-case metadata (Module · Feature · Priority · Severity) lives in
// Reference_doc/Test_metadata_P12_P16.md.
package postgres_test

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/cmd/reconciler/jobs"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// ── P15-JIT-001 ─────────────────────────────────────────────────────────────

// TestP15_JIT_001_ConcurrentSameUserMembershipAdd — two concurrent JIT SAML
// flows for the same (tenant, user) attempt to INSERT a fresh active
// tenant_memberships row. The uq_tm_active_user partial unique
// (WHERE deleted_at IS NULL) must reject the loser; final state has
// exactly ONE active row.
func TestP15_JIT_001_ConcurrentSameUserMembershipAdd(t *testing.T) {
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "jit-001")
	userID := uuid.New()

	const racers = 4
	var wg sync.WaitGroup
	var wins int32
	var conflicts int32
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := rawPool.Exec(ctx, `
				INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
				VALUES (gen_random_uuid(), $1, $2, 'active')`,
				tenantID, userID)
			if err == nil {
				atomic.AddInt32(&wins, 1)
			} else {
				atomic.AddInt32(&conflicts, 1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), wins,
		"P15-JIT-001: exactly one racer must win (uq_tm_active_user)")
	assert.Equal(t, int32(racers-1), conflicts,
		"P15-JIT-001: remaining racers must all hit the partial unique")

	var active int
	require.NoError(t, rawPool.QueryRow(ctx, `
		SELECT count(*) FROM tenant_memberships
		WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL`,
		tenantID, userID).Scan(&active))
	assert.Equal(t, 1, active, "P15-JIT-001: exactly one active membership after race")
}

// ── P15-ACCEPT-001 ──────────────────────────────────────────────────────────

// TestP15_ACCEPT_001_ConcurrentAcceptOfSameInvitation — two racers UPDATE
// the same pending invitation's status to 'accepted'. Guard clause
// `WHERE status = 'pending'` in the transition allows only the first to
// mutate; the second observes zero rows affected.
func TestP15_ACCEPT_001_ConcurrentAcceptOfSameInvitation(t *testing.T) {
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "accept-001")

	invID := uuid.New()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO pending_invitations (id, tenant_id, email, full_name, invited_by, status, expires_at)
		VALUES ($1, $2, 'accept001@example.com', 'Accept Test', gen_random_uuid(), 'pending', now() + interval '7 days')`,
		invID, tenantID)
	require.NoError(t, err)

	const racers = 3
	var wg sync.WaitGroup
	var accepted int32
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ct, err := rawPool.Exec(ctx, `
				UPDATE pending_invitations
				SET status = 'accepted', accepted_at = now()
				WHERE id = $1 AND status = 'pending'`, invID)
			if err == nil && ct.RowsAffected() == 1 {
				atomic.AddInt32(&accepted, 1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), accepted,
		"P15-ACCEPT-001: exactly one racer must have flipped pending→accepted (transition guard)")

	var finalStatus string
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT status FROM pending_invitations WHERE id = $1`, invID).Scan(&finalStatus))
	assert.Equal(t, "accepted", finalStatus, "P15-ACCEPT-001: final status stable at 'accepted'")
}

// ── P15-DEL-CREATE-001 ──────────────────────────────────────────────────────

// TestP15_DEL_CREATE_001_ConcurrentDelegationCreate — two racers try to
// insert a delegation for the same delegator, both scope=all, different
// delegates. DEL-1 (one active per delegator) is enforced at the app
// layer via FOR UPDATE. We simulate that guard here — the losing racer
// must observe an existing active delegation and abort.
func TestP15_DEL_CREATE_001_ConcurrentDelegationCreate(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "del-create-001")

	// Seed 1 delegator + 2 candidate delegates as active members.
	delegator, delegatorMem := uuid.New(), uuid.New()
	delegateA, delegateAMem := uuid.New(), uuid.New()
	delegateB, delegateBMem := uuid.New(), uuid.New()
	for _, seed := range [][3]uuid.UUID{
		{delegatorMem, tenantID, delegator},
		{delegateAMem, tenantID, delegateA},
		{delegateBMem, tenantID, delegateB},
	} {
		_, err := rawPool.Exec(ctx, `
			INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
			VALUES ($1, $2, $3, 'active')`, seed[0], seed[1], seed[2])
		require.NoError(t, err)
	}

	create := func(delegate, delegateMem uuid.UUID) error {
		ctxT := withTenant(ctx, tenantID)
		return pgcommon.RunInTx(ctxT, appPool, pgxTxOpts(), func(ctx context.Context, tx pgxTx) error {
			// DEL-1 guard — advisory tx-lock keyed on (tenant, delegator).
			// Postgres rejects FOR UPDATE on aggregate queries, so we
			// serialize sibling racers with pg_advisory_xact_lock and then
			// do a plain existence check. The lock is auto-released at
			// commit/rollback.
			if _, err := tx.Exec(ctx,
				`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
				tenantID.String()+":"+delegator.String()); err != nil {
				return err
			}
			var existing int
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FROM delegations
				WHERE tenant_id = $1 AND delegator_id = $2
				  AND status = 'active' AND deleted_at IS NULL`,
				tenantID, delegator).Scan(&existing); err != nil {
				return err
			}
			if existing > 0 {
				return errDelegationExists
			}
			_, err := tx.Exec(ctx, `
				INSERT INTO delegations
					(id, tenant_id, delegator_id, delegate_id, delegator_membership_id, delegate_membership_id,
					 scope, starts_at, status)
				VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, 'all', now(), 'active')`,
				tenantID, delegator, delegate, delegatorMem, delegateMem)
			return err
		})
	}

	var wg sync.WaitGroup
	var wins int32
	wg.Add(2)
	go func() {
		defer wg.Done()
		if create(delegateA, delegateAMem) == nil {
			atomic.AddInt32(&wins, 1)
		}
	}()
	go func() {
		defer wg.Done()
		if create(delegateB, delegateBMem) == nil {
			atomic.AddInt32(&wins, 1)
		}
	}()
	wg.Wait()

	assert.Equal(t, int32(1), wins,
		"P15-DEL-CREATE-001: DEL-1 must permit exactly one active delegation per delegator")

	var active int
	require.NoError(t, rawPool.QueryRow(ctx, `
		SELECT count(*) FROM delegations
		WHERE tenant_id = $1 AND delegator_id = $2 AND status = 'active' AND deleted_at IS NULL`,
		tenantID, delegator).Scan(&active))
	assert.Equal(t, 1, active, "P15-DEL-CREATE-001: exactly one active delegation after race")
}

// ── P15-DEL-CANCEL-001 ──────────────────────────────────────────────────────

// TestP15_DEL_CANCEL_001_ConcurrentCancelSameVersion — two racers cancel
// the same delegation with the SAME (stale-after-first) record_version.
// CONC-1 optimistic lock permits one; the other's UPDATE affects 0 rows.
func TestP15_DEL_CANCEL_001_ConcurrentCancelSameVersion(t *testing.T) {
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "del-cancel-001")

	delegator, delegatorMem := uuid.New(), uuid.New()
	delegate, delegateMem := uuid.New(), uuid.New()
	for _, seed := range [][3]uuid.UUID{
		{delegatorMem, tenantID, delegator},
		{delegateMem, tenantID, delegate},
	} {
		_, err := rawPool.Exec(ctx, `
			INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
			VALUES ($1, $2, $3, 'active')`, seed[0], seed[1], seed[2])
		require.NoError(t, err)
	}
	delegationID := uuid.New()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO delegations
			(id, tenant_id, delegator_id, delegate_id, delegator_membership_id, delegate_membership_id,
			 scope, starts_at, status, record_version)
		VALUES ($1, $2, $3, $4, $5, $6, 'all', now(), 'active', 1)`,
		delegationID, tenantID, delegator, delegate, delegatorMem, delegateMem)
	require.NoError(t, err)

	const racers = 3
	var wg sync.WaitGroup
	var succeeded int32
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ct, err := rawPool.Exec(ctx, `
				UPDATE delegations
				SET status = 'cancelled', deleted_at = now(), record_version = record_version + 1
				WHERE id = $1 AND record_version = 1 AND status = 'active'`, delegationID)
			if err == nil && ct.RowsAffected() == 1 {
				atomic.AddInt32(&succeeded, 1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), succeeded,
		"P15-DEL-CANCEL-001: optimistic lock must permit exactly one cancel")

	var status string
	var ver int64
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT status, record_version FROM delegations WHERE id = $1`, delegationID).Scan(&status, &ver))
	assert.Equal(t, "cancelled", status, "final status must be cancelled")
	assert.Equal(t, int64(2), ver, "record_version must have incremented exactly once")
}

// ── P15-B15-EXT-001 ─────────────────────────────────────────────────────────

// TestP15_B15_EXT_001_ConcurrentDeptAssignDifferentLevels — two racers
// PUT the same (tenant, user, dept) at different role_levels. The
// uq_dm_active_membership partial unique lets exactly one INSERT succeed.
func TestP15_B15_EXT_001_ConcurrentDeptAssignDifferentLevels(t *testing.T) {
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "b15-ext-001")

	// tenant_departments requires a departments row + a tenant_departments row.
	deptID := uuid.New()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO departments (id, code, name, is_system, is_active)
		VALUES ($1, 'B15EXT001', 'B15 Test', false, true)`, deptID)
	require.NoError(t, err)
	// tenant_departments PK is (tenant_id, department_id) — no id column.
	_, err = rawPool.Exec(ctx, `
		INSERT INTO tenant_departments (tenant_id, department_id, is_active)
		VALUES ($1, $2, true)`, tenantID, deptID)
	require.NoError(t, err)

	// Seed the target user as a tenant member.
	userID, memID := uuid.New(), uuid.New()
	_, err = rawPool.Exec(ctx, `
		INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
		VALUES ($1, $2, $3, 'active')`, memID, tenantID, userID)
	require.NoError(t, err)

	levels := []string{"preparator", "reviewer", "approver"}
	var wg sync.WaitGroup
	var wins int32
	for _, lvl := range levels {
		lvl := lvl
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := rawPool.Exec(ctx, `
				INSERT INTO dept_memberships
					(id, tenant_id, user_id, department_id, tenant_membership_id, role_level, granted_by)
				VALUES (gen_random_uuid(), $1, $2, $3, $4, $5::dept_role, $2)`,
				tenantID, userID, deptID, memID, lvl)
			if err == nil {
				atomic.AddInt32(&wins, 1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), wins,
		"P15-B15-EXT-001: exactly one dept-membership assign must win (uq_dm_active_membership)")

	var active int
	require.NoError(t, rawPool.QueryRow(ctx, `
		SELECT count(*) FROM dept_memberships
		WHERE tenant_id = $1 AND user_id = $2 AND department_id = $3 AND deleted_at IS NULL`,
		tenantID, userID, deptID).Scan(&active))
	assert.Equal(t, 1, active, "P15-B15-EXT-001: exactly one active dept row")
}

// ── P15-REC-001 ─────────────────────────────────────────────────────────────

// TestP15_REC_001_ExpiryReconcilerVsLiveInvites — while the
// invitation-expiry reconciler is running, a burst of fresh (not-yet-
// expired) invites is inserted. The reconciler must flip ONLY the
// truly-expired rows; the fresh invites must remain 'pending'.
func TestP15_REC_001_ExpiryReconcilerVsLiveInvites(t *testing.T) {
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "rec-001")

	// Seed 5 invites — the BEFORE INSERT trigger insists expires_at is in
	// the future, so we insert future-dated then UPDATE the timestamp
	// backwards (the trigger doesn't fire on UPDATE).
	expiredIDs := make([]uuid.UUID, 5)
	for i := range expiredIDs {
		expiredIDs[i] = uuid.New()
		_, err := rawPool.Exec(ctx, `
			INSERT INTO pending_invitations (id, tenant_id, email, full_name, invited_by, status, expires_at)
			VALUES ($1, $2, $3, 'Expired', gen_random_uuid(), 'pending', now() + interval '1 hour')`,
			expiredIDs[i], tenantID, expiredIDs[i].String()+"@example.com")
		require.NoError(t, err)
		_, err = rawPool.Exec(ctx,
			`UPDATE pending_invitations SET expires_at = now() - interval '1 hour' WHERE id = $1`,
			expiredIDs[i])
		require.NoError(t, err)
	}

	// Spawn the reconciler + a live-insert goroutine concurrently.
	jctx := &jobs.Context{
		SysPool:    rawPool,
		BatchLimit: 100,
		Logger:     slog.Default(),
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, err := jobs.InvitationExpiry(ctx, jctx)
		require.NoError(t, err)
	}()

	freshIDs := make([]uuid.UUID, 5)
	go func() {
		defer wg.Done()
		for i := range freshIDs {
			freshIDs[i] = uuid.New()
			_, err := rawPool.Exec(ctx, `
				INSERT INTO pending_invitations (id, tenant_id, email, full_name, invited_by, status, expires_at)
				VALUES ($1, $2, $3, 'Fresh', gen_random_uuid(), 'pending', now() + interval '7 days')`,
				freshIDs[i], tenantID, freshIDs[i].String()+"@example.com")
			require.NoError(t, err)
		}
	}()
	wg.Wait()

	// Expired rows all flipped.
	var expiredCount int
	require.NoError(t, rawPool.QueryRow(ctx, `
		SELECT count(*) FROM pending_invitations
		WHERE tenant_id = $1 AND status = 'expired'`, tenantID).Scan(&expiredCount))
	assert.Equal(t, 5, expiredCount, "P15-REC-001: all 5 truly-expired must be flipped")

	// Fresh rows untouched.
	var freshCount int
	require.NoError(t, rawPool.QueryRow(ctx, `
		SELECT count(*) FROM pending_invitations
		WHERE tenant_id = $1 AND status = 'pending' AND full_name = 'Fresh'`, tenantID).Scan(&freshCount))
	assert.Equal(t, 5, freshCount,
		"P15-REC-001: fresh invites (expires_at 7d out) must NOT be marked expired")
}

// ── P15-OUTBOX-001 ──────────────────────────────────────────────────────────

// TestP15_OUTBOX_001_SkipLockedPreventsDuplicatePublish — the outbox
// runner claim query uses SELECT … FOR UPDATE SKIP LOCKED. Two concurrent
// claimers must produce DISJOINT batches (no row appears in both). This
// is the horizontal-scale safety property for the runner.
func TestP15_OUTBOX_001_SkipLockedPreventsDuplicatePublish(t *testing.T) {
	_, rawPool := setupTestDB(t)
	ctx := context.Background()

	// Seed 20 outbox_events (unpublished).
	tenantID := seedTenant(t, ctx, rawPool, "outbox-001")
	const total = 20
	seededIDs := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		id := uuid.NewString()
		_, err := rawPool.Exec(ctx, `
			INSERT INTO outbox_events (id, event_type, payload, tenant_id, created_at, scheduled_at)
			VALUES ($1, 'DelegationStarted', '{}'::jsonb, $2, now(), now())`,
			id, tenantID.String())
		require.NoError(t, err)
		seededIDs[id] = true
	}

	// Two concurrent claimers, each opens its own tx and claims up to
	// batchSize rows. Under SKIP LOCKED they can never grab the same row.
	claim := func(batchSize int) []string {
		tx, err := rawPool.BeginTx(ctx, pgx.TxOptions{})
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()

		rows, err := tx.Query(ctx, `
			SELECT id FROM outbox_events
			WHERE published_at IS NULL AND scheduled_at <= now()
			ORDER BY scheduled_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $1`, batchSize)
		require.NoError(t, err)
		defer rows.Close()

		var claimed []string
		for rows.Next() {
			var id string
			require.NoError(t, rows.Scan(&id))
			claimed = append(claimed, id)
		}
		// Hold the transaction until sibling has claimed too — sleep just
		// long enough that the second claimer racing us has time to
		// acquire its own batch.
		time.Sleep(200 * time.Millisecond)
		return claimed
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var claimedA, claimedB []string
	go func() { defer wg.Done(); claimedA = claim(10) }()
	go func() { defer wg.Done(); claimedB = claim(10) }()
	wg.Wait()

	// Disjointness: no id in both batches.
	seen := map[string]bool{}
	for _, id := range claimedA {
		seen[id] = true
	}
	for _, id := range claimedB {
		assert.False(t, seen[id],
			"P15-OUTBOX-001: SKIP LOCKED violated — id %s appears in both batches", id)
	}

	// Combined batch size <= total (never over-claim).
	assert.LessOrEqual(t, len(claimedA)+len(claimedB), total,
		"P15-OUTBOX-001: combined claims must not exceed seeded row count")
}

// ── shared error sentinel ───────────────────────────────────────────────────

var errDelegationExists = &delegationExistsErr{}

type delegationExistsErr struct{}

func (*delegationExistsErr) Error() string { return "P15-DEL-CREATE-001: active delegation already exists" }
