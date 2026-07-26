package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
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
// Enqueue's error branches without needing a working tx (the encode
// check happens before insertEnvelope).
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
func (f fakeCodec) Decode(context.Context, []byte) (string, []byte, error) {
	return "", nil, errors.New("not used")
}

func TestPublisher_Enqueue_MarshalPayloadFailure(t *testing.T) {
	// A channel type cannot be JSON-marshalled → the first branch of
	// Enqueue returns an error before touching tx or codec.
	p := New("test", fakeCodec{})
	err := p.Enqueue(context.Background(), nil, &domain.DomainEvent{
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
	err := p.Enqueue(context.Background(), nil, &domain.DomainEvent{
		Type:     "TenantCreated",
		TenantID: uuid.New(),
		Data:     map[string]any{"foo": "bar"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "encode event TenantCreated")
	assert.ErrorIs(t, err, codecErr)
}

// ── insertEnvelope — oversize guard fires before tx.Exec ───────────────

// ── Enqueue happy path via fakeTx (covers option branches too) ────────

func TestPublisher_Enqueue_HappyPathInsertsIntoOutbox(t *testing.T) {
	tx := &fakeTx{}
	p := New("iam-org-membership", NoopCodec{})
	err := p.Enqueue(context.Background(), tx, &domain.DomainEvent{
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

func TestPublisher_EnqueueInTx_DelegatesToEnqueue(t *testing.T) {
	tx := &fakeTx{}
	p := New("iam-org-membership", NoopCodec{})
	err := p.EnqueueInTx(context.Background(), tx, &domain.DomainEvent{
		Type: "TenantRoleGranted", TenantID: uuid.New(),
		Data: map[string]any{"x": 1},
	})
	require.NoError(t, err)
	assert.True(t, tx.execCalled, "EnqueueInTx must reach the underlying Enqueue path")
}

func TestPublisher_Enqueue_TxExecErrorPropagates(t *testing.T) {
	execErr := errors.New("outbox conflict")
	tx := &fakeTx{execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
		return pgconn.CommandTag{}, execErr
	}}
	p := New("test", NoopCodec{})
	err := p.Enqueue(context.Background(), tx, &domain.DomainEvent{
		Type: "TenantRoleGranted", TenantID: uuid.New(),
		Data: map[string]any{"x": 1},
	})
	assert.ErrorIs(t, err, execErr)
}

func TestPublisher_Enqueue_WithSchemaVersionIDFromCodec(t *testing.T) {
	// A non-empty schema version ID from the codec should be propagated
	// as an Envelope option (WithSchemaID). We can't easily inspect the
	// envelope after construction, but exercising the branch keeps
	// coverage honest.
	tx := &fakeTx{}
	p := New("test", fakeCodec{encoded: []byte(`{"ok":true}`), schemaID: "arn:aws:glue:us-east-1:123:schemaVersion/abc"})
	err := p.Enqueue(context.Background(), tx, &domain.DomainEvent{
		Type: "TenantCreated", TenantID: uuid.New(),
		Data: map[string]any{"a": "b"},
	})
	require.NoError(t, err)
	assert.True(t, tx.execCalled)
}

func TestInsertEnvelope_HappyPathCallsTxExec(t *testing.T) {
	tx := &fakeTx{}
	env := events.NewEnvelope(
		"TenantCreated", "iam-org-membership", json.RawMessage(`{"ok":true}`),
		events.WithTenantID(uuid.New().String()),
	)
	err := insertEnvelope(context.Background(), tx, env)
	require.NoError(t, err)
	assert.True(t, tx.execCalled)
	require.NotEmpty(t, tx.lastArgs)
}

func TestInsertEnvelope_OversizedEnvelopeRejected(t *testing.T) {
	// Craft an envelope whose JSON-serialized form exceeds 240 KB — the
	// size check returns before tx.Exec is called, so nil tx is safe.
	huge := make([]byte, 260*1024)
	for i := range huge {
		huge[i] = 'x'
	}
	env := events.NewEnvelope(
		"TenantCreated",
		"iam-org-membership",
		json.RawMessage(`"`+string(huge)+`"`),
		events.WithTenantID(uuid.New().String()),
	)

	err := insertEnvelope(context.Background(), nil, env)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds SNS 256KB limit")
	assert.True(t, strings.Contains(err.Error(), "envelope is"),
		"error must state the actual byte size for the operator to debug")
}
