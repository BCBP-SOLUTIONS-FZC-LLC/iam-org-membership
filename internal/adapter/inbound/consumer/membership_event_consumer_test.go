package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTx implements just enough of pgx.Tx for applyProjection's tx.Exec
// calls. Non-exec methods nil-panic (via the embedded interface) so tests
// prove the SUT only touched Exec.
type fakeTx struct {
	pgx.Tx
	execFn     func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	lastSQL    string
	lastArgs   []any
	execCalled bool
}

func (f *fakeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execCalled = true
	f.lastSQL = sql
	f.lastArgs = args
	if f.execFn != nil {
		return f.execFn(ctx, sql, args...)
	}
	return pgconn.CommandTag{}, nil
}

// ── NewMembershipEventConsumer — default fields ────────────────────────

func TestNewMembershipEventConsumer_NilLoggerAndZeroSkewGetDefaults(t *testing.T) {
	c := NewMembershipEventConsumer(nil, nil, nil, nil, nil, 0, nil)
	require.NotNil(t, c)
	assert.Equal(t, 300*time.Second, c.skew, "zero skew defaults to 300s")
}

func TestNewMembershipEventConsumer_NegativeSkewAlsoGetsDefault(t *testing.T) {
	c := NewMembershipEventConsumer(nil, nil, nil, nil, nil, -5*time.Second, nil)
	assert.Equal(t, 300*time.Second, c.skew, "negative skew defaults to 300s")
}

func TestNewMembershipEventConsumer_PositiveSkewPreserved(t *testing.T) {
	c := NewMembershipEventConsumer(nil, nil, nil, nil, nil, 42*time.Second, nil)
	assert.Equal(t, 42*time.Second, c.skew)
}

// ── applyProjection — walk every case with fakeTx ──────────────────────

func mkEnv(eventType string, payload any) events.Envelope[json.RawMessage] {
	b, _ := json.Marshal(payload)
	return events.Envelope[json.RawMessage]{
		ID:      uuid.New().String(),
		Type:    eventType,
		Payload: json.RawMessage(b),
	}
}

func applyOn(c *MembershipEventConsumer, tx pgx.Tx, env events.Envelope[json.RawMessage], prevStatus domain.SubscriptionStatus, prevPlan domain.TenantPlan) (domain.SubscriptionStatus, domain.TenantPlan, error) {
	// trialDurationDays is only meaningful for TrialReactivated, and fakeTx
	// ignores Exec's bound args entirely (see fakeTx.Exec above) — any
	// constant is fine for every test that goes through this helper.
	return c.applyProjection(context.Background(), tx, uuid.New(), env, prevStatus, prevPlan, 30)
}

func newConsumer() *MembershipEventConsumer {
	return NewMembershipEventConsumer(nil, nil, nil, nil, nil, 300*time.Second, nil)
}

// The Trial signup path is a no-op projection (the TrialTenantProvisioned
// consumer handles it directly, not this projection).
func TestApplyProjection_TrialTenantProvisioned_IsNoop(t *testing.T) {
	tx := &fakeTx{}
	st, pl, err := applyOn(newConsumer(), tx,
		mkEnv("TrialTenantProvisioned", struct{}{}),
		domain.StatusTrial, domain.TenantPlan("starter"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusTrial, st, "no state change")
	assert.Equal(t, domain.TenantPlan("starter"), pl)
	assert.False(t, tx.execCalled, "no UPDATE for TrialTenantProvisioned")
}

func TestApplyProjection_TenantRealmReady_UpdatesRealmColumns(t *testing.T) {
	tx := &fakeTx{}
	_, _, err := applyOn(newConsumer(), tx,
		mkEnv("TenantRealmReady", map[string]any{
			"realm_id": "acme-realm", "realm_type": "dedicated", "keycloak_shard": "shard-1",
		}),
		domain.StatusActive, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.True(t, tx.execCalled)
	assert.Contains(t, tx.lastSQL, "realm_id")
	assert.Contains(t, tx.lastSQL, "realm_type")
	assert.Contains(t, tx.lastSQL, "keycloak_shard")
}

func TestApplyProjection_TenantRealmReady_MalformedPayloadReturnsError(t *testing.T) {
	tx := &fakeTx{}
	env := events.Envelope[json.RawMessage]{
		Type: "TenantRealmReady", Payload: json.RawMessage(`{not json}`),
	}
	_, _, err := applyOn(newConsumer(), tx, env, domain.StatusActive, domain.TenantPlan("pro"))
	require.Error(t, err)
	assert.False(t, tx.execCalled, "malformed payload must short-circuit before tx.Exec")
}

func TestApplyProjection_TenantConverted_ActivatesAndFlipsPlan(t *testing.T) {
	tx := &fakeTx{}
	st, pl, err := applyOn(newConsumer(), tx,
		mkEnv("TenantConverted", map[string]any{"plan": "enterprise"}),
		domain.StatusTrial, domain.TenantPlan("starter"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, st)
	assert.Equal(t, domain.TenantPlan("enterprise"), pl)
	assert.Contains(t, tx.lastSQL, "subscription_started_at")
}

func TestApplyProjection_TenantConverted_EmptyPlanKeepsPrev(t *testing.T) {
	tx := &fakeTx{}
	_, pl, err := applyOn(newConsumer(), tx,
		mkEnv("TenantConverted", map[string]any{"plan": ""}),
		domain.StatusTrial, domain.TenantPlan("starter"))
	require.NoError(t, err)
	assert.Equal(t, domain.TenantPlan("starter"), pl,
		"empty plan preserves the prior plan value")
}

func TestApplyProjection_DirectPaidSignup_ActivatesAndFlipsPlan(t *testing.T) {
	tx := &fakeTx{}
	st, pl, err := applyOn(newConsumer(), tx,
		mkEnv("DirectPaidSignup", map[string]any{"plan": "pro"}),
		domain.StatusTrial, domain.TenantPlan("starter"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, st)
	assert.Equal(t, domain.TenantPlan("pro"), pl)
}

func TestApplyProjection_TrialExpired_FlipsStatus(t *testing.T) {
	tx := &fakeTx{}
	st, _, err := applyOn(newConsumer(), tx,
		mkEnv("TrialExpired", struct{}{}),
		domain.StatusTrial, domain.TenantPlan("starter"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusTrialExpired, st)
	assert.Contains(t, tx.lastSQL, "trial_expired")
}

func TestApplyProjection_TrialReactivated_ZeroRowsAffectedIsNoop(t *testing.T) {
	// TRIAL-5 cap: reactivation_count < 1 gate. When the row is capped
	// (tag.RowsAffected() == 0), the projection must return prev state
	// unchanged.
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		},
	}
	st, pl, err := applyOn(newConsumer(), tx,
		mkEnv("TrialReactivated", struct{}{}),
		domain.StatusTrialExpired, domain.TenantPlan("starter"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusTrialExpired, st, "cap hit → no state change")
	assert.Equal(t, domain.TenantPlan("starter"), pl)
}

func TestApplyProjection_TrialReactivated_RowsAffectedFlipsStatus(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	st, _, err := applyOn(newConsumer(), tx,
		mkEnv("TrialReactivated", struct{}{}),
		domain.StatusTrialExpired, domain.TenantPlan("starter"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusTrial, st)
}

func TestApplyProjection_TrialReactivated_ExecErrorPropagates(t *testing.T) {
	execErr := errors.New("db down")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	_, _, err := applyOn(newConsumer(), tx,
		mkEnv("TrialReactivated", struct{}{}),
		domain.StatusTrialExpired, domain.TenantPlan("starter"))
	assert.ErrorIs(t, err, execErr)
}

func TestApplyProjection_TenantSuspended_SetsStatusAndCancelledAt(t *testing.T) {
	// No "source" field — defaults to billing_lapse (T-16), matching the
	// pre-RP-14 behavior this test originally covered.
	tx := &fakeTx{}
	st, _, err := applyOn(newConsumer(), tx,
		mkEnv("TenantSuspended", struct{}{}),
		domain.StatusActive, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusSuspended, st)
	assert.Contains(t, tx.lastSQL, "cancelled_at")
	assert.Contains(t, tx.lastSQL, "COALESCE",
		"idempotency preservation: don't reset an existing cancelled_at")
	assert.Contains(t, tx.lastSQL, "suspension_source")
	assert.Contains(t, tx.lastArgs, string(domain.SuspensionSourceBillingLapse))
}

func TestApplyProjection_TenantSuspended_ExplicitBillingLapse_SetsCancelledAt(t *testing.T) {
	tx := &fakeTx{}
	st, _, err := applyOn(newConsumer(), tx,
		mkEnv("TenantSuspended", map[string]any{"source": "billing_lapse"}),
		domain.StatusCancelled, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusSuspended, st)
	assert.Contains(t, tx.lastSQL, "COALESCE")
	assert.Contains(t, tx.lastArgs, string(domain.SuspensionSourceBillingLapse))
}

// T-16 (new, resolves RP-11): an RP-14 operator-sourced suspension can land
// directly on active/trial. cancelled_at must NOT be touched — this tenant
// never enters the §15.5 grace/retention clock.
func TestApplyProjection_TenantSuspended_OperatorSource_LeavesCancelledAtUntouched(t *testing.T) {
	tx := &fakeTx{}
	st, _, err := applyOn(newConsumer(), tx,
		mkEnv("TenantSuspended", map[string]any{"source": "operator"}),
		domain.StatusActive, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusSuspended, st)
	assert.NotContains(t, tx.lastSQL, "cancelled_at",
		"operator-sourced suspension must not stamp cancelled_at")
	assert.Contains(t, tx.lastSQL, "suspension_source")
	assert.Contains(t, tx.lastArgs, string(domain.SuspensionSourceOperator))
}

func TestApplyProjection_TenantSuspended_OperatorSource_FromTrial(t *testing.T) {
	// RP-11's own scenario: active/trial straight to suspended, skipping cancelled.
	tx := &fakeTx{}
	st, _, err := applyOn(newConsumer(), tx,
		mkEnv("TenantSuspended", map[string]any{"source": "operator"}),
		domain.StatusTrial, domain.TenantPlan("starter"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusSuspended, st)
}

func TestApplyProjection_TenantOffboarded_SetsTerminalState(t *testing.T) {
	tx := &fakeTx{}
	st, _, err := applyOn(newConsumer(), tx,
		mkEnv("TenantOffboarded", struct{}{}),
		domain.StatusSuspended, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusOffboarded, st)
	assert.Contains(t, tx.lastSQL, "deleted_at")
	assert.Contains(t, tx.lastSQL, "suspension_source = NULL",
		"T-16: offboarded requires suspension_source NULL even from an operator-suspended tenant")
}

func TestApplyProjection_TenantPlanChanged_UpdatesPlanOnly(t *testing.T) {
	tx := &fakeTx{}
	st, pl, err := applyOn(newConsumer(), tx,
		mkEnv("TenantPlanChanged", map[string]any{"plan": "enterprise"}),
		domain.StatusActive, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, st, "T-9: status untouched")
	assert.Equal(t, domain.TenantPlan("enterprise"), pl)
	assert.NotContains(t, tx.lastSQL, "feature_flags",
		"T-9: feature_flags untouched on plan changes")
}

func TestApplyProjection_TenantPaymentPastDue_FlipsToPastDue(t *testing.T) {
	tx := &fakeTx{}
	st, _, err := applyOn(newConsumer(), tx,
		mkEnv("TenantPaymentPastDue", struct{}{}),
		domain.StatusActive, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusPastDue, st)
}

func TestApplyProjection_TenantSubscriptionCancelled_PreservesExistingCancelledAt(t *testing.T) {
	tx := &fakeTx{}
	st, _, err := applyOn(newConsumer(), tx,
		mkEnv("TenantSubscriptionCancelled", struct{}{}),
		domain.StatusActive, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusCancelled, st)
	assert.Contains(t, tx.lastSQL, "COALESCE")
}

func TestApplyProjection_TenantReactivated_FromOffboarded_Rejected(t *testing.T) {
	// PAID-1: reactivation on an offboarded tenant is silently rejected.
	tx := &fakeTx{}
	st, _, err := applyOn(newConsumer(), tx,
		mkEnv("TenantReactivated", struct{}{}),
		domain.StatusOffboarded, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusOffboarded, st, "PAID-1: terminal, no revival")
	assert.False(t, tx.execCalled, "no UPDATE must fire on rejected reactivate")
}

func TestApplyProjection_TenantReactivated_FromCancelled_Succeeds(t *testing.T) {
	// subscription_started_at IS NOT NULL branch matches (RowsAffected=1) —
	// this represents a tenant that was previously paid (T-16).
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	st, _, err := applyOn(newConsumer(), tx,
		mkEnv("TenantReactivated", struct{}{}),
		domain.StatusCancelled, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, st)
	assert.Contains(t, tx.lastSQL, "cancelled_at = NULL")
	assert.Contains(t, tx.lastSQL, "suspension_source = NULL",
		"T-16: reactivation clears suspension_source regardless of which path led to suspended")
}

// T-16 (resolves RP-11): reactivating an operator-suspended tenant that was
// previously paid clears suspension_source the same way as a
// billing_lapse-suspended one, and still resolves to 'active'.
func TestApplyProjection_TenantReactivated_FromOperatorSuspended_ClearsSuspensionSource(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	st, _, err := applyOn(newConsumer(), tx,
		mkEnv("TenantReactivated", struct{}{}),
		domain.StatusSuspended, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, st)
	assert.Contains(t, tx.lastSQL, "suspension_source = NULL")
}

// T-16 (resolves RP-11): reactivating a tenant operator-suspended while still
// a never-converted trial (subscription_started_at IS NULL) must return it
// to 'trial', not 'active' — the first conditioned UPDATE affects 0 rows,
// the second (subscription_started_at IS NULL) affects 1.
func TestApplyProjection_TenantReactivated_NeverConvertedTrial_ReturnsToTrial(t *testing.T) {
	calls := 0
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			calls++
			if calls == 1 {
				return pgconn.NewCommandTag("UPDATE 0"), nil
			}
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	st, _, err := applyOn(newConsumer(), tx,
		mkEnv("TenantReactivated", struct{}{}),
		domain.StatusSuspended, domain.TenantPlan("starter"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusTrial, st)
	assert.Equal(t, 2, calls, "must try the active-branch UPDATE first, then fall back to trial")
	assert.Contains(t, tx.lastSQL, "status = 'trial'")
}

func TestApplyProjection_TenantSeatsChanged_UpdatesLicensedSeats(t *testing.T) {
	tx := &fakeTx{}
	st, _, err := applyOn(newConsumer(), tx,
		mkEnv("TenantSeatsChanged", map[string]any{"licensed_seats": 50}),
		domain.StatusActive, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, st, "SEAT-2: state unaffected")
	assert.Contains(t, tx.lastSQL, "licensed_seats")
	// Verify the licensed_seats arg was threaded through as int.
	require.Len(t, tx.lastArgs, 2)
	assert.Equal(t, 50, tx.lastArgs[1])
}

func TestApplyProjection_TenantSeatsChanged_MalformedPayloadReturnsError(t *testing.T) {
	tx := &fakeTx{}
	env := events.Envelope[json.RawMessage]{
		Type:    "TenantSeatsChanged",
		Payload: json.RawMessage(`{bad json`),
	}
	_, _, err := applyOn(newConsumer(), tx, env, domain.StatusActive, domain.TenantPlan("pro"))
	require.Error(t, err)
	assert.False(t, tx.execCalled)
}

// ── Unknown event type: returns unchanged prev + no tx.Exec ────────────

func TestApplyProjection_UnknownTypeIsNoop(t *testing.T) {
	tx := &fakeTx{}
	st, pl, err := applyOn(newConsumer(), tx,
		mkEnv("SomeFutureEvent", struct{}{}),
		domain.StatusActive, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, st)
	assert.Equal(t, domain.TenantPlan("pro"), pl)
	assert.False(t, tx.execCalled, "unknown types fall through to default no-op")
}
