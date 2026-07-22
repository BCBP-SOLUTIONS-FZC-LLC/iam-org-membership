package eventbus

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// schemasFS embeds the JSON Schema Draft-07 files under
// internal/eventschema/. Phase 3 populates it with one file per outbound
// event type per §7.3. Until then it is empty and ValidatingCodec
// gracefully passes payloads through.
//
// The relative-path embed reaches out of adapter/outbound/eventbus/ into
// the top-level internal/eventschema/ workspace so the schema files can be
// treated as the design-time source of truth alongside asyncapi.yaml.
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
// caches the compiled schemas. A missing schema for a given event type
// causes validation to be skipped (a warning is emitted by the caller if
// desired) — the wrapped codec still encodes so unknown-type propagation
// is a soft error, not a hard failure.
func NewValidatingCodec(inner Codec) (*ValidatingCodec, error) {
	c := &ValidatingCodec{inner: inner, schemas: make(map[string]*jsonschema.Schema)}
	entries, err := schemasFS.ReadDir("schemas")
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
		name := strings.TrimSuffix(e.Name(), ".json")
		data, rerr := schemasFS.ReadFile(path.Join("schemas", e.Name()))
		if rerr != nil {
			return nil, fmt.Errorf("read schema %s: %w", e.Name(), rerr)
		}
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
// delegates encoding to the inner codec.
func (c *ValidatingCodec) Encode(ctx context.Context, eventType string, payload []byte) ([]byte, string, error) {
	c.mu.RLock()
	sch, ok := c.schemas[eventType]
	c.mu.RUnlock()
	if ok {
		var doc any
		if err := json.Unmarshal(payload, &doc); err != nil {
			return nil, "", fmt.Errorf("validate %s: payload is not JSON: %w", eventType, err)
		}
		if err := sch.Validate(doc); err != nil {
			return nil, "", fmt.Errorf("validate %s: %w", eventType, err)
		}
	}
	return c.inner.Encode(ctx, eventType, payload)
}
