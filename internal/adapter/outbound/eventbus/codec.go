// Package eventbus implements the outbound event bus: a transactional
// Publisher (writes envelopes into outbox_events atomically with the
// business tx, EVT-10) plus a RoutingPublisher that dispatches published
// envelopes to one of two SNS topics based on the event type
// (iam.membership.events vs iam.tenant.events, §7.3).
package eventbus

import "context"

// Codec encodes event payloads before they are written to the outbox.
// Returns the encoded bytes and the schema version ID used (a Glue schema
// version UUID for GlueCodec, empty string for NoopCodec). Callers echo the
// version ID into the envelope's schema_version field so the JSON body
// carries the same registry pointer that is already present in the 18-byte
// Glue wire-format header.
type Codec interface {
	Encode(ctx context.Context, schemaName string, payload []byte) (encoded []byte, schemaVersionID string, err error)
}

// NoopCodec returns payloads unchanged. Used in dev/test environments where
// no Glue registry is configured. When GLUE_REGISTRY_NAME is unset the
// composition root wires this codec.
type NoopCodec struct{}

// Encode passes payload through untouched. Schema version ID is empty so
// the envelope's SchemaID option is skipped by the caller.
func (NoopCodec) Encode(_ context.Context, _ string, payload []byte) ([]byte, string, error) {
	return payload, "", nil
}
