package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrependGlueHeader_ExactByteLayout(t *testing.T) {
	schemaVersionID := "b6f8f6d0-4b1a-4b1a-8b1a-1234567890ab"
	payload := []byte(`{"tenant_id":"x"}`)

	out, err := prependGlueHeader(schemaVersionID, payload)
	require.NoError(t, err)

	require.Len(t, out, glueHeaderSize+len(payload))
	assert.Equal(t, byte(0x03), out[0], "byte 0 must be the Glue wire-format version magic byte")
	assert.Equal(t, byte(0x00), out[1], "byte 1 must be the no-compression marker")

	wantUUID := uuid.MustParse(schemaVersionID)
	assert.Equal(t, wantUUID[:], out[2:18], "bytes 2..17 must be the raw 16-byte schema-version UUID")
	assert.Equal(t, payload, out[18:], "bytes 18.. must be the JSON payload, byte-for-byte unchanged")
}

func TestStripGlueHeader_RoundTripsWithPrepend(t *testing.T) {
	schemaVersionID := uuid.New().String()
	payload := []byte(`{"a":1,"b":"two"}`)

	encoded, err := prependGlueHeader(schemaVersionID, payload)
	require.NoError(t, err)

	decoded, err := stripGlueHeader(encoded)
	require.NoError(t, err)
	assert.JSONEq(t, string(payload), string(decoded))
}

func TestStripGlueHeader_TooShortErrors(t *testing.T) {
	_, err := stripGlueHeader([]byte{0x03, 0x00, 0x01})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shorter than")
}

func TestStripGlueHeader_WrongVersionByteErrors(t *testing.T) {
	encoded, err := prependGlueHeader(uuid.New().String(), []byte(`{}`))
	require.NoError(t, err)
	encoded[0] = 0x99

	_, err = stripGlueHeader(encoded)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected header version byte")
}

func TestPrependGlueHeader_InvalidUUIDErrors(t *testing.T) {
	_, err := prependGlueHeader("not-a-uuid", []byte(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse schema version UUID")
}

// TestAllSchemaNames_ReadsEmbeddedDirectory covers AllSchemaNames' happy
// path against the real embedded schemas/*.json fixtures this package
// ships (the read-error branch is unreachable without editing production
// code — schemasFS is a compiled-in go:embed, not injectable).
func TestAllSchemaNames_ReadsEmbeddedDirectory(t *testing.T) {
	names, err := AllSchemaNames()
	require.NoError(t, err)
	assert.NotEmpty(t, names, "the repo ships embedded event schemas")
	for _, n := range names {
		assert.NotContains(t, n, ".json", "names must have the extension trimmed")
	}
	assert.Contains(t, names, "TenantCreated")
}

// ── GlueCodec — AWS Glue Schema Registry client wrapper ────────────────
//
// glue.Client is a concrete AWS SDK v2 struct, not an interface, so these
// tests point it at a local httptest server via glue.Options.BaseEndpoint
// (the exact seam cmd/server/main.go already uses for AWS_ENDPOINT_URL) and
// speak the AWS JSON 1.1 protocol Glue uses directly: POST /, header
// `X-Amz-Target: AWSGlue.GetSchemaVersion`, request body
// `{"SchemaId":{"RegistryName":...,"SchemaName":...},"SchemaVersionNumber":{"LatestVersion":true}}`,
// response body `{"SchemaVersionId":"<uuid>"}` (200) or an AWS JSON error
// shape (4xx) — matching this SDK version's own recorded request/response
// snapshots for GetSchemaVersion.

// glueSchemaIDRequest is the subset of GetSchemaVersionInput's wire shape
// this fake server needs to read to route per-schema-name responses.
type glueSchemaIDRequest struct {
	SchemaID struct {
		SchemaName   string `json:"SchemaName"`
		RegistryName string `json:"RegistryName"`
	} `json:"SchemaId"`
}

// fakeGlueServer is a minimal AWS JSON 1.1 stand-in for the Glue
// GetSchemaVersion operation. respond is called once per request (holding
// the lock released) and returns the HTTP status + JSON body to send back;
// it also tracks how many requests were made per schema name.
type fakeGlueServer struct {
	mu      sync.Mutex
	calls   int
	respond func(schemaName string, callNum int) (status int, body string)
	srv     *httptest.Server
}

func newFakeGlueServer(t *testing.T, respond func(schemaName string, callNum int) (int, string)) *fakeGlueServer {
	t.Helper()
	f := &fakeGlueServer{respond: respond}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGlueServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req glueSchemaIDRequest
	_ = json.Unmarshal(body, &req)

	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()

	status, respBody := f.respond(req.SchemaID.SchemaName, call)
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(respBody))
}

func (f *fakeGlueServer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newTestGlueClient builds a *glue.Client pointed at srv, following the
// exact BaseEndpoint-override seam cmd/server/main.go uses for
// AWS_ENDPOINT_URL/LocalStack.
func newTestGlueClient(srv *httptest.Server) *glue.Client {
	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
	}
	endpoint := srv.URL
	return glue.NewFromConfig(cfg, func(o *glue.Options) {
		o.BaseEndpoint = &endpoint
		o.Retryer = aws.NopRetryer{} // no retries — a single failed call must fail the test fast
	})
}

// glueErrorBody is a minimal AWS JSON 1.1 error response shape.
func glueErrorBody(errType, msg string) string {
	return fmt.Sprintf(`{"__type":%q,"message":%q}`, errType, msg)
}

func TestNewGlueCodec_PrefetchesAndCachesEveryName(t *testing.T) {
	versions := map[string]string{
		"DepartmentMembershipGranted": uuid.New().String(),
		"TenantCreated":               uuid.New().String(),
	}
	srv := newFakeGlueServer(t, func(name string, _ int) (int, string) {
		return http.StatusOK, fmt.Sprintf(`{"SchemaVersionId":%q}`, versions[name])
	})
	client := newTestGlueClient(srv.srv)

	codec, err := NewGlueCodec(context.Background(), client, "iam-membership-events",
		[]string{"DepartmentMembershipGranted", "TenantCreated"})
	require.NoError(t, err)
	require.NotNil(t, codec)
	assert.Equal(t, 2, srv.callCount(), "prefetch must fetch exactly one version per schema name")

	// Encode for an already-cached name must not issue another HTTP call.
	payload := []byte(`{"a":1}`)
	out, versionID, err := codec.Encode(context.Background(), "TenantCreated", payload)
	require.NoError(t, err)
	assert.Equal(t, versions["TenantCreated"], versionID)
	assert.Equal(t, byte(0x03), out[0])
	assert.Equal(t, 2, srv.callCount(), "cache hit must not re-fetch")
}

func TestNewGlueCodec_PrefetchFailureWrapsError(t *testing.T) {
	srv := newFakeGlueServer(t, func(string, int) (int, string) {
		return http.StatusBadRequest, glueErrorBody("com.amazonaws.glue#EntityNotFoundException", "no such schema")
	})
	client := newTestGlueClient(srv.srv)

	codec, err := NewGlueCodec(context.Background(), client, "iam-membership-events", []string{"Missing"})
	require.Error(t, err)
	assert.Nil(t, codec)
	assert.Contains(t, err.Error(), `prefetch glue schema "Missing"`)
	assert.Contains(t, err.Error(), "make schema-verify")
}

func TestGlueCodec_Encode_CacheMissFetchesAndPopulatesCache(t *testing.T) {
	versionID := uuid.New().String()
	srv := newFakeGlueServer(t, func(string, int) (int, string) {
		return http.StatusOK, fmt.Sprintf(`{"SchemaVersionId":%q}`, versionID)
	})
	client := newTestGlueClient(srv.srv)

	// Built directly (bypassing NewGlueCodec) so the cache starts empty —
	// exercises versionID's cache-miss branch via Encode.
	codec := &GlueCodec{
		client:       client,
		registryName: "iam-membership-events",
		versionCache: map[string]string{},
	}
	out, gotVersion, err := codec.Encode(context.Background(), "TenantRoleGranted", []byte(`{"b":2}`))
	require.NoError(t, err)
	assert.Equal(t, versionID, gotVersion)
	assert.Len(t, out, glueHeaderSize+len(`{"b":2}`))
	assert.Equal(t, 1, srv.callCount())

	codec.mu.RLock()
	cached, ok := codec.versionCache["TenantRoleGranted"]
	codec.mu.RUnlock()
	assert.True(t, ok, "successful fetch must populate the cache")
	assert.Equal(t, versionID, cached)

	// A second Encode for the same name must hit the now-warm cache.
	_, _, err = codec.Encode(context.Background(), "TenantRoleGranted", []byte(`{"b":3}`))
	require.NoError(t, err)
	assert.Equal(t, 1, srv.callCount(), "second call must be served from cache")
}

func TestGlueCodec_Encode_FetchErrorPropagates(t *testing.T) {
	srv := newFakeGlueServer(t, func(string, int) (int, string) {
		return http.StatusInternalServerError, glueErrorBody("com.amazonaws.glue#InternalServiceException", "boom")
	})
	client := newTestGlueClient(srv.srv)
	codec := &GlueCodec{client: client, registryName: "reg", versionCache: map[string]string{}}

	_, _, err := codec.Encode(context.Background(), "Unknown", []byte(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `get glue schema version "Unknown"`)
}

func TestGlueCodec_Encode_HeaderPrependErrorPropagates(t *testing.T) {
	// A non-UUID SchemaVersionId reaches Encode's prependGlueHeader call and
	// must surface that parse error directly (Encode does not re-wrap it).
	srv := newFakeGlueServer(t, func(string, int) (int, string) {
		return http.StatusOK, `{"SchemaVersionId":"not-a-uuid"}`
	})
	client := newTestGlueClient(srv.srv)
	codec := &GlueCodec{client: client, registryName: "reg", versionCache: map[string]string{}}

	_, _, err := codec.Encode(context.Background(), "TenantCreated", []byte(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse schema version UUID")
}

func TestGlueCodec_fetchVersionID_NilSchemaVersionIDErrors(t *testing.T) {
	srv := newFakeGlueServer(t, func(string, int) (int, string) {
		return http.StatusOK, `{}` // no SchemaVersionId field
	})
	client := newTestGlueClient(srv.srv)
	codec := &GlueCodec{client: client, registryName: "reg", versionCache: map[string]string{}}

	_, err := codec.fetchVersionID(context.Background(), "TenantCreated")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil SchemaVersionId")
}

func TestGlueCodec_Decode_StripsHeaderRegardlessOfSchemaIDArg(t *testing.T) {
	codec := &GlueCodec{}
	payload := []byte(`{"z":9}`)
	encoded, err := prependGlueHeader(uuid.New().String(), payload)
	require.NoError(t, err)

	decoded, err := codec.Decode(context.Background(), "ignored-schema-id", encoded)
	require.NoError(t, err)
	assert.JSONEq(t, string(payload), string(decoded))
}

// glueTestLogger is a minimal port.Logger fake that records Warn calls so
// StartRefresher's failure-logging branch can be asserted without pulling
// in the real gincommon-backed logger.
type glueTestLogger struct {
	mu    sync.Mutex
	warns []map[string]any
}

func (l *glueTestLogger) Debug(string, map[string]any) {}
func (l *glueTestLogger) Info(string, map[string]any)  {}
func (l *glueTestLogger) Warn(_ string, fields map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, fields)
}
func (l *glueTestLogger) Error(string, map[string]any) {}
func (l *glueTestLogger) warnCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.warns)
}

var _ port.Logger = &glueTestLogger{}

func TestGlueCodec_WithLogger_ReturnsSameInstanceAndWiresSink(t *testing.T) {
	codec := &GlueCodec{versionCache: map[string]string{}}
	log := &glueTestLogger{}

	got := codec.WithLogger(log)
	assert.Same(t, codec, got, "WithLogger must return the same *GlueCodec for chaining")

	// Exercise the wired sink directly through the zero-arg Warn call
	// StartRefresher would make, to confirm the field actually routes.
	codec.log.Warn("test warning", "schema", "X", "error", "boom")
	assert.Equal(t, 1, log.warnCount())
}

func TestGlueCodec_StartRefresher_RefreshesCacheOnSuccess(t *testing.T) {
	oldID := uuid.New().String()
	newID := uuid.New().String()
	srv := newFakeGlueServer(t, func(name string, call int) (int, string) {
		// First call already answered during setup below; every refresher
		// tick after that gets the new ID.
		return http.StatusOK, fmt.Sprintf(`{"SchemaVersionId":%q}`, newID)
	})
	client := newTestGlueClient(srv.srv)
	codec := &GlueCodec{
		client:       client,
		registryName: "reg",
		versionCache: map[string]string{"TenantCreated": oldID},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codec.StartRefresher(ctx, 10*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		codec.mu.RLock()
		cur := codec.versionCache["TenantCreated"]
		codec.mu.RUnlock()
		if cur == newID {
			return // success
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("StartRefresher did not update the cached version ID within the deadline")
}

func TestGlueCodec_StartRefresher_KeepsStaleIDAndWarnsOnFailure(t *testing.T) {
	staleID := uuid.New().String()
	srv := newFakeGlueServer(t, func(string, int) (int, string) {
		return http.StatusInternalServerError, glueErrorBody("com.amazonaws.glue#InternalServiceException", "refresh boom")
	})
	client := newTestGlueClient(srv.srv)
	log := &glueTestLogger{}
	codec := (&GlueCodec{
		client:       client,
		registryName: "reg",
		versionCache: map[string]string{"TenantCreated": staleID},
	}).WithLogger(log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codec.StartRefresher(ctx, 10*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if log.warnCount() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if log.warnCount() == 0 {
		t.Fatal("StartRefresher did not log a warning on refresh failure within the deadline")
	}

	codec.mu.RLock()
	cur := codec.versionCache["TenantCreated"]
	codec.mu.RUnlock()
	assert.Equal(t, staleID, cur, "a failed refresh must leave the stale cached ID in place")
}
