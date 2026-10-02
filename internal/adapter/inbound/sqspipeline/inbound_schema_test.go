package sqspipeline

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events/mock"
)

// realValidator is the production ValidatingCodec over the embedded
// schemas/*.json, adapted exactly as cmd/server does (ErrNoSchema → valid),
// so these tests exercise the actual consumed contracts.
func realValidator(t *testing.T) Validator {
	t.Helper()
	v, err := eventbusadapter.NewValidatingCodec(eventbusadapter.NoopCodec{})
	require.NoError(t, err)
	return ValidatorFunc(func(eventType string, payload []byte) error {
		if err := v.Validate(eventType, payload); !errors.Is(err, eventbusadapter.ErrNoSchema) {
			return err
		}
		return nil
	})
}

// recordingHandler counts calls and returns err.
type recordingHandler struct {
	calls int
	err   error
}

func (r *recordingHandler) handle(context.Context, events.Envelope[json.RawMessage]) error {
	r.calls++
	return r.err
}

func envelopeOf(eventType, payload string) events.Envelope[json.RawMessage] {
	env := testEnvelope()
	env.Type = eventType
	env.Payload = json.RawMessage(payload)
	return env
}

func TestValidateConsumed_ValidPayload_ReachesHandler(t *testing.T) {
	inner := &recordingHandler{}
	h := validateConsumed(inner.handle, realValidator(t), nil)
	require.NoError(t, h(context.Background(), envelopeOf("TenantSuspended", `{"tenant_id":"t","realm":"r","suspended_at":"2026-09-23T00:00:00Z","source":"operator"}`)))
	assert.Equal(t, 1, inner.calls)
}

func TestValidateConsumed_OpenSchema_ExtraFieldsAccepted(t *testing.T) {
	inner := &recordingHandler{}
	h := validateConsumed(inner.handle, realValidator(t), nil)
	require.NoError(t, h(context.Background(), envelopeOf("TenantSeatsChanged", `{"licensed_seats":25,"added_later":"ok"}`)))
	assert.Equal(t, 1, inner.calls, "additionalProperties: true — new producer fields must not be rejected")
}

func TestValidateConsumed_Violations_RejectedBeforeHandler(t *testing.T) {
	cases := map[string]events.Envelope[json.RawMessage]{
		"missing required field": envelopeOf("TenantSeatsChanged", `{}`),
		"wrong type":             envelopeOf("TenantSeatsChanged", `{"licensed_seats":"25"}`),
		"enum violation":         envelopeOf("TenantSuspended", `{"source":"hacker"}`),
		"not JSON":               envelopeOf("TenantConverted", `not-json`),
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			inner := &recordingHandler{}
			log := &fakeLogger{}
			h := validateConsumed(inner.handle, realValidator(t), log)
			err := h(context.Background(), env)
			assert.ErrorIs(t, err, errSchemaViolation)
			assert.Zero(t, inner.calls, "a violating payload must never reach the projection")
			assert.Equal(t, int32(1), log.warnCalls)
			reason, ok := dlqReason(err)
			assert.True(t, ok)
			assert.Equal(t, schemaViolationReason, reason)
		})
	}
}

func TestValidateConsumed_UnknownType_PassesThroughForAckUnknown(t *testing.T) {
	inner := &recordingHandler{}
	h := validateConsumed(inner.handle, realValidator(t), nil)
	require.NoError(t, h(context.Background(), envelopeOf("SomeFutureEvent", `{"anything":true}`)))
	assert.Equal(t, 1, inner.calls)
}

func TestValidateConsumed_HandlerErrorPropagatesUnchanged(t *testing.T) {
	transient := errors.New("db down")
	inner := &recordingHandler{err: transient}
	h := validateConsumed(inner.handle, realValidator(t), nil)
	err := h(context.Background(), envelopeOf("TenantSeatsChanged", `{"licensed_seats":1}`))
	assert.Equal(t, transient, err)
	_, ok := dlqReason(err)
	assert.False(t, ok, "transient handler errors must keep normal retry semantics")
}

// TestInboundHandler_ValidEvent_NoDLQ: a conforming event reaches the
// handler and nothing is dead-lettered. (The violation path, through a real
// consumer, is TestConsumer_SchemaViolation_ForwardedThroughInboundPipeline.)
func TestInboundHandler_ValidEvent_NoDLQ(t *testing.T) {
	dlq := &mock.DLQPublisher{}
	inner := &recordingHandler{}
	h := inboundHandler("tenant-orgm-q", inner.handle, realValidator(t), dlq, &fakeLogger{})

	require.NoError(t, h(context.Background(), envelopeOf("TenantSeatsChanged", `{"licensed_seats":3}`)))
	assert.Equal(t, 1, inner.calls)
	assert.Empty(t, dlq.Sent())
}

// TestValidateConsumed_TenantRealmReady_MatchesRealmProvisionerShape pins
// the consumed schema to RP's actual producer payload ("realm", not the
// stale "realm_id") — so a real RP event is accepted and the old shape,
// which would have written an empty tenants.realm_id, is rejected.
func TestValidateConsumed_TenantRealmReady_MatchesRealmProvisionerShape(t *testing.T) {
	rpPayload := `{"tenant_id":"6f1c2c1e-0000-4000-8000-000000000001","realm":"acme-realm","keycloak_shard":"shard-1","oidc_clients":["web","mobile"]}`
	inner := &recordingHandler{}
	h := validateConsumed(inner.handle, realValidator(t), nil)
	require.NoError(t, h(context.Background(), envelopeOf("TenantRealmReady", rpPayload)))
	assert.Equal(t, 1, inner.calls)

	for name, payload := range map[string]string{
		"stale realm_id shape": `{"realm_id":"acme-realm","realm_type":"dedicated","keycloak_shard":"shard-1"}`,
		"missing shard":        `{"realm":"acme-realm"}`,
		"empty realm":          `{"realm":"","keycloak_shard":"shard-1"}`,
	} {
		t.Run(name, func(t *testing.T) {
			inner := &recordingHandler{}
			h := validateConsumed(inner.handle, realValidator(t), nil)
			assert.ErrorIs(t, h(context.Background(), envelopeOf("TenantRealmReady", payload)), errSchemaViolation)
			assert.Zero(t, inner.calls)
		})
	}
}
