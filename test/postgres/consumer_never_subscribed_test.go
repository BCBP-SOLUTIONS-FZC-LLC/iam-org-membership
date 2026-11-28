//go:build integration

// Billing compatibility check C-3 (LLD rev 2.67): TenantSubscriptionCancelled
// and TenantPaymentPastDue for a tenant whose conversion was reverted
// (subscription_started_at IS NULL, trial_plan set) are acked as no-ops with a
// warning, instead of failing chk_subscription_started_required and
// dead-lettering. For a trial that never converted they still fail and are
// retried, because they may have overtaken RP's TenantConverted.
package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/consumer"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// warnRecorder is a port.Logger that keeps the Warn messages.
type warnRecorder struct {
	mu    sync.Mutex
	warns []string
}

func (l *warnRecorder) Debug(string, map[string]any) {}
func (l *warnRecorder) Info(string, map[string]any)  {}
func (l *warnRecorder) Error(string, map[string]any) {}
func (l *warnRecorder) Warn(msg string, _ map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}

var _ port.Logger = (*warnRecorder)(nil)

func TestConsumer_BillingEventForRevertedConversion_AckedAsNoOp(t *testing.T) {
	for _, eventType := range []string{"TenantSubscriptionCancelled", "TenantPaymentPastDue"} {
		t.Run(eventType, func(t *testing.T) {
			t.Parallel()
			appPool, rawPool, _ := setupTestDB(t)
			ctx := context.Background()
			tenantID := seedTenant(t, ctx, rawPool, "c3-"+eventType) // trial, subscription_started_at NULL
			// A conversion revert leaves trial_plan set (rev 2.65).
			_, err := rawPool.Exec(ctx, `UPDATE tenants SET trial_plan = plan WHERE id = $1`, tenantID)
			require.NoError(t, err)

			type row struct {
				Status      string
				LastEventAt *time.Time
				CancelledAt *time.Time
			}
			read := func() row {
				var r row
				require.NoError(t, rawPool.QueryRow(ctx,
					`SELECT status, last_event_at, cancelled_at FROM tenants WHERE id = $1`, tenantID).
					Scan(&r.Status, &r.LastEventAt, &r.CancelledAt))
				return r
			}
			before := read()

			outbox := &captureOutbox{}
			logger := &warnRecorder{}
			c := consumer.NewMembershipEventConsumer(pgadapter.NewInboxRepository(appPool, outbox),
				pgadapter.NewTenantRepository(appPool), nil, nil, 5*time.Minute, logger)

			env := mkEnvelope(t, eventType, tenantID, time.Now().UTC().Add(-time.Minute), map[string]string{})
			require.NoError(t, c.Handle(ctx, env), "must be acked, not retried into the DLQ")

			assert.Equal(t, before, read(), "no DB change: status, last_event_at and cancelled_at untouched")
			assert.Equal(t, "trial", read().Status)
			assert.Zero(t, outbox.count(), "no TenantStateChanged relay")

			var processed int
			require.NoError(t, rawPool.QueryRow(ctx,
				`SELECT count(*) FROM processed_events WHERE event_id = $1::uuid`, env.ID).Scan(&processed))
			assert.Equal(t, 1, processed, "the event is recorded as processed (acked)")

			logger.mu.Lock()
			defer logger.mu.Unlock()
			assert.Contains(t, logger.warns, "billing event for a reverted conversion — acked as a no-op")
		})
	}
}

// A cancel that overtakes RP's TenantConverted (different queues, unordered)
// must not be dropped: it fails, is redelivered, and applies once the
// conversion has landed.
func TestConsumer_CancelBeforeTenantConverted_RetriedThenApplies(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "c3-cancel-before-convert") // never converted, trial_plan NULL

	c := consumer.NewMembershipEventConsumer(pgadapter.NewInboxRepository(appPool, &captureOutbox{}),
		pgadapter.NewTenantRepository(appPool), nil, nil, 5*time.Minute, nil)

	t0 := time.Now().UTC().Add(-time.Minute)
	converted := mkEnvelope(t, "TenantConverted", tenantID, t0,
		map[string]any{"tenant_id": tenantID, "plan": "pro"})
	cancel := mkEnvelope(t, "TenantSubscriptionCancelled", tenantID, t0.Add(10*time.Second), map[string]string{})

	require.Error(t, c.Handle(ctx, cancel), "an early cancel must fail so SQS redelivers it")
	var processed int
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT count(*) FROM processed_events WHERE event_id = $1::uuid`, cancel.ID).Scan(&processed))
	assert.Zero(t, processed, "the failed cancel must not be recorded as processed")

	require.NoError(t, c.Handle(ctx, converted))
	require.NoError(t, c.Handle(ctx, cancel), "the redelivered cancel applies after the conversion")

	var status string
	var cancelledAt *time.Time
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT status, cancelled_at FROM tenants WHERE id = $1`, tenantID).Scan(&status, &cancelledAt))
	assert.Equal(t, "cancelled", status)
	assert.NotNil(t, cancelledAt, "the grace/retention clock starts")
}
