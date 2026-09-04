package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
	"github.com/google/uuid"
)

const (
	// glueHeaderVersion is the magic byte for the AWS Glue Schema Registry wire format.
	glueHeaderVersion byte = 0x03
	// glueNoCompression signals an uncompressed payload.
	glueNoCompression byte = 0x00
	// glueHeaderSize = 1 (version) + 1 (compression) + 16 (schema version UUID) = 18 bytes.
	glueHeaderSize = 18
)

// GlueCodec implements platform-events' events.Codec (pkg/events, aliasing
// port.Codec) directly — no local interface is defined here. It is injected
// per-topic via events.WithCodec at the SNS-publish step (see
// cmd/server/main.go's buildTopicPublisher), not at outbox-enqueue time: the
// transactional outbox always stores plain, validated JSON regardless of
// which codec is configured (ValidatingCodec's job); wire-format encoding
// (the Glue header) is applied transiently by the SNS publisher immediately
// before publish, and the encoded bytes are base64-wrapped into a JSON
// string by platform-events itself (domain.WrapCodecPayload) so
// outbox_events.payload is never touched — see port.Codec's own doc comment
// in platform-events for the exact contract this relies on.
//
// One GlueCodec instance is scoped to exactly one Glue registry (SCHEMA-7 —
// this service uses two registries, iam-membership-events and
// iam-tenant-events, one per SNS topic): construct one per topic with the
// schema names that belong to that topic (see AllSchemaNames +
// domain.TopicForEvent in cmd/server/main.go for how the 14 embedded
// schemas are split 12/2 between the two).
//
// Unlike the sibling iam-user-profile service, this service's event Type
// strings (e.g. "DepartmentMembershipGranted") ARE already the PascalCase
// Glue schema name — the on-disk schemas/*.json filename stem, the
// envelope.type value, and the Glue registry schema name are all the exact
// same string, so no dot-notation-to-PascalCase translation is needed here
// (contrast iam-user-profile's domain.GlueSchemaName helper).
type GlueCodec struct {
	client       *glue.Client
	registryName string
	mu           sync.RWMutex
	versionCache map[string]string // schema name → schema version UUID string
	log          port.SlogStyleLogger
}

var _ events.Codec = (*GlueCodec)(nil)

// WithLogger routes StartRefresher's background refresh-failure warnings
// through the same gincommon-backed sink as the rest of the service.
// Optional — the zero value falls back to the top-level slog functions.
func (g *GlueCodec) WithLogger(log port.Logger) *GlueCodec {
	g.log = port.NewSlogStyleLogger(log)
	return g
}

// NewGlueCodec creates a GlueCodec and pre-fetches the latest schema version
// ID for each name in schemaNames from registryName. Fails fast at startup
// if any lookup fails — the alternative is a silent publish failure on the
// first event of that type.
func NewGlueCodec(ctx context.Context, client *glue.Client, registryName string, schemaNames []string) (*GlueCodec, error) {
	c := &GlueCodec{
		client:       client,
		registryName: registryName,
		versionCache: make(map[string]string, len(schemaNames)),
	}
	for _, name := range schemaNames {
		id, err := c.fetchVersionID(ctx, name)
		if err != nil {
			return nil, fmt.Errorf(
				"prefetch glue schema %q in registry %q: %w — "+
					"run `make schema-verify` to confirm the expected schemas exist",
				name, registryName, err,
			)
		}
		c.versionCache[name] = id
	}
	return c, nil
}

// Encode looks up the cached schema version ID for schemaName, prepends the
// 18-byte Glue wire-format header to payload, and returns the schema version
// UUID so platform-events can echo it into the envelope's SchemaID field.
func (g *GlueCodec) Encode(ctx context.Context, schemaName string, payload json.RawMessage) ([]byte, string, error) {
	versionID, err := g.versionID(ctx, schemaName)
	if err != nil {
		return nil, "", fmt.Errorf("get glue schema version %q: %w", schemaName, err)
	}
	out, err := prependGlueHeader(versionID, payload)
	if err != nil {
		return nil, "", err
	}
	return out, versionID, nil
}

// Decode strips the 18-byte Glue wire-format header from encoded and returns
// the remaining plain-JSON payload. schemaID is accepted for events.Codec
// interface compatibility but isn't needed to strip the header — the schema
// version UUID is self-contained in the encoded bytes at offset 2:18 — so it
// isn't cross-checked against anything here.
func (g *GlueCodec) Decode(_ context.Context, _ string, encoded []byte) (json.RawMessage, error) {
	return stripGlueHeader(encoded)
}

// stripGlueHeader removes the 18-byte Glue wire-format header, returning the
// remaining plain-JSON payload. Factored out of Decode so it can be tested
// without constructing a *GlueCodec.
func stripGlueHeader(encoded []byte) (json.RawMessage, error) {
	if len(encoded) < glueHeaderSize {
		return nil, fmt.Errorf("glue codec: encoded payload is %d bytes — shorter than the %d-byte Glue header", len(encoded), glueHeaderSize)
	}
	if encoded[0] != glueHeaderVersion {
		return nil, fmt.Errorf("glue codec: unexpected header version byte 0x%02x — want 0x%02x", encoded[0], glueHeaderVersion)
	}
	return json.RawMessage(encoded[glueHeaderSize:]), nil
}

func (g *GlueCodec) versionID(ctx context.Context, schemaName string) (string, error) {
	g.mu.RLock()
	id, ok := g.versionCache[schemaName]
	g.mu.RUnlock()
	if ok {
		return id, nil
	}
	id, err := g.fetchVersionID(ctx, schemaName)
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	g.versionCache[schemaName] = id
	g.mu.Unlock()
	return id, nil
}

func (g *GlueCodec) fetchVersionID(ctx context.Context, schemaName string) (string, error) {
	out, err := g.client.GetSchemaVersion(ctx, &glue.GetSchemaVersionInput{
		SchemaId: &gluetypes.SchemaId{
			SchemaName:   &schemaName,
			RegistryName: &g.registryName,
		},
		SchemaVersionNumber: &gluetypes.SchemaVersionNumber{
			LatestVersion: true,
		},
	})
	if err != nil {
		return "", err
	}
	if out.SchemaVersionId == nil {
		return "", fmt.Errorf("nil SchemaVersionId for schema %q in registry %q", schemaName, g.registryName)
	}
	return *out.SchemaVersionId, nil
}

// StartRefresher runs a background goroutine that re-fetches every cached
// schema version ID at the given interval, so a new schema version
// registered in Glue is picked up without a pod restart. Exits when ctx is
// cancelled. Fetch errors are non-fatal — the stale cached ID remains in use
// until the next successful refresh.
func (g *GlueCodec) StartRefresher(ctx context.Context, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				g.mu.RLock()
				names := make([]string, 0, len(g.versionCache))
				for name := range g.versionCache {
					names = append(names, name)
				}
				g.mu.RUnlock()
				for _, name := range names {
					if id, err := g.fetchVersionID(ctx, name); err == nil {
						g.mu.Lock()
						g.versionCache[name] = id
						g.mu.Unlock()
					} else {
						g.log.Warn("glue schema version refresh failed — using cached ID", "schema", name, "error", err.Error())
					}
				}
			}
		}
	}()
}

func prependGlueHeader(schemaVersionID string, payload []byte) ([]byte, error) {
	id, err := uuid.Parse(schemaVersionID)
	if err != nil {
		return nil, fmt.Errorf("parse schema version UUID %q: %w", schemaVersionID, err)
	}
	out := make([]byte, glueHeaderSize+len(payload))
	out[0] = glueHeaderVersion
	out[1] = glueNoCompression
	copy(out[2:18], id[:])
	copy(out[glueHeaderSize:], payload)
	return out, nil
}

// AllSchemaNames returns every embedded schemas/*.json file's name (without
// the .json extension) — the single source of truth cmd/server/main.go uses
// to build the per-registry schema-name lists NewGlueCodec needs, so the
// membership/tenant split can never drift from what ValidatingCodec actually
// loads.
func AllSchemaNames() ([]string, error) {
	entries, err := schemasFS.ReadDir("schemas")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	return names, nil
}
