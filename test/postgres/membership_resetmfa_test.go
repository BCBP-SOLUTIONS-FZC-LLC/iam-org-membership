//go:build integration

// P-34 (§16 OQ-8/F6) — MFA reset lands its audit event through the REAL
// outbox mechanics (real tx, real outbox_events insert). The fail-closed
// branch (RP-9 outage → no event enqueued) is already covered at the unit
// layer (test/unit/membership_resetmfa_test.go) with a fake TxRunner; this
// test verifies the one thing a fake TxRunner can't: that the happy path
// actually commits the MFAReset row atomically via the real Postgres tx.
package postgres_test

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResetUserMFA_HappyPath_EmitsMFAResetToOutbox(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerID := seedTenantWithOwner(t, ctx, fx, "acme-p34")
	tctx := withSystemAndTenant(ctx, tenantID)
	actorID := uuid.New()

	baseline := fx.countOutboxEvents(t, ctx, tenantID)

	err := fx.Membership.ResetUserMFA(tctx, tenantID, ownerID, actorID)
	require.NoError(t, err)

	assert.Equal(t, baseline+1, fx.countOutboxEvents(t, ctx, tenantID),
		"exactly one new outbox row (MFAReset) must land")

	rows, err := fx.rawPool.Query(ctx, `
		SELECT event_type, payload::text FROM outbox_events
		WHERE tenant_id = $1
		ORDER BY created_at DESC LIMIT 5`, tenantID.String())
	require.NoError(t, err)
	defer rows.Close()
	var sawMFAReset bool
	for rows.Next() {
		var eventType, payload string
		require.NoError(t, rows.Scan(&eventType, &payload))
		if eventType == domain.EventMFAReset {
			sawMFAReset = true
			assert.Contains(t, payload, tenantID.String())
			assert.Contains(t, payload, ownerID.String())
			assert.Contains(t, payload, actorID.String())
		}
	}
	assert.True(t, sawMFAReset, "MFAReset must be in the outbox")
}

func TestResetUserMFA_MemberNotActive_NoOutboxEvent(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerID := seedTenantWithOwner(t, ctx, fx, "acme-p34-inactive")
	tctx := withSystemAndTenant(ctx, tenantID)

	_, err := fx.rawPool.Exec(ctx, `UPDATE tenant_memberships SET status='suspended' WHERE tenant_id=$1 AND user_id=$2`, tenantID, ownerID)
	require.NoError(t, err)

	baseline := fx.countOutboxEvents(t, ctx, tenantID)

	err = fx.Membership.ResetUserMFA(tctx, tenantID, ownerID, uuid.New())
	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrMemberNotActive, de.Cause)
	assert.Equal(t, baseline, fx.countOutboxEvents(t, ctx, tenantID),
		"no event may be enqueued when the target isn't active")
}
