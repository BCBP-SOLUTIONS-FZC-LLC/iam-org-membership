package consumer

// CD-17 tests — envelope source-name acceptance for Commercial domain events.
//
// Design decision (confirmed by code audit, 2026-10-14):
//   iam-org-membership's billing-orgm-q consumer routes exclusively on
//   env.Type — it does NOT validate, filter, or branch on env.Source. An
//   event with Source="commercial-billing" is processed identically to one
//   with Source="iam-billing" (the retired name) or even an empty Source.
//
// Behaviour for old names: since no source validation exists, events with
// retired source names ("iam-billing", "billing") also pass through. This is
// intentional forward-compat: no deployed environment is known to produce
// Commercial events yet, so there is no need to reject old names, and adding
// a rejection would risk dropping events during any transition window. If a
// future audit shows a live producer using old names, a separate PR should
// add explicit rejection after the producer is updated.
//
// What changes under CD-17: the KnownSources constants (known_sources.go)
// give future code and tests a stable single reference instead of scattered
// string literals. The asyncapi.yaml SNS-attribute descriptions already use
// "commercial-billing" (updated in the prior CD-17 asyncapi pass).

import (
	"encoding/json"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mkEnvWithSource creates a test envelope with a specific Source field.
// Use this helper in any test that cares about the envelope source;
// mkEnv (existing helper) leaves Source empty.
func mkEnvWithSource(eventType, source string, payload any) events.Envelope[json.RawMessage] {
	b, _ := json.Marshal(payload)
	return events.Envelope[json.RawMessage]{
		ID:      uuid.New().String(),
		Type:    eventType,
		Source:  source,
		Payload: json.RawMessage(b),
	}
}

// ── CD-17: commercial-billing events are accepted and correctly projected ──

// TestCD17_CommercialBilling_TenantPlanChanged_Accepted verifies that a
// TenantPlanChanged event carrying Source="commercial-billing" (the new
// agreed name for the Billing Service) is processed identically to one with
// no source set. This is the primary billing-orgm-q event type.
func TestCD17_CommercialBilling_TenantPlanChanged_Accepted(t *testing.T) {
	c, tenants := newConsumer()
	env := mkEnvWithSource("TenantPlanChanged", SourceCommercialBilling,
		map[string]any{"plan": "enterprise"})

	_, newPlan, err := applyOn(c, env, domain.StatusActive, domain.PlanPro)

	require.NoError(t, err)
	assert.Equal(t, domain.PlanEnterprise, newPlan,
		"commercial-billing source must not block TenantPlanChanged projection")
	patch := lastPatch(t, tenants)
	assert.Equal(t, domain.PlanEnterprise, patch.Plan)
}

// TestCD17_CommercialBilling_TenantPaymentPastDue_Accepted verifies the
// past-due state transition from a commercial-billing source.
func TestCD17_CommercialBilling_TenantPaymentPastDue_Accepted(t *testing.T) {
	c, _ := newConsumer()
	env := mkEnvWithSource("TenantPaymentPastDue", SourceCommercialBilling, struct{}{})

	newStatus, _, err := applyOn(c, env, domain.StatusActive, domain.PlanPro)

	require.NoError(t, err)
	assert.Equal(t, domain.StatusPastDue, newStatus)
}

// TestCD17_CommercialBilling_TenantSubscriptionCancelled_Accepted verifies
// the cancellation projection from a commercial-billing source.
func TestCD17_CommercialBilling_TenantSubscriptionCancelled_Accepted(t *testing.T) {
	c, _ := newConsumer()
	env := mkEnvWithSource("TenantSubscriptionCancelled", SourceCommercialBilling, struct{}{})

	newStatus, _, err := applyOn(c, env, domain.StatusActive, domain.PlanPro)

	require.NoError(t, err)
	assert.Equal(t, domain.StatusCancelled, newStatus)
}

// TestCD17_CommercialBilling_TenantSeatsChanged_Accepted verifies the seat
// count update from a commercial-billing source.
func TestCD17_CommercialBilling_TenantSeatsChanged_Accepted(t *testing.T) {
	c, tenants := newConsumer()
	env := mkEnvWithSource("TenantSeatsChanged", SourceCommercialBilling,
		map[string]any{"licensed_seats": 25})

	_, _, err := applyOn(c, env, domain.StatusActive, domain.PlanPro)

	require.NoError(t, err)
	patch := lastPatch(t, tenants)
	assert.Equal(t, 25, patch.LicensedSeats)
}

// TestCD17_CommercialBilling_TenantReactivated_Accepted verifies
// TenantReactivated from commercial-billing resolves to active for a paid
// tenant (subscription_started_at set — LifecycleReactivatePaid path).
func TestCD17_CommercialBilling_TenantReactivated_Accepted(t *testing.T) {
	c, _ := newConsumer()
	env := mkEnvWithSource("TenantReactivated", SourceCommercialBilling, struct{}{})

	// newConsumer uses recordingTenants which returns 1 row for any patch
	// (simulating subscription_started_at IS NOT NULL).
	newStatus, _, err := applyOn(c, env, domain.StatusCancelled, domain.PlanPro)

	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, newStatus)
}

// ── CD-17: source field does not affect payload-level source distinction ──

// TestCD17_TenantSuspended_PayloadSource_Unaffected_By_EnvelopeSource
// verifies that the TenantSuspended PAYLOAD source field ("billing_lapse" vs
// "operator") is decoded from the event DATA payload — not from the envelope
// Source field. An event with envelope Source="commercial-billing" and payload
// source "operator" must still produce LifecycleSuspendOperator, not
// LifecycleSuspendBillingLapse. This proves the two "source" concepts remain
// independent after CD-17.
func TestCD17_TenantSuspended_PayloadSource_Unaffected_By_EnvelopeSource(t *testing.T) {
	c, tenants := newConsumer()
	// Envelope source = commercial-billing (billing queue),
	// payload source = "operator" (RP-14 administrative suspension).
	env := mkEnvWithSource("TenantSuspended", SourceCommercialBilling,
		map[string]any{"source": "operator"})

	newStatus, _, err := applyOn(c, env, domain.StatusActive, domain.PlanPro)

	require.NoError(t, err)
	assert.Equal(t, domain.StatusSuspended, newStatus)
	patch := lastPatch(t, tenants)
	// Payload source "operator" must win, not the envelope source.
	assert.Equal(t, port.LifecycleSuspendOperator, patch.Op,
		"payload source=operator must produce LifecycleSuspendOperator regardless of envelope Source")
}

// ── CD-17: retired source names do not cause rejection ────────────────────

// TestCD17_RetiredBillingSource_StillAccepted documents the behaviour for
// old source names. Since no source validation exists in the consumer, events
// with the retired "iam-billing" envelope source pass through unchanged.
// This test is intentionally documenting current behaviour, not endorsing
// future use of the old name.
//
// Decision: do NOT add rejection for old names until a live producer using
// them is identified. See CD-17 implementation note in this file's header.
func TestCD17_RetiredBillingSource_StillAccepted(t *testing.T) {
	c, _ := newConsumer()
	// "iam-billing" is the retired source name (before CD-17).
	env := mkEnvWithSource("TenantPlanChanged", "iam-billing",
		map[string]any{"plan": "pro"})

	_, newPlan, err := applyOn(c, env, domain.StatusActive, domain.PlanStarter)

	require.NoError(t, err)
	assert.Equal(t, domain.PlanPro, newPlan,
		"retired source name iam-billing must not block processing (no rejection logic exists)")
}

// TestCD17_SourceConstants_StableValues is a wire-contract guard ensuring the
// exported source name constants never silently change. These strings flow
// into SNS message attributes and consumer allow-lists in other services.
func TestCD17_SourceConstants_StableValues(t *testing.T) {
	assert.Equal(t, "commercial-billing", SourceCommercialBilling)
	assert.Equal(t, "commercial-entitlements", SourceCommercialEntitlements)
	assert.Equal(t, "commercial-usage-metering", SourceCommercialUsageMetering)
}
