// Package eventbus — unit tests for GlueCodec (glue_codec.go).
//
// The GlueCodec depends on *glue.Client which calls AWS Glue's HTTP API.
// We mock the HTTP layer using httptest.NewServer + a custom endpoint set
// via glue.Options.BaseEndpoint, eliminating any AWS credentials or real
// Glue registry dependency.
//
// Covered functions:
//   - NewGlueCodec — resolves by definition (GetSchemaByDefinition), error
//     propagation, non-AVAILABLE status, missing embedded schema file
//   - Encode — cache hit, cache miss + re-fetch, unknown schema error
//   - Decode — correct strip, too-short error, wrong magic byte error
//   - versionID — cache hit / miss paths
//   - prependGlueHeader — called via Encode; tested through it
//   - AllSchemaNames — reads embedded FS
//   - registeredDefinition — byte-identical to schema-gov register's upload
package eventbus

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── helpers ───────────────────────────────────────────────────────────────

// mockGlueServer builds an httptest.Server that answers GetSchemaByDefinition
// with the given schema version ID (status AVAILABLE).
func mockGlueServer(t *testing.T, versionID string) *httptest.Server {
	t.Helper()
	srv, _ := mockGlueByDefinition(t, versionID, "AVAILABLE")
	return srv
}

// mockGlueByDefinition answers GetSchemaByDefinition with versionID/status
// and records every SchemaDefinition it receives. Any other Glue action
// (e.g. the retired GetSchemaVersion LatestVersion lookup) fails the test.
func mockGlueByDefinition(t *testing.T, versionID, status string) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	defs := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if target := r.Header.Get("X-Amz-Target"); target != "AWSGlue.GetSchemaByDefinition" {
			t.Errorf("unexpected Glue action %q — versions must be resolved by definition", target)
		}
		var in struct{ SchemaDefinition string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		defs = append(defs, in.SchemaDefinition)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(map[string]any{"SchemaVersionId": versionID, "Status": status})
	}))
	return srv, &defs
}

// testSchemas is a schema FS covering the arbitrary names these tests use;
// eventTypeFromSchemaFile falls back to the filename stem for them.
var testSchemas = fstest.MapFS{}

func init() {
	for _, n := range []string{"S", "Evt", "SomeSchema", "X", "BadSchema", "ValidSchema", "DynamicSchema", "MissingSchema"} {
		testSchemas["schemas/"+n+".json"] = &fstest.MapFile{Data: []byte("{\n  \"type\": \"object\"\n}\n")}
	}
}

// mockGlueServerError builds an httptest.Server that always returns 400.
func mockGlueServerError(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"__type":  "EntityNotFoundException",
			"Message": "Schema not found",
		})
	}))
}

// buildGlueClient creates a *glue.Client pointed at srv's URL with dummy
// static credentials (required by the SDK — never sent to a real endpoint).
func buildGlueClient(t *testing.T, srv *httptest.Server) *glue.Client {
	t.Helper()
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test", SessionToken: "test"}, nil
		})),
	)
	require.NoError(t, err)
	ep := srv.URL
	return glue.NewFromConfig(cfg, func(o *glue.Options) {
		o.BaseEndpoint = &ep
	})
}

// deterministicVersionID returns a valid UUID string for use as a schema version ID.
var deterministicVersionID = "12345678-1234-1234-1234-1234567890ab"

// ── NewGlueCodec — happy path ─────────────────────────────────────────────

// TestNewGlueCodec_PreFetchesVersionID verifies that NewGlueCodec succeeds
// when the Glue API is reachable, and the version ID is stored in the cache.
func TestNewGlueCodec_PreFetchesVersionID(t *testing.T) {
	srv := mockGlueServer(t, deterministicVersionID)
	defer srv.Close()

	client := buildGlueClient(t, srv)
	codec, err := newGlueCodecFromFS(context.Background(), client, "reg", []string{"SomeSchema"}, testSchemas)

	require.NoError(t, err)
	require.NotNil(t, codec)
}

// TestNewGlueCodec_FetchError_Propagates verifies that when the Glue API
// returns an error during pre-fetch, NewGlueCodec returns an error wrapping
// the original.
func TestNewGlueCodec_FetchError_Propagates(t *testing.T) {
	srv := mockGlueServerError(t)
	defer srv.Close()

	client := buildGlueClient(t, srv)
	_, err := newGlueCodecFromFS(context.Background(), client, "reg", []string{"BadSchema"}, testSchemas)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "BadSchema",
		"error must mention the failing schema name")
}

// ── Decode — header stripping ─────────────────────────────────────────────

// TestGlueCodec_Decode_HappyPath verifies that a correctly encoded payload
// has its 18-byte header stripped and the JSON body returned verbatim.
func TestGlueCodec_Decode_HappyPath(t *testing.T) {
	srv := mockGlueServer(t, deterministicVersionID)
	defer srv.Close()

	client := buildGlueClient(t, srv)
	codec, err := newGlueCodecFromFS(context.Background(), client, "reg", []string{"S"}, testSchemas)
	require.NoError(t, err)

	payload := json.RawMessage(`{"event":"test"}`)
	encoded, versionID, err := codec.Encode(context.Background(), "S", payload)
	require.NoError(t, err)
	require.NotEmpty(t, versionID)

	decoded, err := codec.Decode(context.Background(), versionID, encoded)
	require.NoError(t, err)
	assert.Equal(t, string(payload), string(decoded))
}

// TestGlueCodec_Decode_TooShort_ReturnsError verifies that a payload shorter
// than the 18-byte Glue header is rejected.
func TestGlueCodec_Decode_TooShort_ReturnsError(t *testing.T) {
	srv := mockGlueServer(t, deterministicVersionID)
	defer srv.Close()

	codec, err := newGlueCodecFromFS(context.Background(), buildGlueClient(t, srv), "r", []string{"X"}, testSchemas)
	require.NoError(t, err)

	_, err = codec.Decode(context.Background(), "any", []byte("short"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shorter than the")
}

// TestGlueCodec_Decode_WrongMagicByte_ReturnsError verifies that a payload
// whose first byte is not the expected Glue magic byte (0x03) is rejected.
func TestGlueCodec_Decode_WrongMagicByte_ReturnsError(t *testing.T) {
	srv := mockGlueServer(t, deterministicVersionID)
	defer srv.Close()

	codec, err := newGlueCodecFromFS(context.Background(), buildGlueClient(t, srv), "r", []string{"X"}, testSchemas)
	require.NoError(t, err)

	bad := make([]byte, glueHeaderSize+1)
	bad[0] = 0xFF // wrong magic
	_, err = codec.Decode(context.Background(), "any", bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected header version byte")
}

// ── Encode — cache hit / miss / unknown schema ────────────────────────────

// TestGlueCodec_Encode_CacheMiss_FetchesAndCaches verifies that when a
// schema name is NOT in the pre-resolved cache, Encode resolves it by
// definition and caches the result for subsequent calls.
func TestGlueCodec_Encode_CacheMiss_FetchesAndCaches(t *testing.T) {
	srv := mockGlueServer(t, deterministicVersionID)
	defer srv.Close()

	client := buildGlueClient(t, srv)
	// Build with no pre-fetched schemas — the first Encode call must fetch.
	codec, err := newGlueCodecFromFS(context.Background(), client, "reg", []string{}, testSchemas)
	require.NoError(t, err)

	payload := json.RawMessage(`{"x":1}`)
	encoded, versionID, err := codec.Encode(context.Background(), "DynamicSchema", payload)
	require.NoError(t, err)
	assert.NotEmpty(t, versionID)
	assert.Len(t, encoded, glueHeaderSize+len(payload))
}

// TestGlueCodec_Encode_UnknownSchema_PropagatesError verifies that when Glue
// returns an error for an unknown schema, Encode propagates the error.
func TestGlueCodec_Encode_UnknownSchema_PropagatesError(t *testing.T) {
	srv := mockGlueServerError(t)
	defer srv.Close()

	codec, err := newGlueCodecFromFS(context.Background(), buildGlueClient(t, srv), "r", []string{}, testSchemas)
	require.NoError(t, err)

	_, _, err = codec.Encode(context.Background(), "MissingSchema", json.RawMessage(`{}`))
	require.Error(t, err)
}

// ── prependGlueHeader — invalid UUID ─────────────────────────────────────

// TestPrependGlueHeader_InvalidUUID_ReturnsError verifies that
// prependGlueHeader returns an error when the schema version ID is not a
// valid UUID string, exercising the uuid.Parse error branch.
func TestPrependGlueHeader_InvalidUUID_ReturnsError(t *testing.T) {
	_, err := prependGlueHeader("not-a-uuid", []byte(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse schema version UUID")
}

// TestPrependGlueHeader_ValidUUID_ProducesCorrectBytes verifies the happy path:
// the output is exactly glueHeaderSize+len(payload) bytes with the right
// magic header byte.
func TestPrependGlueHeader_ValidUUID_ProducesCorrectBytes(t *testing.T) {
	id := uuid.New().String()
	payload := []byte(`{"hello":"world"}`)

	out, err := prependGlueHeader(id, payload)
	require.NoError(t, err)
	assert.Len(t, out, glueHeaderSize+len(payload))
	assert.Equal(t, glueHeaderVersion, out[0], "first byte must be the magic version byte 0x03")
	assert.Equal(t, glueNoCompression, out[1], "second byte must be 0x00 (no compression)")
}

// ── AllSchemaNames ────────────────────────────────────────────────────────

// TestAllSchemaNames_ReturnsNonEmptyList verifies that AllSchemaNames reads
// the embedded schemas FS and returns at least one name (the service must
// have at least one registered event type).
func TestAllSchemaNames_ReturnsNonEmptyList(t *testing.T) {
	names, err := AllSchemaNames()
	require.NoError(t, err)
	assert.NotEmpty(t, names, "AllSchemaNames must return at least one schema")
	// Spot-check that returned names are bare schema names (no .json extension).
	for _, n := range names {
		assert.False(t, strings.HasSuffix(n, ".json"),
			"schema name %q must not carry the .json extension", n)
		assert.NotEmpty(t, n, "schema name must not be empty")
	}
}

// ── fetchVersionID — nil SchemaVersionId branch ───────────────────────────

// TestGlueCodec_FetchVersionID_NilSchemaVersionId_ReturnsError verifies that
// when the Glue API returns a 200 response with a nil SchemaVersionId field,
// fetchVersionID returns a descriptive error (the nil-check branch at line 154).
func TestGlueCodec_FetchVersionID_NilSchemaVersionId_ReturnsError(t *testing.T) {
	// Build a server that returns a response WITHOUT SchemaVersionId.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deliberately omit SchemaVersionId to trigger the nil-check branch.
		resp := map[string]any{
			"Status": "AVAILABLE",
			// SchemaVersionId intentionally absent → SDK will leave it nil
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := buildGlueClient(t, srv)
	// NewGlueCodec pre-fetches — this will trigger the nil SchemaVersionId branch.
	_, err := newGlueCodecFromFS(context.Background(), client, "reg", []string{"SomeSchema"}, testSchemas)
	require.Error(t, err, "nil SchemaVersionId must cause an error")
	assert.Contains(t, err.Error(), "nil SchemaVersionId",
		"error message must identify the nil SchemaVersionId condition")
}

// ── Encode — prependGlueHeader error via invalid UUID in cache ────────────

// TestGlueCodec_Encode_InvalidCachedUUID_ReturnsError verifies that if the
// version cache somehow holds a non-UUID value (defensive), Encode propagates
// the prependGlueHeader parse error. We exercise this by directly mutating the
// codec's internal cache (whitebox, same package).
func TestGlueCodec_Encode_InvalidCachedUUID_ReturnsError(t *testing.T) {
	srv := mockGlueServer(t, deterministicVersionID)
	defer srv.Close()

	client := buildGlueClient(t, srv)
	codec, err := newGlueCodecFromFS(context.Background(), client, "reg", []string{"ValidSchema"}, testSchemas)
	require.NoError(t, err)

	// Directly corrupt the cache entry so prependGlueHeader gets a bad UUID.
	codec.mu.Lock()
	codec.versionCache["ValidSchema"] = "not-a-valid-uuid"
	codec.mu.Unlock()

	_, _, encErr := codec.Encode(context.Background(), "ValidSchema", json.RawMessage(`{}`))
	require.Error(t, encErr, "invalid cached UUID must cause Encode to return an error")
	assert.Contains(t, encErr.Error(), "parse schema version UUID",
		"error message must mention the UUID parse failure")
}

// ── AllSchemaNames — happy path with directory entry skipping ─────────────

// TestAllSchemaNames_SkipsNonJSONAndDirs verifies that AllSchemaNames filters
// out non-JSON entries. Since the embedded FS only contains .json files,
// this test verifies the positive case — all returned names lack the .json
// suffix and are non-empty.
func TestAllSchemaNames_AllEntriesAreValidSchemaNames(t *testing.T) {
	names, err := AllSchemaNames()
	require.NoError(t, err)
	require.NotEmpty(t, names)

	// Verify filtering logic: no name contains ".json"
	for _, n := range names {
		assert.NotContains(t, n, ".json",
			"AllSchemaNames must strip the .json extension")
		assert.NotEmpty(t, n)
		assert.True(t, domain.IsProducedEvent(n),
			"AllSchemaNames must return PascalCase produced Glue names, got %q", n)
	}
	assert.Contains(t, names, "TenantCreated")
	assert.Contains(t, names, "MFAReset")
	assert.NotContains(t, names, "tenant_created")
	assert.NotContains(t, names, "mfa_reset")
	assert.NotContains(t, names, "TenantOffboarded")
}

// ── Encode — ValidUUID round-trip assertion ───────────────────────────────

// TestGlueCodec_Encode_Decode_RoundTrip verifies the full encode→decode
// round-trip: payload bytes are preserved exactly.
func TestGlueCodec_Encode_Decode_RoundTrip(t *testing.T) {
	srv := mockGlueServer(t, uuid.New().String())
	defer srv.Close()

	client := buildGlueClient(t, srv)
	codec, err := newGlueCodecFromFS(context.Background(), client, "r", []string{"Evt"}, testSchemas)
	require.NoError(t, err)

	original := json.RawMessage(`{"type":"Evt","data":{"id":1}}`)
	encoded, vid, err := codec.Encode(context.Background(), "Evt", original)
	require.NoError(t, err)

	decoded, err := codec.Decode(context.Background(), vid, encoded)
	require.NoError(t, err)
	assert.Equal(t, string(original), string(decoded))
}

// ── GlueDecoder — consumer-side decode-only codec ─────────────────────────

func TestGlueDecoder_Decode_StripsHeader(t *testing.T) {
	payload := json.RawMessage(`{"event":"upstream"}`)
	encoded, err := prependGlueHeader(deterministicVersionID, payload)
	require.NoError(t, err)

	decoded, err := GlueDecoder{}.Decode(context.Background(), deterministicVersionID, encoded)
	require.NoError(t, err)
	assert.Equal(t, string(payload), string(decoded))
}

func TestGlueDecoder_Decode_InvalidFrame_ReturnsError(t *testing.T) {
	_, err := GlueDecoder{}.Decode(context.Background(), "any", []byte("short"))
	assert.Error(t, err)
}

func TestGlueDecoder_Encode_AlwaysFails(t *testing.T) {
	_, _, err := GlueDecoder{}.Encode(context.Background(), "TenantCreated", json.RawMessage(`{}`))
	assert.ErrorContains(t, err, "decode-only")
}

// ── GetSchemaByDefinition resolution ──────────────────────────────────────

// TestNewGlueCodec_SendsRegisteredDefinition verifies that the real embedded
// schema is sent compacted — the exact string schema-gov register uploads —
// not the pretty-printed file.
func TestNewGlueCodec_SendsRegisteredDefinition(t *testing.T) {
	srv, defs := mockGlueByDefinition(t, deterministicVersionID, "AVAILABLE")
	defer srv.Close()

	codec, err := NewGlueCodec(context.Background(), buildGlueClient(t, srv), "iam-tenant-events", []string{"TenantCreated"})
	require.NoError(t, err)

	want, err := registeredDefinition(schemasFS, "TenantCreated")
	require.NoError(t, err)
	require.Equal(t, []string{want}, *defs)
	assert.NotContains(t, want, "\n", "definition must be compact")

	_, vid, err := codec.Encode(context.Background(), "TenantCreated", json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.Equal(t, deterministicVersionID, vid)
	assert.Len(t, *defs, 1, "resolved once at startup — Encode must not call Glue")
}

// TestNewGlueCodec_NonAvailableVersion_FailsStartup: a version still PENDING
// Glue's compatibility check (or FAILURE) must not be stamped on events.
func TestNewGlueCodec_NonAvailableVersion_FailsStartup(t *testing.T) {
	for _, status := range []string{"PENDING", "FAILURE", "DELETING"} {
		t.Run(status, func(t *testing.T) {
			srv, _ := mockGlueByDefinition(t, deterministicVersionID, status)
			defer srv.Close()
			_, err := newGlueCodecFromFS(context.Background(), buildGlueClient(t, srv), "reg", []string{"S"}, testSchemas)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not AVAILABLE")
		})
	}
}

// TestNewGlueCodec_NoEmbeddedSchemaFile_FailsWithoutCallingGlue: a name with
// no schema file can't be looked up by definition.
func TestNewGlueCodec_NoEmbeddedSchemaFile_FailsWithoutCallingGlue(t *testing.T) {
	srv, defs := mockGlueByDefinition(t, deterministicVersionID, "AVAILABLE")
	defer srv.Close()
	_, err := newGlueCodecFromFS(context.Background(), buildGlueClient(t, srv), "reg", []string{"Unknown"}, testSchemas)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no embedded schema file")
	assert.Empty(t, *defs)
}

func TestRegisteredDefinition_BadFS(t *testing.T) {
	_, err := registeredDefinition(fstest.MapFS{}, "S")
	assert.Error(t, err, "missing schemas/ dir")
	_, err = registeredDefinition(fstest.MapFS{"schemas/S.json": {Data: []byte("{not json")}}, "S")
	assert.Error(t, err, "invalid JSON")
}

func TestASCIIEscape_MatchesPythonEnsureASCII(t *testing.T) {
	assert.Equal(t, `{"d":"caf\u00e9 \u2014 \ud83d\ude00"}`, asciiEscape([]byte(`{"d":"café — 😀"}`)))
	assert.Equal(t, `{"a":1}`, asciiEscape([]byte(`{"a":1}`)))
}

// TestRegisteredDefinition_MatchesSchemaGov pins registeredDefinition to the
// exact bytes schema-gov register uploads —
// json.dumps(json.loads(file), separators=(",", ":")) — for every embedded
// schema file. If this drifts, GetSchemaByDefinition may stop matching in
// real AWS Glue and every pod fails startup.
func TestRegisteredDefinition_MatchesSchemaGov(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	entries, err := schemasFS.ReadDir("schemas")
	require.NoError(t, err)
	for _, e := range entries {
		t.Run(e.Name(), func(t *testing.T) {
			raw, err := schemasFS.ReadFile("schemas/" + e.Name())
			require.NoError(t, err)
			cmd := exec.CommandContext(t.Context(), python, "-c", `import json,sys; sys.stdout.write(json.dumps(json.loads(sys.stdin.read()), separators=(",", ":")))`)
			cmd.Stdin = strings.NewReader(string(raw))
			want, err := cmd.Output()
			require.NoError(t, err)

			got, err := registeredDefinition(schemasFS, eventTypeFromSchemaFile(e.Name()))
			require.NoError(t, err)
			assert.Equal(t, string(want), got)
		})
	}
}
