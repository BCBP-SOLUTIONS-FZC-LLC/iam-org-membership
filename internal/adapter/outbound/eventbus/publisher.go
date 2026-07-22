package eventbus

import (
	"context"
	"encoding/json"
	"fmt"

	"go.opentelemetry.io/otel/trace"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/jackc/pgx/v5"
)

// Publisher implements port.EventPublisher. It JSON-marshals the event
// payload, encodes it via the configured Codec (Glue wire format in
// production, noop in dev), wraps it in an events.Envelope[json.RawMessage],
// and inserts it into outbox_events within the caller's active pgx.Tx —
// atomic with the business write (EVT-10, CONS-1..4).
type Publisher struct {
	source string
	codec  Codec
}

// New creates a Publisher with the given source label (e.g.
// "iam-org-membership") and Codec.
func New(source string, codec Codec) *Publisher {
	return &Publisher{source: source, codec: codec}
}

// EnqueueInTx is the alias the inbound consumer uses to emit
// TenantStateChanged (EVT-16) inside its projection tx. Same body as
// Enqueue; kept as a named method so the consumer package can depend on
// a narrow interface without importing the port package's tx type.
func (p *Publisher) EnqueueInTx(ctx context.Context, tx pgx.Tx, event *domain.DomainEvent) error {
	return p.Enqueue(ctx, tx, event)
}

// Enqueue writes one envelope into outbox_events. Called by service code
// via port.EventPublisherFromContext(txCtx).EnqueueCtx after the business
// row is inserted/updated in the same transaction.
func (p *Publisher) Enqueue(ctx context.Context, tx pgx.Tx, event *domain.DomainEvent) error {
	raw, err := json.Marshal(event.Data)
	if err != nil {
		return fmt.Errorf("marshal event payload: %w", err)
	}
	encoded, schemaVersionID, err := p.codec.Encode(ctx, event.Type, raw)
	if err != nil {
		return fmt.Errorf("encode event %s: %w", event.Type, err)
	}
	opts := []events.EnvelopeOpt{
		events.WithTenantID(event.TenantID.String()),
		events.WithSubject(event.Subject),
		events.WithActor(event.Actor),
		events.WithSchemaVersion("1"),
	}
	// Echo the Glue schema version UUID as SchemaID so the JSON body carries
	// the same registry pointer already present in the 18-byte Glue header.
	// Omitted for NoopCodec (dev/test) — schemaVersionID is empty.
	if schemaVersionID != "" {
		opts = append(opts, events.WithSchemaID(schemaVersionID))
	}
	// User-initiated events only (§7.4 population rule) — system/background
	// callers leave both empty.
	if event.IPAddress != "" {
		opts = append(opts, events.WithIPAddress(event.IPAddress))
	}
	if event.UserAgent != "" {
		opts = append(opts, events.WithUserAgent(event.UserAgent))
	}
	// Propagate OTel trace / span IDs from the calling request context so
	// consumers can correlate the event to the originating request span.
	if spanCtx := trace.SpanFromContext(ctx).SpanContext(); spanCtx.IsValid() {
		opts = append(opts, events.WithTraceID(spanCtx.TraceID().String()))
		opts = append(opts, events.WithCorrelationID(spanCtx.SpanID().String()))
	}
	env := events.NewEnvelope(event.Type, p.source, json.RawMessage(encoded), opts...)
	return insertEnvelope(ctx, tx, env)
}

// insertEnvelope writes the envelope into outbox_events using
// json.RawMessage for the payload column so pgx emits JSON text under
// simple-protocol mode (PgBouncer transaction pooling) — bytea hex would
// not be valid JSONB and would corrupt the row.
func insertEnvelope(ctx context.Context, tx pgx.Tx, env events.Envelope[json.RawMessage]) error {
	// json.Marshal on an Envelope with json.RawMessage payload cannot fail —
	// every field is string/UUID/time.Time/json.RawMessage.
	b, _ := json.Marshal(env)
	const maxEnvelopeBytes = 240 * 1024
	if len(b) > maxEnvelopeBytes {
		return fmt.Errorf("outbox: envelope is %d bytes — exceeds SNS 256KB limit (leaving 16KB for SNS overhead)", len(b))
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO outbox_events
			(id, event_type, payload, tenant_id, trace_id, created_at, scheduled_at)
		VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
	`,
		env.ID,
		env.Type,
		json.RawMessage(b),
		env.TenantID,
		env.TraceID,
	)
	return err
}
