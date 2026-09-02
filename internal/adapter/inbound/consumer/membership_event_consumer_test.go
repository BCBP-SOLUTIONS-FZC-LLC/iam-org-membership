package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingTenants captures ApplyLifecyclePatch calls so applyProjection
// tests assert on the port patch instead of SQL strings.
type recordingTenants struct {
	port.TenantRepositoryNoop
	patches []port.TenantLifecyclePatch
	applyFn func(ctx context.Context, id uuid.UUID, patch port.TenantLifecyclePatch) (int64, error)
}

func (r *recordingTenants) ApplyLifecyclePatch(ctx context.Context, id uuid.UUID, patch port.TenantLifecyclePatch) (int64, error) {
	r.patches = append(r.patches, patch)
	if r.applyFn != nil {
		return r.applyFn(ctx, id, patch)
	}
	return 1, nil
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

// ── applyProjection — walk every case with recordingTenants ────────────

func mkEnv(eventType string, payload any) events.Envelope[json.RawMessage] {
	b, _ := json.Marshal(payload)
	return events.Envelope[json.RawMessage]{
		ID:      uuid.New().String(),
		Type:    eventType,
		Payload: json.RawMessage(b),
	}
}

func applyOn(c *MembershipEventConsumer, env events.Envelope[json.RawMessage], prevStatus domain.SubscriptionStatus, prevPlan domain.TenantPlan) (domain.SubscriptionStatus, domain.TenantPlan, error) {
	return c.applyProjection(context.Background(), uuid.New(), env, prevStatus, prevPlan, 30)
}

func newConsumer() (*MembershipEventConsumer, *recordingTenants) {
	tenants := &recordingTenants{}
	return NewMembershipEventConsumer(nil, tenants, nil, nil, nil, 300*time.Second, nil), tenants
}

func lastPatch(t *testing.T, tenants *recordingTenants) port.TenantLifecyclePatch {
	t.Helper()
	require.NotEmpty(t, tenants.patches)
	return tenants.patches[len(tenants.patches)-1]
}

// The Trial signup path is a no-op projection (the TrialTenantProvisioned
// consumer handles it directly, not this projection).
func TestApplyProjection_TrialTenantProvisioned_IsNoop(t *testing.T) {
	c, tenants := newConsumer()
	st, pl, err := applyOn(c,
		mkEnv("TrialTenantProvisioned", struct{}{}),
		domain.StatusTrial, domain.TenantPlan("starter"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusTrial, st, "no state change")
	assert.Equal(t, domain.TenantPlan("starter"), pl)
	assert.Empty(t, tenants.patches, "no UPDATE for TrialTenantProvisioned")
}

func TestApplyProjection_TenantRealmReady_UpdatesRealmColumns(t *testing.T) {
	// Payload shape matches RP's actual frozen TenantRealmReadyPayload
	// (§25): "realm" + "keycloak_shard" — NOT "realm_id"/"realm_type",
	// which RP never sends (a prior version of this test used the wrong,
	// imagined field names, matching the handler's own bug rather than
	// RP's real wire format — extra fields RP also sends, tenant_id and
	// oidc_clients, are included here too, to prove they're harmlessly
	// ignored rather than causing a decode error).
	c, tenants := newConsumer()
	_, _, err := applyOn(c,
		mkEnv("TenantRealmReady", map[string]any{
			"tenant_id": uuid.New().String(), "realm": "acme-realm",
			"keycloak_shard": "shard-1", "oidc_clients": []string{"web", "mobile"},
		}),
		domain.StatusActive, domain.TenantPlan("pro"))
	require.NoError(t, err)
	p := lastPatch(t, tenants)
	assert.Equal(t, port.LifecycleSetRealm, p.Op)
	assert.Equal(t, "acme-realm", p.RealmID)
	assert.Equal(t, "dedicated", p.RealmType,
		"RealmType is hardcoded, never read from the payload — TenantRealmReady is only ever emitted for RP-2/RP-3, never the shared trial realm")
	assert.Equal(t, "shard-1", p.KeycloakShard)
}

func TestApplyProjection_TenantRealmReady_MalformedPayloadReturnsError(t *testing.T) {
	c, tenants := newConsumer()
	env := events.Envelope[json.RawMessage]{
		Type: "TenantRealmReady", Payload: json.RawMessage(`{not json}`),
	}
	_, _, err := applyOn(c, env, domain.StatusActive, domain.TenantPlan("pro"))
	require.Error(t, err)
	assert.Empty(t, tenants.patches, "malformed payload must short-circuit before the patch")
}

func TestApplyProjection_TenantConverted_ActivatesAndFlipsPlan(t *testing.T) {
	c, tenants := newConsumer()
	st, pl, err := applyOn(c,
		mkEnv("TenantConverted", map[string]any{"plan": "enterprise"}),
		domain.StatusTrial, domain.TenantPlan("starter"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, st)
	assert.Equal(t, domain.TenantPlan("enterprise"), pl)
	p := lastPatch(t, tenants)
	assert.Equal(t, port.LifecycleActivatePaid, p.Op)
	assert.Equal(t, domain.TenantPlan("enterprise"), p.Plan)
}

func TestApplyProjection_TenantConverted_EmptyPlanKeepsPrev(t *testing.T) {
	c, tenants := newConsumer()
	_, pl, err := applyOn(c,
		mkEnv("TenantConverted", map[string]any{"plan": ""}),
		domain.StatusTrial, domain.TenantPlan("starter"))
	require.NoError(t, err)
	assert.Equal(t, domain.TenantPlan("starter"), pl,
		"empty plan preserves the prior plan value")
	assert.Equal(t, domain.TenantPlan("starter"), lastPatch(t, tenants).Plan)
}

func TestApplyProjection_DirectPaidSignup_ActivatesAndFlipsPlan(t *testing.T) {
	c, _ := newConsumer()
	st, pl, err := applyOn(c,
		mkEnv("DirectPaidSignup", map[string]any{"plan": "pro"}),
		domain.StatusTrial, domain.TenantPlan("starter"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, st)
	assert.Equal(t, domain.TenantPlan("pro"), pl)
}

func TestApplyProjection_TrialExpired_FlipsStatus(t *testing.T) {
	c, tenants := newConsumer()
	st, _, err := applyOn(c,
		mkEnv("TrialExpired", struct{}{}),
		domain.StatusTrial, domain.TenantPlan("starter"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusTrialExpired, st)
	p := lastPatch(t, tenants)
	assert.Equal(t, port.LifecycleSetStatusClearSuspension, p.Op)
	assert.Equal(t, domain.StatusTrialExpired, p.Status)
}

func TestApplyProjection_TrialReactivated_ZeroRowsAffectedIsNoop(t *testing.T) {
	// TRIAL-5 cap: reactivation_count < 1 gate. When the row is capped
	// (rows == 0), the projection must return prev state unchanged.
	tenants := &recordingTenants{
		applyFn: func(context.Context, uuid.UUID, port.TenantLifecyclePatch) (int64, error) {
			return 0, nil
		},
	}
	c := NewMembershipEventConsumer(nil, tenants, nil, nil, nil, 300*time.Second, nil)
	st, pl, err := applyOn(c,
		mkEnv("TrialReactivated", struct{}{}),
		domain.StatusTrialExpired, domain.TenantPlan("starter"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusTrialExpired, st, "cap hit → no state change")
	assert.Equal(t, domain.TenantPlan("starter"), pl)
}

func TestApplyProjection_TrialReactivated_RowsAffectedFlipsStatus(t *testing.T) {
	c, _ := newConsumer()
	st, _, err := applyOn(c,
		mkEnv("TrialReactivated", struct{}{}),
		domain.StatusTrialExpired, domain.TenantPlan("starter"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusTrial, st)
}

func TestApplyProjection_TrialReactivated_ExecErrorPropagates(t *testing.T) {
	execErr := errors.New("db down")
	tenants := &recordingTenants{
		applyFn: func(context.Context, uuid.UUID, port.TenantLifecyclePatch) (int64, error) {
			return 0, execErr
		},
	}
	c := NewMembershipEventConsumer(nil, tenants, nil, nil, nil, 300*time.Second, nil)
	_, _, err := applyOn(c,
		mkEnv("TrialReactivated", struct{}{}),
		domain.StatusTrialExpired, domain.TenantPlan("starter"))
	assert.ErrorIs(t, err, execErr)
}

func TestApplyProjection_TenantSuspended_SetsStatusAndCancelledAt(t *testing.T) {
	// No "source" field — defaults to billing_lapse (T-16), matching the
	// pre-RP-14 behavior this test originally covered.
	c, tenants := newConsumer()
	st, _, err := applyOn(c,
		mkEnv("TenantSuspended", struct{}{}),
		domain.StatusActive, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusSuspended, st)
	assert.Equal(t, port.LifecycleSuspendBillingLapse, lastPatch(t, tenants).Op)
}

func TestApplyProjection_TenantSuspended_ExplicitBillingLapse_SetsCancelledAt(t *testing.T) {
	c, tenants := newConsumer()
	st, _, err := applyOn(c,
		mkEnv("TenantSuspended", map[string]any{"source": "billing_lapse"}),
		domain.StatusCancelled, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusSuspended, st)
	assert.Equal(t, port.LifecycleSuspendBillingLapse, lastPatch(t, tenants).Op)
}

// T-16 (new, resolves RP-11): an RP-14 operator-sourced suspension can land
// directly on active/trial. cancelled_at must NOT be touched — this tenant
// never enters the §15.5 grace/retention clock.
func TestApplyProjection_TenantSuspended_OperatorSource_LeavesCancelledAtUntouched(t *testing.T) {
	c, tenants := newConsumer()
	st, _, err := applyOn(c,
		mkEnv("TenantSuspended", map[string]any{"source": "operator"}),
		domain.StatusActive, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusSuspended, st)
	assert.Equal(t, port.LifecycleSuspendOperator, lastPatch(t, tenants).Op)
}

func TestApplyProjection_TenantSuspended_OperatorSource_FromTrial(t *testing.T) {
	// RP-11's own scenario: active/trial straight to suspended, skipping cancelled.
	c, tenants := newConsumer()
	st, _, err := applyOn(c,
		mkEnv("TenantSuspended", map[string]any{"source": "operator"}),
		domain.StatusTrial, domain.TenantPlan("starter"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusSuspended, st)
	assert.Equal(t, port.LifecycleSuspendOperator, lastPatch(t, tenants).Op)
}

func TestApplyProjection_TenantOffboarded_SetsTerminalState(t *testing.T) {
	c, tenants := newConsumer()
	st, _, err := applyOn(c,
		mkEnv("TenantOffboarded", struct{}{}),
		domain.StatusSuspended, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusOffboarded, st)
	assert.Equal(t, port.LifecycleOffboard, lastPatch(t, tenants).Op)
}

func TestApplyProjection_TenantPlanChanged_UpdatesPlanOnly(t *testing.T) {
	c, tenants := newConsumer()
	st, pl, err := applyOn(c,
		mkEnv("TenantPlanChanged", map[string]any{"plan": "enterprise"}),
		domain.StatusActive, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, st, "T-9: status untouched")
	assert.Equal(t, domain.TenantPlan("enterprise"), pl)
	p := lastPatch(t, tenants)
	assert.Equal(t, port.LifecycleSetPlan, p.Op)
	assert.Equal(t, domain.TenantPlan("enterprise"), p.Plan)
}

func TestApplyProjection_TenantPaymentPastDue_FlipsToPastDue(t *testing.T) {
	c, _ := newConsumer()
	st, _, err := applyOn(c,
		mkEnv("TenantPaymentPastDue", struct{}{}),
		domain.StatusActive, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusPastDue, st)
}

func TestApplyProjection_TenantSubscriptionCancelled_PreservesExistingCancelledAt(t *testing.T) {
	c, tenants := newConsumer()
	st, _, err := applyOn(c,
		mkEnv("TenantSubscriptionCancelled", struct{}{}),
		domain.StatusActive, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusCancelled, st)
	assert.Equal(t, port.LifecycleCancel, lastPatch(t, tenants).Op)
}

func TestApplyProjection_TenantReactivated_FromOffboarded_Rejected(t *testing.T) {
	// PAID-1: reactivation on an offboarded tenant is silently rejected.
	c, tenants := newConsumer()
	st, _, err := applyOn(c,
		mkEnv("TenantReactivated", struct{}{}),
		domain.StatusOffboarded, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusOffboarded, st, "PAID-1: terminal, no revival")
	assert.Empty(t, tenants.patches, "no UPDATE must fire on rejected reactivate")
}

func TestApplyProjection_TenantReactivated_FromCancelled_Succeeds(t *testing.T) {
	c, tenants := newConsumer()
	st, _, err := applyOn(c,
		mkEnv("TenantReactivated", struct{}{}),
		domain.StatusCancelled, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, st)
	assert.Equal(t, port.LifecycleReactivatePaid, lastPatch(t, tenants).Op)
}

// T-16 (resolves RP-11): reactivating an operator-suspended tenant that was
// previously paid clears suspension_source the same way as a
// billing_lapse-suspended one, and still resolves to 'active'.
func TestApplyProjection_TenantReactivated_FromOperatorSuspended_ClearsSuspensionSource(t *testing.T) {
	c, tenants := newConsumer()
	st, _, err := applyOn(c,
		mkEnv("TenantReactivated", struct{}{}),
		domain.StatusSuspended, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, st)
	assert.Equal(t, port.LifecycleReactivatePaid, lastPatch(t, tenants).Op)
}

// T-16 (resolves RP-11): reactivating a tenant operator-suspended while still
// a never-converted trial (subscription_started_at IS NULL) must return it
// to 'trial', not 'active' — the first conditioned UPDATE affects 0 rows,
// the second (subscription_started_at IS NULL) affects 1.
func TestApplyProjection_TenantReactivated_NeverConvertedTrial_ReturnsToTrial(t *testing.T) {
	tenants := &recordingTenants{
		applyFn: func(_ context.Context, _ uuid.UUID, patch port.TenantLifecyclePatch) (int64, error) {
			if patch.Op == port.LifecycleReactivatePaid {
				return 0, nil
			}
			return 1, nil
		},
	}
	c := NewMembershipEventConsumer(nil, tenants, nil, nil, nil, 300*time.Second, nil)
	st, _, err := applyOn(c,
		mkEnv("TenantReactivated", struct{}{}),
		domain.StatusSuspended, domain.TenantPlan("starter"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusTrial, st)
	require.Len(t, tenants.patches, 2, "must try the active-branch UPDATE first, then fall back to trial")
	assert.Equal(t, port.LifecycleReactivatePaid, tenants.patches[0].Op)
	assert.Equal(t, port.LifecycleReactivateTrial, tenants.patches[1].Op)
}

func TestApplyProjection_TenantSeatsChanged_UpdatesLicensedSeats(t *testing.T) {
	c, tenants := newConsumer()
	st, _, err := applyOn(c,
		mkEnv("TenantSeatsChanged", map[string]any{"licensed_seats": 50}),
		domain.StatusActive, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, st, "SEAT-2: state unaffected")
	p := lastPatch(t, tenants)
	assert.Equal(t, port.LifecycleSetLicensedSeats, p.Op)
	assert.Equal(t, 50, p.LicensedSeats)
}

func TestApplyProjection_TenantSeatsChanged_MalformedPayloadReturnsError(t *testing.T) {
	c, tenants := newConsumer()
	env := events.Envelope[json.RawMessage]{
		Type:    "TenantSeatsChanged",
		Payload: json.RawMessage(`{bad json`),
	}
	_, _, err := applyOn(c, env, domain.StatusActive, domain.TenantPlan("pro"))
	require.Error(t, err)
	assert.Empty(t, tenants.patches)
}

// ── Unknown event type: returns unchanged prev + no patch ──────────────

func TestApplyProjection_UnknownTypeIsNoop(t *testing.T) {
	c, tenants := newConsumer()
	st, pl, err := applyOn(c,
		mkEnv("SomeFutureEvent", struct{}{}),
		domain.StatusActive, domain.TenantPlan("pro"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, st)
	assert.Equal(t, domain.TenantPlan("pro"), pl)
	assert.Empty(t, tenants.patches, "unknown types fall through to default no-op")
}
