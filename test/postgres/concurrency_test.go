//go:build integration

// Concurrency + rejoin invariants (Phase 7 hardening):
//
//   - SEAT-1: transactional seat cap under concurrent invites — exactly one
//     of N racers wins at the cap boundary.
//   - TM-13: concurrent P-28 last-owner stripping — the DB-level TM-8
//     guard (or its serializable projection) ensures at most one goroutine
//     removes the last tenant_owner. We assert that the tenant always
//     retains at least one active owner row after the race resolves.
//   - TM-11 rejoin: soft-leave a tenant_memberships row (status='left' +
//     deleted_at set), then re-add — the partial unique index
//     (uq_tm_active_user WHERE deleted_at IS NULL) permits it.
package postgres_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSEAT1_ConcurrencyRace — SEAT-1 transactional cap. 5 racers, seats=3
// existing active + we test one seat left. Exactly one of the 3 concurrent
// invite-inserts should succeed; the others should get a partial-unique
// violation OR (in stricter mode) fail the count-then-insert atomically
// under SERIALIZABLE / advisory locks.
//
// This test approximates the SEAT-1 semantics using a raw SQL approach:
// each racer opens a tx, SELECTs the tenants row FOR UPDATE (the very
// lock that seat-usage relies on in production), computes active+pending,
// and inserts iff strictly under cap. FOR UPDATE serializes; only one
// racer's read observes "under cap" and inserts.
func TestSEAT1_ConcurrencyRace(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "seat-test")

	// Set licensed_seats = 3 and seed 2 active members so exactly 1 seat is free.
	_, err := rawPool.Exec(ctx, `UPDATE tenants SET licensed_seats = 3 WHERE id = $1`, tenantA)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		userID := uuid.New()
		_, err := rawPool.Exec(ctx, `
			INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
			VALUES (gen_random_uuid(), $1, $2, 'active')`,
			tenantA, userID)
		require.NoError(t, err)
	}

	const racers = 5
	var succeeded int32
	var wg sync.WaitGroup

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			userID := uuid.New()
			ctxA := withTenant(ctx, tenantA)
			err := pgcommon.RunInTx(ctxA, appPool, pgxTxOpts(), func(ctx context.Context, tx pgxTx) error {
				// FOR UPDATE serializes with sibling racers.
				var licensed int
				if err := tx.QueryRow(ctx,
					`SELECT licensed_seats FROM tenants WHERE id = $1 FOR UPDATE`, tenantA).Scan(&licensed); err != nil {
					return err
				}
				var active int
				if err := tx.QueryRow(ctx, `
					SELECT count(*) FROM tenant_memberships
					WHERE tenant_id = $1 AND deleted_at IS NULL AND status = 'active'`, tenantA).Scan(&active); err != nil {
					return err
				}
				if active >= licensed {
					return fmt.Errorf("seat_limit_reached (racer %d saw %d/%d)", idx, active, licensed)
				}
				_, err := tx.Exec(ctx, `
					INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
					VALUES (gen_random_uuid(), $1, $2, 'active')`, tenantA, userID)
				return err
			})
			if err == nil {
				atomic.AddInt32(&succeeded, 1)
			}
		}(i)
	}
	wg.Wait()

	// Exactly one racer should have won the last seat.
	assert.Equal(t, int32(1), succeeded, "SEAT-1: expected exactly 1 racer to win the last seat")

	// Final state: active count == licensed_seats.
	var final int
	require.NoError(t, rawPool.QueryRow(ctx, `
		SELECT count(*) FROM tenant_memberships
		WHERE tenant_id = $1 AND deleted_at IS NULL AND status = 'active'`, tenantA).Scan(&final))
	assert.Equal(t, 3, final, "SEAT-1: active count must equal licensed_seats after race")
}

// TestTM13_ConcurrentLastOwnerRemoval — 2 concurrent goroutines both try
// to soft-delete the last active tenant_owner row. The DB has no direct
// CHECK preventing this (TM-8 is service-layer). What TM-13 asserts is
// that the tenant never ends up in an ownerless state without the
// ownerless_since marker.
//
// We simulate the service-layer guard: each goroutine, inside a tx, counts
// active owners and only revokes if count > 1. Under FOR UPDATE this
// serializes so exactly one goroutine sees count>1 (or both see count=1
// and both back off, which is also correct).
func TestTM13_ConcurrentLastOwnerRemoval(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "tm13-test")

	// Seed 2 owners.
	user1, user2 := uuid.New(), uuid.New()
	mem1, mem2 := uuid.New(), uuid.New()
	for i, pair := range [][2]uuid.UUID{{mem1, user1}, {mem2, user2}} {
		memID, userID := pair[0], pair[1]
		_, err := rawPool.Exec(ctx, `
			INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
			VALUES ($1, $2, $3, 'active')`, memID, tenantA, userID)
		require.NoError(t, err, "seed membership %d", i)
		_, err = rawPool.Exec(ctx, `
			INSERT INTO tenant_roles (tenant_id, user_id, tenant_membership_id, role_code, granted_by)
			VALUES ($1, $2, $3, 'tenant_owner', $2)`, tenantA, userID, memID)
		require.NoError(t, err, "seed owner grant %d", i)
	}

	// Two racers each try to strip THEIR OWN owner role.
	type outcome struct {
		userID  uuid.UUID
		removed bool
	}
	outcomes := make([]outcome, 2)
	var wg sync.WaitGroup
	for i, uid := range []uuid.UUID{user1, user2} {
		wg.Add(1)
		go func(idx int, userID uuid.UUID) {
			defer wg.Done()
			ctxA := withTenant(ctx, tenantA)
			_ = pgcommon.RunInTx(ctxA, appPool, pgxTxOpts(), func(ctx context.Context, tx pgxTx) error {
				var ownerCount int
				if err := tx.QueryRow(ctx, `
					SELECT count(*) FROM tenant_roles
					WHERE tenant_id = $1 AND role_code = 'tenant_owner' AND deleted_at IS NULL
					FOR UPDATE`, tenantA).Scan(&ownerCount); err != nil {
					return err
				}
				if ownerCount <= 1 {
					// TM-8 guard fires here — don't remove.
					outcomes[idx] = outcome{userID: userID, removed: false}
					return nil
				}
				_, err := tx.Exec(ctx, `
					UPDATE tenant_roles SET deleted_at = now()
					WHERE tenant_id = $1 AND user_id = $2 AND role_code = 'tenant_owner' AND deleted_at IS NULL`,
					tenantA, userID)
				if err != nil {
					return err
				}
				outcomes[idx] = outcome{userID: userID, removed: true}
				return nil
			})
		}(i, uid)
	}
	wg.Wait()

	// Post-race check: at least one owner must remain.
	var remaining int
	require.NoError(t, rawPool.QueryRow(ctx, `
		SELECT count(*) FROM tenant_roles
		WHERE tenant_id = $1 AND role_code = 'tenant_owner' AND deleted_at IS NULL`, tenantA).Scan(&remaining))
	assert.GreaterOrEqual(t, remaining, 1, "TM-13: at least one active tenant_owner must survive")

	// Correctness: no more than one of the two racers should have removed
	// their role (because the second racer's FOR UPDATE sees count=1 and
	// backs off).
	removedCount := 0
	for _, o := range outcomes {
		if o.removed {
			removedCount++
		}
	}
	assert.LessOrEqual(t, removedCount, 1, "TM-13: at most one racer may remove their owner role")
}

// TestTM11_RejoinAfterSoftLeave — soft-delete a membership (simulating a
// user who left), then re-insert a new active row for the same
// (tenant_id, user_id). The partial unique index uq_tm_active_user
// (WHERE deleted_at IS NULL) must permit this because the old row's
// deleted_at is set.
func TestTM11_RejoinAfterSoftLeave(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "tm11-test")

	userID := uuid.New()
	firstMem := uuid.New()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
		VALUES ($1, $2, $3, 'active')`, firstMem, tenantA, userID)
	require.NoError(t, err)

	// Soft-delete: status='left', deleted_at=now(). This is what
	// MembershipRepository.SoftDelete does.
	_, err = rawPool.Exec(ctx, `
		UPDATE tenant_memberships SET status = 'left', deleted_at = now()
		WHERE id = $1`, firstMem)
	require.NoError(t, err)

	// Now rejoin — insert a NEW row for the same (tenant_id, user_id).
	// Partial unique WHERE deleted_at IS NULL should allow this.
	ctxA := withTenant(ctx, tenantA)
	err = pgcommon.RunInTx(ctxA, appPool, pgxTxOpts(), func(ctx context.Context, tx pgxTx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
			VALUES (gen_random_uuid(), $1, $2, 'active')`, tenantA, userID)
		return err
	})
	require.NoError(t, err, "TM-11: rejoin after soft-leave must succeed")

	// Should now have exactly one ACTIVE row for the user and one deleted row.
	var active, total int
	require.NoError(t, rawPool.QueryRow(ctx, `
		SELECT count(*) FROM tenant_memberships
		WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL`, tenantA, userID).Scan(&active))
	require.NoError(t, rawPool.QueryRow(ctx, `
		SELECT count(*) FROM tenant_memberships
		WHERE tenant_id = $1 AND user_id = $2`, tenantA, userID).Scan(&total))
	assert.Equal(t, 1, active, "exactly one active row after rejoin")
	assert.Equal(t, 2, total, "old + new row both present in table")
}

// TestPI1_InvitationRejoinAfterTerminal — mirrors TM-11 for
// pending_invitations. The partial unique uq_pi_pending (WHERE
// status='pending') must permit a fresh 'pending' row for the same
// (tenant_id, email) after the previous one moved to a terminal state.
func TestPI1_InvitationRejoinAfterTerminal(t *testing.T) {
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "pi1-test")

	// First invitation: revoked (terminal).
	_, err := rawPool.Exec(ctx, `
		INSERT INTO pending_invitations (id, tenant_id, email, full_name, invited_by, status, expires_at)
		VALUES (gen_random_uuid(), $1, 'bob@example.com', 'Bob', gen_random_uuid(), 'revoked', now() + interval '7 days')`,
		tenantA)
	require.NoError(t, err)

	// Second invitation for the same email — should succeed since the
	// previous is terminal (status <> 'pending'), so uq_pi_pending
	// (WHERE status='pending') doesn't fire.
	_, err = rawPool.Exec(ctx, `
		INSERT INTO pending_invitations (id, tenant_id, email, full_name, invited_by, status, expires_at)
		VALUES (gen_random_uuid(), $1, 'bob@example.com', 'Bob', gen_random_uuid(), 'pending', now() + interval '7 days')`,
		tenantA)
	require.NoError(t, err, "PI-1: reinvite after terminal must succeed")

	// A THIRD 'pending' insert for the same email MUST fail — one active
	// pending at a time per (tenant, email).
	_, err = rawPool.Exec(ctx, `
		INSERT INTO pending_invitations (id, tenant_id, email, full_name, invited_by, status, expires_at)
		VALUES (gen_random_uuid(), $1, 'bob@example.com', 'Bob', gen_random_uuid(), 'pending', now() + interval '7 days')`,
		tenantA)
	require.Error(t, err, "PI-1: two simultaneous pending invitations for same email must be rejected")
	// pgx returns a *pgconn.PgError with code 23505 (unique_violation).
	assert.Contains(t, err.Error(), "uq_pi_pending", "PI-1 partial unique should be the constraint that fires")

	// suppress unused-import warning
	_ = pgx.ErrNoRows
}
