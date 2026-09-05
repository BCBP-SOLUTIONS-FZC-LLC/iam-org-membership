// Phase 19 — consumer edge branches that don't need a real *pgcommon.Pool:
//   - EVT-15 future-time-clamp poison pill
//   - Envelope.TenantID parse error
//   - classify() enum coverage
//   - applyProjection malformed-JSON branches for the JSON-decoding cases
package consumer

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gdprWipeCacheKeys returns the exactly-known, bounded tenant-scoped keys
// (CACHE-8) — a pure function, directly testable.
func TestGdprWipeCacheKeys_ReturnsBoundedTenantScopedKeys(t *testing.T) {
	tenantID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	keys := gdprWipeCacheKeys(tenantID)

	want := []string{
		"om:tenant:11111111-1111-1111-1111-111111111111",
		"om:locale:11111111-1111-1111-1111-111111111111",
		"om:roles:11111111-1111-1111-1111-111111111111",
		"om:seat_usage:11111111-1111-1111-1111-111111111111",
		"om:members:11111111-1111-1111-1111-111111111111:50",
		"om:grm:11111111-1111-1111-1111-111111111111",
		"om:grm:stale:11111111-1111-1111-1111-111111111111",
		"om:gdm:11111111-1111-1111-1111-111111111111",
		"om:gdm:stale:11111111-1111-1111-1111-111111111111",
		"om:gtrm:11111111-1111-1111-1111-111111111111",
		"om:gtrm:stale:11111111-1111-1111-1111-111111111111",
	}
	assert.Equal(t, want, keys)
}

// EVT-15: env.Timestamp > now() + skew → ErrPoisonPill (returns BEFORE
// touching the pool, so a nil pool is fine).
func TestP19Consumer_EVT15_FutureTimestamp_ReturnsPoisonPill(t *testing.T) {
	c := NewMembershipEventConsumer(nil, nil, nil, nil, nil, 5*time.Minute, nil)
	env := events.Envelope[json.RawMessage]{
		ID:        uuid.New().String(),
		Type:      "TenantSuspended",
		TenantID:  uuid.New().String(),
		Timestamp: time.Now().UTC().Add(1 * time.Hour),
	}
	err := c.Handle(context.Background(), env)
	assert.ErrorIs(t, err, ErrPoisonPill)
}

// Handle returns "parse tenant_id" error when TenantID is not a UUID. The
// classify() step happens first — use a known event type so classify picks
// kindTenantLifecycle and we then hit the uuid.Parse call.
func TestP19Consumer_Handle_BadTenantID_ReturnsParseError(t *testing.T) {
	c := NewMembershipEventConsumer(nil, nil, nil, nil, nil, 5*time.Minute, nil)
	env := events.Envelope[json.RawMessage]{
		ID:        uuid.New().String(),
		Type:      "TenantSuspended", // known → kindTenantLifecycle
		TenantID:  "not-a-uuid",
		Timestamp: time.Now().UTC(),
	}
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse tenant_id")
}

// classify() coverage: exercise both known and unknown branches.
func TestP19Consumer_Classify_EnumCoverage(t *testing.T) {
	assert.Equal(t, kindTenantLifecycle, classify("TenantSuspended"))
	assert.Equal(t, kindTenantLifecycle, classify("TrialReactivated"))
	assert.Equal(t, kindBilling, classify("TenantPlanChanged"))
	assert.Equal(t, kindBilling, classify("TenantSeatsChanged"))
	assert.Equal(t, kindUnknown, classify("NeverHeardOfThis"))
	assert.Equal(t, kindUnknown, classify(""))
}

// applyProjection malformed-JSON branches — these run against the
// recordingTenants in membership_event_consumer_test.go and don't require
// a real pool.
func TestP19Consumer_ApplyProjection_TenantConverted_MalformedPayload(t *testing.T) {
	c, _ := newConsumer()
	env := events.Envelope[json.RawMessage]{
		Type:    "TenantConverted",
		Payload: json.RawMessage([]byte(`{not json`)),
	}
	_, _, err := applyOn(c, env, domain.StatusTrial, domain.TenantPlan("free"))
	assert.Error(t, err)
}

func TestP19Consumer_ApplyProjection_DirectPaidSignup_MalformedPayload(t *testing.T) {
	c, _ := newConsumer()
	env := events.Envelope[json.RawMessage]{
		Type:    "DirectPaidSignup",
		Payload: json.RawMessage([]byte(`{not json`)),
	}
	_, _, err := applyOn(c, env, domain.StatusTrial, domain.TenantPlan("free"))
	assert.Error(t, err)
}

func TestP19Consumer_ApplyProjection_TenantPlanChanged_MalformedPayload(t *testing.T) {
	c, _ := newConsumer()
	env := events.Envelope[json.RawMessage]{
		Type:    "TenantPlanChanged",
		Payload: json.RawMessage([]byte(`{not json`)),
	}
	_, _, err := applyOn(c, env, domain.StatusActive, domain.TenantPlan("free"))
	assert.Error(t, err)
}

func TestP19Consumer_ApplyProjection_TenantSeatsChanged_MalformedPayload(t *testing.T) {
	c, _ := newConsumer()
	env := events.Envelope[json.RawMessage]{
		Type:    "TenantSeatsChanged",
		Payload: json.RawMessage([]byte(`{not json`)),
	}
	_, _, err := applyOn(c, env, domain.StatusActive, domain.TenantPlan("free"))
	assert.Error(t, err)
}

// T-16 (resolves RP-11): TenantSuspended's "source" field decode failure.
func TestP19Consumer_ApplyProjection_TenantSuspended_MalformedPayload(t *testing.T) {
	c, _ := newConsumer()
	env := events.Envelope[json.RawMessage]{
		Type:    "TenantSuspended",
		Payload: json.RawMessage([]byte(`{not json`)),
	}
	_, _, err := applyOn(c, env, domain.StatusActive, domain.TenantPlan("free"))
	assert.Error(t, err)
}
