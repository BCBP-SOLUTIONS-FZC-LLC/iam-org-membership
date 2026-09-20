package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/trace"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
)

// Publisher implements port.EventPublisher. It JSON-marshals the event
// payload, validates it via the configured Codec (schema validation only —
// no wire encoding), wraps it as plain JSON in an events.Envelope, and
// inserts it into outbox_events on the transaction TxRunner attached to
// ctx — atomic with the business write (EVT-10, CONS-1..4).
//
// Glue/wire encoding is NOT performed here. It is deferred to the SNS
// publisher (outbox runner publish path) via events.WithCodec so the
// outbox stores human-readable plain JSON for observability and replay.
type Publisher struct {
	source string
	codec  Codec
	log    port.Logger
}

// New creates a Publisher with the given source label (e.g.
// "iam-org-membership") and Codec. The Codec is used for JSON schema
// validation only; it must wrap a NoopCodec (not a Glue codec) so that
// outbox_events always stores plain JSON.
func New(source string, codec Codec) *Publisher {
	return &Publisher{source: source, codec: codec}
}

// WithLogger injects the shared gincommon Zap logger so enqueue diagnostics
// flow through the same sink as HTTP/consumer/outbound-client logs.
// Optional — production always injects logger.NewLogger; a nil logger
// skips debug/error lines (no slog.Default() fallback).
func (p *Publisher) WithLogger(log port.Logger) *Publisher {
	p.log = log
	return p
}

var _ port.EventPublisher = (*Publisher)(nil)

// Enqueue validates and writes one plain-JSON envelope into outbox_events
// on the transaction TxRunner stored in ctx. Glue encoding is deferred to
// the outbox runner's SNS publisher (WithCodec).
func (p *Publisher) Enqueue(ctx context.Context, event *domain.DomainEvent) error {
	raw, err := json.Marshal(event.Data)
	if err != nil {
		return fmt.Errorf("marshal event payload: %w", err)
	}
	// Schema validation only — codec must wrap NoopCodec so no wire encoding
	// occurs here. The returned bytes are discarded; we always store raw JSON.
	if _, _, err := p.codec.Encode(ctx, event.Type, raw); err != nil {
		return fmt.Errorf("validate event %s: %w", event.Type, err)
	}
	opts := []events.EnvelopeOpt{
		events.WithTenantID(event.TenantID.String()),
		events.WithSubject(event.Subject),
		events.WithActor(event.Actor),
		events.WithSchemaVersion("1"),
	}
	// User-initiated events only (§7.4 population rule).
	if event.IPAddress != "" {
		opts = append(opts, events.WithIPAddress(event.IPAddress))
	}
	if event.UserAgent != "" {
		opts = append(opts, events.WithUserAgent(event.UserAgent))
	}
	// Propagate OTel trace / span IDs from the calling request context.
	if spanCtx := trace.SpanFromContext(ctx).SpanContext(); spanCtx.IsValid() {
		opts = append(opts, events.WithTraceID(spanCtx.TraceID().String()))
		opts = append(opts, events.WithCorrelationID(spanCtx.SpanID().String()))
	}
	// Store plain JSON payload — Glue encoding happens at publish time.
	env := events.NewEnvelope(event.Type, p.source, json.RawMessage(raw), opts...)
	envBytes, _ := json.Marshal(env)
	if p.log != nil {
		p.log.Debug("outbox.Enqueue", map[string]any{"type": event.Type, "envelope": string(envBytes)})
	}
	tx, ok := port.TxFromContext(ctx)
	if !ok {
		return errors.New("event enqueue requires an open RunInTx transaction")
	}
	if err := outbox.Enqueue(ctx, tx, env); err != nil {
		if p.log != nil {
			p.log.Error("outbox.Enqueue failed", map[string]any{"type": event.Type, "error": err.Error(), "envelope": string(envBytes)})
		}
		return err
	}
	return nil
}
