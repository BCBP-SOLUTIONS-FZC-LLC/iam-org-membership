package eventbus

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// schemasFS embeds the JSON Schema Draft-07 files under this package's
// sibling `schemas/` directory. schema-gov extract 0.4 writes one snake_case
// file per AsyncAPI *Payload (produced and consumed). ValidatingCodec keys
// the compiled schema by the schemaFileNames map (glue_codec.go) so Encode
// still looks up the PascalCase envelope.type / Glue name.
//
//go:embed schemas/*.json
var schemasFS embed.FS

// ValidatingCodec wraps another Codec and validates the payload JSON against
// the embedded JSON Schema for its event type before delegating the encode.
// A validation failure fails the outbox insert (EVT-10) — a malformed event
// is rejected at the source rather than discovered by a downstream consumer.
type ValidatingCodec struct {
	inner   Codec
	schemas map[string]*jsonschema.Schema
	mu      sync.RWMutex
}

// NewValidatingCodec loads every schemas/*.json file at construction and
// caches the compiled schemas. Encode fails closed (EVT-10): a payload for
// an event type with no compiled schema is rejected rather than passed
// through unvalidated, so an event type added without a matching
// schema-gov extract can never reach the outbox.
func NewValidatingCodec(inner Codec) (*ValidatingCodec, error) {
	return newValidatingCodecFromFS(inner, schemasFS)
}

// newValidatingCodecFromFS is the testable implementation of NewValidatingCodec.
// It accepts an fs.FS so tests can inject a fstest.MapFS to trigger each error
// branch (ReadDir error, non-.json continue, json.Unmarshal error, compile error).
func newValidatingCodecFromFS(inner Codec, schemas fs.FS) (*ValidatingCodec, error) {
	c := &ValidatingCodec{inner: inner, schemas: make(map[string]*jsonschema.Schema)}
	entries, err := fs.ReadDir(schemas, "schemas")
	if err != nil {
		// Zero embedded files → nothing to load. Composition still works;
		// validation is a no-op until Phase 3 lands the real event schemas.
		// This is a deliberate soft-fail so scaffolding builds and boots
		// without Phase 3 artefacts.
		return c, nil //nolint:nilerr // intentional soft-fail
	}
	compiler := jsonschema.NewCompiler()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, rerr := fs.ReadFile(schemas, path.Join("schemas", e.Name()))
		if rerr != nil {
			return nil, fmt.Errorf("read schema %s: %w", e.Name(), rerr)
		}
		name := eventTypeFromSchemaFile(e.Name())
		var doc any
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse schema %s: %w", e.Name(), err)
		}
		if err := compiler.AddResource(name, doc); err != nil {
			return nil, fmt.Errorf("register schema %s: %w", e.Name(), err)
		}
		sch, err := compiler.Compile(name)
		if err != nil {
			return nil, fmt.Errorf("compile schema %s: %w", e.Name(), err)
		}
		c.schemas[name] = sch
	}
	return c, nil
}

// Encode validates payload against the schema registered for eventType, then
// delegates encoding to the inner codec. Fails closed: an eventType with no
// compiled schema is an error, not a skipped/soft-passed validation, so a
// new event type shipped without a matching embedded schema can never reach
// the outbox unvalidated.
func (c *ValidatingCodec) Encode(ctx context.Context, eventType string, payload []byte) ([]byte, string, error) {
	c.mu.RLock()
	sch, ok := c.schemas[eventType]
	c.mu.RUnlock()
	if !ok {
		return nil, "", fmt.Errorf("validate %s: no compiled schema registered for this event type", eventType)
	}
	var doc any
	if err := json.Unmarshal(payload, &doc); err != nil {
		return nil, "", fmt.Errorf("validate %s: payload is not JSON: %w", eventType, err)
	}
	if err := sch.Validate(doc); err != nil {
		return nil, "", fmt.Errorf("validate %s: %w", eventType, err)
	}
	return c.inner.Encode(ctx, eventType, payload)
}
