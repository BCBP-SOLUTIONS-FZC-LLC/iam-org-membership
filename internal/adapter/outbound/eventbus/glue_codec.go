package eventbus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
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
// Version resolution is by content, not by recency: each schema's version
// UUID is looked up with GetSchemaByDefinition using this binary's own
// embedded schemas/*.json, so every event is stamped with the version this
// binary actually produces — never merely the registry's latest, which can
// run ahead of (registered but not yet deployed) or behind (deploy raced
// registration, or a rollback) the running code. The UUID is therefore
// fixed for the life of the process; there is no background refresh. A
// definition that isn't registered yet fails startup (see NewGlueCodec), so
// a deploy that outruns schema-registry.yml CrashLoops until registration
// lands, then starts with the correct version.
//
// One GlueCodec instance is scoped to exactly one Glue registry (SCHEMA-7 —
// this service uses two registries, iam-membership-events and
// iam-tenant-events, one per SNS topic): construct one per topic with the
// schema names that belong to that topic (see AllSchemaNames +
// domain.TopicForEvent in cmd/server/main.go for how the 14 embedded
// schemas are split 12/2 between the two).
//
// Event Type strings (e.g. "DepartmentMembershipGranted") are the
// PascalCase Glue schema name and envelope.type. schema-gov extract 0.4
// writes snake_case filenames (tenant_created.json, mfa_reset.json);
// AllSchemaNames / ValidatingCodec recover the Glue name via the
// schemaFileNames map below, the same hand-maintained
// filename→PascalCase-name convention as the sibling services'
// eventschema.ByEventType (iam-realm-provisioner, iam-delegation,
// iam-user-profile) — a new schema needs one line added there.
type GlueCodec struct {
	client       *glue.Client
	registryName string
	schemas      fs.FS // holds schemas/*.json — the definitions looked up in Glue
	mu           sync.RWMutex
	versionCache map[string]string // schema name → schema version UUID string
}

var _ events.Codec = (*GlueCodec)(nil)

// NewGlueCodec creates a GlueCodec and resolves, for each name in
// schemaNames, the Glue schema version whose definition matches this
// binary's embedded schema file (GetSchemaByDefinition). Fails fast at
// startup if any definition isn't registered (and AVAILABLE) in
// registryName — the alternative is stamping events with a version that
// doesn't describe them, or a publish failure on the first event of that
// type.
func NewGlueCodec(ctx context.Context, client *glue.Client, registryName string, schemaNames []string) (*GlueCodec, error) {
	return newGlueCodecFromFS(ctx, client, registryName, schemaNames, schemasFS)
}

// newGlueCodecFromFS is NewGlueCodec over an injectable schema FS (tests).
func newGlueCodecFromFS(ctx context.Context, client *glue.Client, registryName string, schemaNames []string, schemas fs.FS) (*GlueCodec, error) {
	c := &GlueCodec{
		client:       client,
		registryName: registryName,
		schemas:      schemas,
		versionCache: make(map[string]string, len(schemaNames)),
	}
	for _, name := range schemaNames {
		id, err := c.fetchVersionID(ctx, name)
		if err != nil {
			return nil, fmt.Errorf(
				"resolve glue schema %q in registry %q by definition: %w — "+
					"this binary's schema version isn't registered yet: wait for "+
					"schema-registry.yml to register it, or run `make schema-verify`",
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

// GlueDecoder is the consumer-side events.Codec, injected on every inbound
// SQS consumer via events.WithConsumerCodec. Upstream producers (Realm
// Provisioner, Billing, Catalog) encode into their own Glue registries,
// which this service never prefetches from — and doesn't need to, because
// Decode only strips the self-describing 18-byte header. So unlike
// GlueCodec this needs no Glue client or registry name. platform-events
// only calls Decode when envelope.dataschema is non-empty, so plain-JSON
// producers (NoopCodec, dev/test) bypass it entirely.
type GlueDecoder struct{}

var _ events.Codec = GlueDecoder{}

// Encode always fails — GlueDecoder is never wired on a publisher. Use
// GlueCodec (one per owned registry) for the SNS publish path.
func (GlueDecoder) Encode(_ context.Context, schemaName string, _ json.RawMessage) ([]byte, string, error) {
	return nil, "", fmt.Errorf("glue decoder: Encode(%q) called on a decode-only codec — use GlueCodec on publishers", schemaName)
}

// Decode strips the 18-byte Glue wire-format header, same as GlueCodec.Decode.
func (GlueDecoder) Decode(_ context.Context, _ string, encoded []byte) (json.RawMessage, error) {
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
	definition, err := registeredDefinition(g.schemas, schemaName)
	if err != nil {
		return "", err
	}
	out, err := g.client.GetSchemaByDefinition(ctx, &glue.GetSchemaByDefinitionInput{
		SchemaId: &gluetypes.SchemaId{
			SchemaName:   &schemaName,
			RegistryName: &g.registryName,
		},
		SchemaDefinition: &definition,
	})
	if err != nil {
		return "", err
	}
	if out.SchemaVersionId == nil {
		return "", fmt.Errorf("nil SchemaVersionId for schema %q in registry %q", schemaName, g.registryName)
	}
	if out.Status != gluetypes.SchemaVersionStatusAvailable {
		return "", fmt.Errorf("schema %q version %s in registry %q is %s, not AVAILABLE", schemaName, *out.SchemaVersionId, g.registryName, out.Status)
	}
	return *out.SchemaVersionId, nil
}

// registeredDefinition returns schemaName's embedded schema file in exactly
// the form schema-gov register uploads it: Python's
// json.dumps(schema, separators=(",", ":")) — compact, key order preserved,
// non-ASCII escaped as \uXXXX (ensure_ascii defaults to True). Sending the
// byte-identical string makes GetSchemaByDefinition match whether or not
// Glue normalises JSON definitions. TestRegisteredDefinition_MatchesSchemaGov
// pins this against real Python for every embedded file.
func registeredDefinition(schemas fs.FS, schemaName string) (string, error) {
	entries, err := fs.ReadDir(schemas, "schemas")
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || eventTypeFromSchemaFile(e.Name()) != schemaName {
			continue
		}
		raw, err := fs.ReadFile(schemas, path.Join("schemas", e.Name()))
		if err != nil {
			return "", err
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			return "", fmt.Errorf("compact schema %s: %w", e.Name(), err)
		}
		return asciiEscape(compact.Bytes()), nil
	}
	return "", fmt.Errorf("no embedded schema file for %q", schemaName)
}

// asciiEscape rewrites every non-ASCII rune as a JSON \uXXXX escape
// (UTF-16 surrogate pairs above U+FFFF), matching Python's ensure_ascii.
// Non-ASCII can only appear inside JSON strings, so this is always safe.
func asciiEscape(b []byte) string {
	var out strings.Builder
	out.Grow(len(b))
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		b = b[size:]
		switch {
		case r < utf8.RuneSelf:
			out.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(&out, "\\u%04x\\u%04x", 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			fmt.Fprintf(&out, "\\u%04x", r)
		}
	}
	return out.String()
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

// AllSchemaNames returns the PascalCase Glue / envelope.type name for every
// produced event schema embedded under schemas/. Consumed (producer-owned)
// extracts stay on disk for schema-gov coverage but are omitted so Glue
// prefetch cannot request schemas this service does not register.
func AllSchemaNames() ([]string, error) {
	names, err := allSchemaNamesFromFS(schemasFS)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if domain.IsProducedEvent(name) {
			out = append(out, name)
		}
	}
	return out, nil
}

// allSchemaNamesFromFS is the testable implementation of AllSchemaNames'
// filesystem walk. It accepts an fs.FS so tests can inject a fstest.MapFS
// to trigger error branches (ReadDir error, non-.json continue). Names
// come from the schemaFileNames map, falling back to the filename stem
// for any file not listed there.
func allSchemaNamesFromFS(schemas fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(schemas, "schemas")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, eventTypeFromSchemaFile(e.Name()))
	}
	return names, nil
}

// schemaFileNames maps every embedded schemas/*.json filename (schema-gov
// extract 0.4's snake_case output) to its PascalCase Glue schema name /
// envelope.type — the same hand-maintained, one-line-per-event convention
// as the sibling services' eventschema.ByEventType maps, rather than
// deriving it at runtime from each file's JSON content. Covers all 28
// embedded files: the 14 this service produces (domain.IsProducedEvent)
// plus the 14 consumed, other-service-owned extracts kept here only for
// schema-gov coverage (LLD §16). A new schema file needs one entry added
// here.
var schemaFileNames = map[string]string{
	"department_catalog_changed.json":          "DepartmentCatalogChanged",
	"department_membership_granted.json":       domain.EventDepartmentMembershipGranted,
	"department_membership_level_changed.json": domain.EventDepartmentMembershipLevelChanged,
	"department_membership_revoked.json":       domain.EventDepartmentMembershipRevoked,
	"direct_paid_signup.json":                  "DirectPaidSignup",
	"membership_revoked.json":                  domain.EventMembershipRevoked,
	"mfa_reset.json":                           domain.EventMFAReset,
	"tenant_converted.json":                    "TenantConverted",
	"tenant_created.json":                      domain.EventTenantCreated,
	"tenant_memberships_purged.json":           domain.EventTenantMembershipsPurged,
	"tenant_offboarded.json":                   "TenantOffboarded",
	"tenant_payment_past_due.json":             "TenantPaymentPastDue",
	"tenant_plan_changed.json":                 "TenantPlanChanged",
	"tenant_reactivated.json":                  "TenantReactivated",
	"tenant_realm_ready.json":                  "TenantRealmReady",
	"tenant_role_granted.json":                 domain.EventTenantRoleGranted,
	"tenant_role_revoked.json":                 domain.EventTenantRoleRevoked,
	"tenant_seat_overage_resolved.json":        domain.EventTenantSeatOverageResolved,
	"tenant_seat_overage_started.json":         domain.EventTenantSeatOverageStarted,
	"tenant_seats_changed.json":                "TenantSeatsChanged",
	"tenant_state_changed.json":                domain.EventTenantStateChanged,
	"tenant_subscription_cancelled.json":       "TenantSubscriptionCancelled",
	"tenant_suspended.json":                    "TenantSuspended",
	"tender_assignee_overridden.json":          domain.EventTenderAssigneeOverridden,
	"trial_expired.json":                       "TrialExpired",
	"trial_reactivated.json":                   "TrialReactivated",
	"trial_started.json":                       domain.EventTrialStarted,
	"trial_tenant_provisioned.json":            "TrialTenantProvisioned",
}

// eventTypeFromSchemaFile returns the PascalCase event / Glue schema name
// for an embedded schema filename via the schemaFileNames map, falling
// back to the filename stem for any file not listed there.
func eventTypeFromSchemaFile(filename string) string {
	if name, ok := schemaFileNames[filename]; ok {
		return name
	}
	return strings.TrimSuffix(filename, ".json")
}
