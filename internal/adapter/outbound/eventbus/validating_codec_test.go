package eventbus

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 18 · 0%-units sweep — ValidatingCodec was at 0% direct coverage.
// Every test carries the standard metadata registry pointer:
// full metadata in Reference_doc/Test_metadata_P12_P16.md is out of scope
// for the 0%-units sweep; these tests are documented via the T18 block in
// Test_cover.md.

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

// TestUnknownEventTypeFallsThroughToInnerCodec — an event
// type with no registered schema is a soft-error path: the wrapped codec
// still encodes. This preserves forward-compat for new event types added
// by producers before this service's schemas catch up.
func TestUnknownEventTypeFallsThroughToInnerCodec(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)

	payload := []byte(`{"anything":"goes"}`)
	out, _, err := c.Encode(context.Background(), "FutureEventTypeNotYetRegistered", payload)
	require.NoError(t, err, "unknown event type must be soft-pass, not hard-fail")
	assert.Equal(t, payload, out, "payload flows to inner codec unchanged")
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

	_, _, err = c.Encode(context.Background(), "DelegationStarted", []byte(`{}`))
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "DelegationStarted"),
		"error string must include the event type (got %q)", err.Error())
}
