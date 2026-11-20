// BUG-18 (LLD L2346: "Reads only code; a missing or unknown code bumps every
// plan"): the consumed PlanCatalogChanged schema must let an unknown or
// missing plan code reach the catalog consumer instead of rejecting it to
// the DLQ as schema_violation. Exercised through the real inbound pipeline
// (sqspipeline.Handler) with the real embedded schemas, adapted as
// cmd/server does (ErrNoSchema → valid).
package unit_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/sqspipeline"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events/mock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// b18Run sends one PlanCatalogChanged payload through the inbound pipeline
// and reports whether the consumer handler ran and whether the pipeline
// rejected it as a schema violation. (Forwarding to the DLQ needs the SQS
// source message in the context, which a unit call has none of, so the
// pipeline returns the schema-violation error instead — dlq.go:100-103.)
func b18Run(t *testing.T, payload string) (handled bool, schemaViolation bool) {
	t.Helper()
	v, err := eventbus.NewValidatingCodec(eventbus.NoopCodec{})
	require.NoError(t, err)
	validator := sqspipeline.ValidatorFunc(func(eventType string, p []byte) error {
		if err := v.Validate(eventType, p); !errors.Is(err, eventbus.ErrNoSchema) {
			return err
		}
		return nil
	})
	dlq := &mock.DLQPublisher{}
	inner := func(context.Context, events.Envelope[json.RawMessage]) error { handled = true; return nil }
	h := sqspipeline.Handler("catalog-orgm-q", inner, validator, dlq, nil)
	env := events.Envelope[json.RawMessage]{
		ID: uuid.NewString(), Type: "PlanCatalogChanged", Source: "iam-catalog-admin",
		TenantID: "00000000-0000-0000-0000-000000000000", SchemaID: uuid.NewString(),
		Timestamp: time.Now().UTC(), Payload: json.RawMessage(payload),
	}
	err = h(context.Background(), env)
	schemaViolation = err != nil && strings.Contains(err.Error(), "violates its embedded schema")
	return handled, schemaViolation
}

// RED before the fix: an unknown plan code reaches the handler (which bumps every plan).
func TestBUG18_UnknownPlanCode_ReachesHandler(t *testing.T) {
	handled, dlqd := b18Run(t, `{"code":"business","change_type":"updated","feature_set":{}}`)
	assert.True(t, handled, "LLD L2346: an unknown code bumps every plan")
	assert.False(t, dlqd)
}

// RED before the fix: a payload without code reaches the handler.
func TestBUG18_MissingPlanCode_ReachesHandler(t *testing.T) {
	handled, dlqd := b18Run(t, `{"change_type":"updated","feature_set":{}}`)
	assert.True(t, handled, "LLD L2346: a missing code bumps every plan")
	assert.False(t, dlqd)
}

// Control: a known code passes, as today.
func TestBUG18_KnownPlanCode_ReachesHandler(t *testing.T) {
	handled, dlqd := b18Run(t, `{"code":"pro","change_type":"updated","feature_set":{}}`)
	assert.True(t, handled)
	assert.False(t, dlqd)
}

// Control (unchanged): change_type and feature_set stay required.
func TestBUG18_MissingChangeTypeOrFeatureSet_StillSchemaViolation(t *testing.T) {
	for name, payload := range map[string]string{
		"no change_type": `{"code":"pro","feature_set":{}}`,
		"no feature_set": `{"code":"pro","change_type":"updated"}`,
	} {
		t.Run(name, func(t *testing.T) {
			handled, dlqd := b18Run(t, payload)
			assert.False(t, handled, "rejected before the handler")
			assert.True(t, dlqd, "rejected as schema_violation (the DLQ route)")
		})
	}
}
