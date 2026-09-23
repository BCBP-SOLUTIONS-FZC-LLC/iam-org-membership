package eventbus

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 18 · 0%-units sweep — ValidatingCodec was at 0% direct coverage.

// TestConstructionLoadsEmbeddedSchemas — NewValidatingCodec
// must succeed and populate the schema cache from the embedded FS.
func TestConstructionLoadsEmbeddedSchemas(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err, "constructor must not fail on repo's embedded schemas")
	assert.NotNil(t, c)
	// At least one schema should have loaded — the repo ships 10+ schemas
	// under eventbus/schemas/. The exact count is fluid (add-only during
	// spec evolution), so assert presence, not equality.
	assert.NotEmpty(t, c.schemas, "at least one schema must have been compiled")
}

// TestEncodeValidPayload — a payload matching the schema for
// its event type passes through to the inner codec.
func TestEncodeValidPayload(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)

	// TenantCreated schema requires tenant_id, slug, plan, status (per
	// event_payloads.go TenantCreatedPayload). NoopCodec echoes payload
	// back unchanged.
	payload := []byte(`{"tenant_id":"11111111-1111-1111-1111-111111111111","slug":"acme","plan":"starter","status":"trial"}`)
	out, schemaVer, err := c.Encode(context.Background(), "TenantCreated", payload)
	require.NoError(t, err, "valid payload must pass schema validation")
	assert.Equal(t, payload, out, "NoopCodec echoes the payload verbatim")
	assert.Empty(t, schemaVer, "NoopCodec returns empty schema version id")
}

// TestEncodeValidPayload_MFAReset — the new §16 OQ-8/F6 event's schema
// (tenant_id, user_id, actor_id, all required) matches domain.MFAResetPayload.
func TestEncodeValidPayload_MFAReset(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)

	payload := []byte(`{"tenant_id":"11111111-1111-1111-1111-111111111111","user_id":"22222222-2222-2222-2222-222222222222","actor_id":"33333333-3333-3333-3333-333333333333"}`)
	out, _, err := c.Encode(context.Background(), "MFAReset", payload)
	require.NoError(t, err, "valid MFAReset payload must pass schema validation")
	assert.Equal(t, payload, out)
}

// TestEncodeRejectsMissingRequiredField — a payload missing a
// required field must produce a descriptive validation error.
func TestEncodeRejectsMissingRequiredField(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)

	// TenantCreated requires slug; omit it.
	bad := []byte(`{"tenant_id":"11111111-1111-1111-1111-111111111111","plan":"starter","status":"trial"}`)
	_, _, err = c.Encode(context.Background(), "TenantCreated", bad)
	require.Error(t, err, "missing-field payload must fail validation")
	assert.Contains(t, err.Error(), "validate TenantCreated",
		"error must name the event type so producers can trace")
	assert.Contains(t, err.Error(), "slug",
		"error must name the missing field so producers can fix it")
}

// TestEncodeRejectsInvalidJSON — non-JSON bytes are caught
// before the schema library sees them; the error indicates JSON parse
// failure specifically.
func TestEncodeRejectsInvalidJSON(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)

	_, _, err = c.Encode(context.Background(), "TenantCreated", []byte(`not-json{`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not JSON",
		"error must indicate the payload is malformed JSON, not a schema mismatch")
}

// TestUnknownEventTypeFailsClosed — an event type with no
// registered schema must be rejected (EVT-10 fail-closed), never passed
// through to the inner codec. A new event type shipped without a matching
// schema-gov extract must never reach the outbox unvalidated.
func TestUnknownEventTypeFailsClosed(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)

	payload := []byte(`{"anything":"goes"}`)
	out, _, err := c.Encode(context.Background(), "FutureEventTypeNotYetRegistered", payload)
	require.Error(t, err, "unknown event type must fail closed, not soft-pass")
	assert.Contains(t, err.Error(), "FutureEventTypeNotYetRegistered",
		"error must name the event type so producers can trace it")
	assert.Nil(t, out, "no bytes must be produced for a rejected event type")
}

// TestConcurrentEncodeSafe — the RWMutex-guarded map must
// tolerate concurrent Encode calls without a data race. Run under
// `go test -race` to catch a broken lock discipline.
func TestConcurrentEncodeSafe(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)

	payload := []byte(`{"tenant_id":"11111111-1111-1111-1111-111111111111","slug":"acme","plan":"starter","status":"trial"}`)
	done := make(chan struct{}, 4)
	for i := 0; i < 4; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 25; j++ {
				_, _, e := c.Encode(context.Background(), "TenantCreated", payload)
				if e != nil {
					t.Errorf("unexpected encode error: %v", e)
					return
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
}

// TestValidationErrorMentionsEventType — regression guard:
// upstream error text must always name the event type so consumers can
// route the error to the responsible schema owner.
func TestValidationErrorMentionsEventType(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)

	_, _, err = c.Encode(context.Background(), "MembershipRevoked", []byte(`{}`))
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "MembershipRevoked"),
		"error string must include the event type (got %q)", err.Error())
}

// ── Validate — shared by Encode and the inbound consumer ─────────────────

func TestValidatingCodec_Validate_UnknownType_WrapsErrNoSchema(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)
	assert.ErrorIs(t, c.Validate("NoSuchEvent", []byte(`{}`)), ErrNoSchema)
}

func TestValidatingCodec_Validate_ConsumedSchema_Enforced(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)
	require.NoError(t, c.Validate("TenantSeatsChanged", []byte(`{"licensed_seats":10}`)))
	err = c.Validate("TenantSeatsChanged", []byte(`{}`))
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNoSchema, "a violation must be distinguishable from a missing schema")
}
