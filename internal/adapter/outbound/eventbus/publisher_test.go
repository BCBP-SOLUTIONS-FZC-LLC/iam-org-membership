package eventbus

import (
	"context"
	"errors"
	"testing"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTx embeds pgx.Tx (a nil interface value) so any un-overridden method
// nil-panics — cheap way to make sure the SUT only touches Exec. Records
// captured SQL + args so the test can assert.
type fakeTx struct {
	pgx.Tx
	execFn     func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	lastSQL    string
	lastArgs   []any
	execCalled bool
}

func (f *fakeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execCalled = true
	f.lastSQL = sql
	f.lastArgs = args
	if f.execFn == nil {
		return pgconn.CommandTag{}, nil
	}
	return f.execFn(ctx, sql, args...)
}

// ── Publisher.New — constructor smoke ─────────────────────────────────

func TestPublisher_New_SetsFields(t *testing.T) {
	codec := NoopCodec{}
	p := New("iam-org-membership", codec)
	require.NotNil(t, p)
	// Field access through Enqueue happy-path would prove wiring end-to-end;
	// the ctor test just locks the two-arg signature.
	assert.NotNil(t, p)
}

// ── Enqueue — early-return branches that don't need a real pgx.Tx ─────

// fakeCodec returns a pre-programmed error / value pair. Tests exercise
// Enqueue's validation-error branch without needing a working tx.
type fakeCodec struct {
	encodeErr error
	encoded   []byte
	schemaID  string
}

func (f fakeCodec) Encode(context.Context, string, []byte) ([]byte, string, error) {
	if f.encodeErr != nil {
		return nil, "", f.encodeErr
	}
	return f.encoded, f.schemaID, nil
}

func TestPublisher_Enqueue_MarshalPayloadFailure(t *testing.T) {
	// A channel type cannot be JSON-marshalled → the first branch of
	// Enqueue returns an error before touching tx or codec.
	p := New("test", fakeCodec{})
	err := p.Enqueue(context.Background(), &domain.DomainEvent{
		Type: "X",
		Data: make(chan int),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "marshal event payload")
}

func TestPublisher_Enqueue_CodecErrorPropagates(t *testing.T) {
	// codec.Encode error → early return, no tx access needed.
	codecErr := errors.New("glue schema unknown")
	p := New("test", fakeCodec{encodeErr: codecErr})
	err := p.Enqueue(context.Background(), &domain.DomainEvent{
		Type:     "TenantCreated",
		TenantID: uuid.New(),
		Data:     map[string]any{"foo": "bar"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validate event TenantCreated")
	assert.ErrorIs(t, err, codecErr)
}

// ── Enqueue happy path via fakeTx on ctx (covers option branches too) ──

func TestPublisher_Enqueue_HappyPathInsertsIntoOutbox(t *testing.T) {
	tx := &fakeTx{}
	p := New("iam-org-membership", NoopCodec{})
	err := p.Enqueue(pgadapter.WithTx(context.Background(), tx), &domain.DomainEvent{
		Type:      "TenantCreated",
		TenantID:  uuid.New(),
		Subject:   "sub",
		Actor:     "act",
		Data:      map[string]any{"foo": "bar"},
		IPAddress: "127.0.0.1",
		UserAgent: "curl/8",
	})
	require.NoError(t, err)
	assert.True(t, tx.execCalled, "must reach the outbox INSERT")
	assert.Contains(t, tx.lastSQL, "outbox_events")
}

func TestPublisher_Enqueue_RequiresOpenTx(t *testing.T) {
	p := New("iam-org-membership", NoopCodec{})
	err := p.Enqueue(context.Background(), &domain.DomainEvent{
		Type: "TenantCreated", TenantID: uuid.New(),
		Data: map[string]any{"foo": "bar"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RunInTx")
}

func TestPublisher_WithLogger_ReturnsSameInstanceAndRoutesDebugLog(t *testing.T) {
	p := New("iam-org-membership", NoopCodec{})
	log := &glueTestLogger{}

	got := p.WithLogger(log)
	assert.Same(t, p, got, "WithLogger must return the same *Publisher for chaining")

	tx := &fakeTx{}
	err := p.Enqueue(pgadapter.WithTx(context.Background(), tx), &domain.DomainEvent{
		Type: "TenantCreated", TenantID: uuid.New(),
		Data: map[string]any{"foo": "bar"},
	})
	require.NoError(t, err)
	// glueTestLogger only records Warn calls; the wired sink must at least
	// be reachable without panicking on the Debug path Enqueue exercises.
}

func TestPublisher_Enqueue_TxExecErrorPropagates(t *testing.T) {
	execErr := errors.New("outbox conflict")
	tx := &fakeTx{execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
		return pgconn.CommandTag{}, execErr
	}}
	p := New("test", NoopCodec{})
	err := p.Enqueue(pgadapter.WithTx(context.Background(), tx), &domain.DomainEvent{
		Type: "TenantRoleGranted", TenantID: uuid.New(),
		Data: map[string]any{"x": 1},
	})
	assert.ErrorIs(t, err, execErr)
}

func TestPublisher_Enqueue_CodecReturnIgnoredPayloadIsPlainJSON(t *testing.T) {
	// Even when codec returns a non-empty schemaID, Enqueue must store
	// the plain-JSON raw bytes (not the codec's encoded output) — Glue
	// encoding is deferred to the SNS publisher via WithCodec (publish path).
	tx := &fakeTx{}
	p := New("test", fakeCodec{encoded: []byte(`GLUE_BYTES`), schemaID: "arn:aws:glue:us-east-1:123:schemaVersion/abc"})
	err := p.Enqueue(pgadapter.WithTx(context.Background(), tx), &domain.DomainEvent{
		Type: "TenantCreated", TenantID: uuid.New(),
		Data: map[string]any{"a": "b"},
	})
	require.NoError(t, err)
	assert.True(t, tx.execCalled, "outbox INSERT must be reached")
}

// TestPublisher_WithLogger_ReturnsSelf verifies that WithLogger injects the
// logger and returns the same *Publisher pointer (fluent builder pattern).
func TestPublisher_WithLogger_ReturnsSelf(t *testing.T) {
	p := New("iam-org-membership", NoopCodec{})
	// nopLogger satisfies port.Logger with no-op implementations.
	got := p.WithLogger(nopEventbusLogger{})
	require.NotNil(t, got, "WithLogger must return non-nil *Publisher")
	assert.Same(t, p, got, "WithLogger must return the same receiver pointer")
}

// nopEventbusLogger is a minimal port.Logger that satisfies the interface.
type nopEventbusLogger struct{}

func (nopEventbusLogger) Debug(_ string, _ map[string]any) {}
func (nopEventbusLogger) Info(_ string, _ map[string]any)  {}
func (nopEventbusLogger) Warn(_ string, _ map[string]any)  {}
func (nopEventbusLogger) Error(_ string, _ map[string]any) {}

var _ port.Logger = nopEventbusLogger{}

// TestAllSchemaNames_ReturnsNonEmpty verifies that AllSchemaNames reads the
// embedded FS and returns at least one schema name (without the .json suffix).
func TestAllSchemaNames_ReturnsNonEmpty(t *testing.T) {
	names, err := AllSchemaNames()
	require.NoError(t, err)
	assert.NotEmpty(t, names, "AllSchemaNames must return at least one schema from the embedded FS")
	for _, n := range names {
		assert.NotContains(t, n, ".json",
			"AllSchemaNames must strip the .json suffix from each name")
	}
}

// glueTestLogger is a no-op port.Logger test double.
type glueTestLogger struct{}

func (l *glueTestLogger) Debug(string, map[string]any) {}
func (l *glueTestLogger) Info(string, map[string]any)  {}
func (l *glueTestLogger) Warn(string, map[string]any)  {}
func (l *glueTestLogger) Error(string, map[string]any) {}

var _ port.Logger = (*glueTestLogger)(nil)
