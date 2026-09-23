//go:build integration

// Glue schema-version resolution against a real (floci) Glue Schema
// Registry. Verifies GlueCodec stamps events with the version matching this
// binary's embedded schema (GetSchemaByDefinition), not the registry's
// latest — the property the retired LatestVersion prefetch + 5-minute
// refresher got wrong during deploy/registration races and rollbacks.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/eventbus"
)

func newFlociGlue(t *testing.T) *glue.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping floci integration test in short mode")
	}
	endpoint := startFloci(t)
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(flociRegion),
		awsconfig.WithCredentialsProvider(awscreds.NewStaticCredentialsProvider("test", "test", "")),
	)
	require.NoError(t, err)
	return glue.NewFromConfig(cfg, func(o *glue.Options) { o.BaseEndpoint = &endpoint })
}

// registeredForm returns an embedded schema file the way schema-gov
// register uploads it (compact; the files are ASCII-only, which the
// eventbus package's parity test enforces).
func registeredForm(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "adapter", "outbound", "eventbus", "schemas", file))
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, json.Compact(&out, raw))
	return out.String()
}

func createRegistry(t *testing.T, cli *glue.Client) string {
	t.Helper()
	name := "it-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	_, err := cli.CreateRegistry(context.Background(), &glue.CreateRegistryInput{RegistryName: &name})
	require.NoError(t, err)
	return name
}

func createSchema(t *testing.T, cli *glue.Client, registry, name, definition string) string {
	t.Helper()
	out, err := cli.CreateSchema(context.Background(), &glue.CreateSchemaInput{
		RegistryId:       &gluetypes.RegistryId{RegistryName: &registry},
		SchemaName:       &name,
		DataFormat:       gluetypes.DataFormatJson,
		Compatibility:    gluetypes.CompatibilityBackward,
		SchemaDefinition: &definition,
	})
	require.NoError(t, err)
	return aws.ToString(out.SchemaVersionId)
}

func encodedVersion(t *testing.T, codec *eventbusadapter.GlueCodec, schema string) string {
	t.Helper()
	_, vid, err := codec.Encode(context.Background(), schema, json.RawMessage(`{}`))
	require.NoError(t, err)
	return vid
}

// P12-GLUE-001: a binary keeps stamping the version it ships even after a
// newer version is registered — the rollback / registered-ahead-of-deploy
// case.
func TestGlueCodec_ResolvesOwnVersion_NotLatest(t *testing.T) {
	cli := newFlociGlue(t)
	registry := createRegistry(t, cli)

	v1 := createSchema(t, cli, registry, "TenantCreated", registeredForm(t, "tenant_created.json"))

	var v1Doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(registeredForm(t, "tenant_created.json")), &v1Doc))
	// A BACKWARD-compatible change (annotation only) — Glue rejects
	// constraining new properties on an OPEN schema as narrowing.
	v1Doc["description"] = "v2"
	v2Def, err := json.Marshal(v1Doc)
	require.NoError(t, err)
	v2Out, err := cli.RegisterSchemaVersion(context.Background(), &glue.RegisterSchemaVersionInput{
		SchemaId:         &gluetypes.SchemaId{RegistryName: &registry, SchemaName: aws.String("TenantCreated")},
		SchemaDefinition: aws.String(string(v2Def)),
	})
	require.NoError(t, err)
	v2 := aws.ToString(v2Out.SchemaVersionId)
	require.NotEqual(t, v1, v2)

	latest, err := cli.GetSchemaVersion(context.Background(), &glue.GetSchemaVersionInput{
		SchemaId:            &gluetypes.SchemaId{RegistryName: &registry, SchemaName: aws.String("TenantCreated")},
		SchemaVersionNumber: &gluetypes.SchemaVersionNumber{LatestVersion: true},
	})
	require.NoError(t, err)
	require.Equal(t, v2, aws.ToString(latest.SchemaVersionId), "precondition: v2 is latest")

	codec, err := eventbusadapter.NewGlueCodec(context.Background(), cli, registry, []string{"TenantCreated"})
	require.NoError(t, err)
	assert.Equal(t, v1, encodedVersion(t, codec, "TenantCreated"),
		"must stamp the version matching the embedded (v1) schema, not latest (v2)")
}

// P12-GLUE-002: every produced schema, registered exactly as CI registers
// it, resolves to its own version in both registries.
func TestGlueCodec_ResolvesAllProducedSchemas(t *testing.T) {
	cli := newFlociGlue(t)
	registry := createRegistry(t, cli)

	names, err := eventbusadapter.AllSchemaNames()
	require.NoError(t, err)
	want := map[string]string{}
	for _, name := range names {
		want[name] = createSchema(t, cli, registry, name, registeredForm(t, schemaFileFor(t, name)))
	}

	codec, err := eventbusadapter.NewGlueCodec(context.Background(), cli, registry, names)
	require.NoError(t, err)
	for name, vid := range want {
		assert.Equal(t, vid, encodedVersion(t, codec, name), name)
	}
}

// P12-GLUE-003: a deploy that outruns schema registration fails startup
// instead of stamping a wrong or missing version.
func TestGlueCodec_UnregisteredDefinition_FailsStartup(t *testing.T) {
	cli := newFlociGlue(t)
	registry := createRegistry(t, cli)

	// Registered, but with a different (older) definition than the binary's.
	createSchema(t, cli, registry, "TrialStarted", `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","additionalProperties":true}`)

	_, err := eventbusadapter.NewGlueCodec(context.Background(), cli, registry, []string{"TrialStarted"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "isn't registered yet")

	_, err = eventbusadapter.NewGlueCodec(context.Background(), cli, registry, []string{"TenantCreated"})
	require.Error(t, err, "schema absent from the registry entirely")
}

// schemaFileFor maps a PascalCase schema name back to its snake_case file.
func schemaFileFor(t *testing.T, name string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "..", "internal", "adapter", "outbound", "eventbus", "schemas"))
	require.NoError(t, err)
	for _, e := range entries {
		stem := strings.TrimSuffix(e.Name(), ".json")
		if strings.ReplaceAll(stem, "_", "") == strings.ToLower(name) {
			return e.Name()
		}
	}
	require.Fail(t, fmt.Sprintf("no schema file for %s", name))
	return ""
}
