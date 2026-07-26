package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
)

// stubTx is a zero-value pgx.Tx used only to prove pointer identity through
// context.Value round-trips. None of its methods are ever invoked.
type stubTx struct{ pgx.Tx }

// ── WithTx + TxFromContext round-trip ──────────────────────────────────

func TestWithTx_RoundTripReturnsSameTx(t *testing.T) {
	tx := &stubTx{}
	ctx := WithTx(context.Background(), tx)

	got, ok := TxFromContext(ctx)
	assert.True(t, ok)
	assert.Same(t, pgx.Tx(tx), got, "TxFromContext must return the pointer WithTx stored")
}

func TestTxFromContext_MissingReturnsFalse(t *testing.T) {
	got, ok := TxFromContext(context.Background())
	assert.False(t, ok)
	assert.Nil(t, got)
}

// ── pgadapterTxFromContext (package-private) ───────────────────────────

func TestPgadapterTxFromContext_MissingReturnsFalse(t *testing.T) {
	got, ok := pgadapterTxFromContext(context.Background())
	assert.False(t, ok)
	assert.Nil(t, got)
}

func TestPgadapterTxFromContext_PresentReturnsTx(t *testing.T) {
	tx := &stubTx{}
	ctx := WithTx(context.Background(), tx)

	got, ok := pgadapterTxFromContext(ctx)
	assert.True(t, ok)
	assert.NotNil(t, got)
}

// ── Wrong-type value under an unrelated key is ignored ─────────────────
// Guards against a future refactor that changes the key type breaking
// silently: an unrelated value under a different key must NOT satisfy
// the lookup.
func TestTxFromContext_UnrelatedContextValueIgnored(t *testing.T) {
	type otherKey struct{}
	ctx := context.WithValue(context.Background(), otherKey{}, &stubTx{})

	got, ok := TxFromContext(ctx)
	assert.False(t, ok)
	assert.Nil(t, got)
}
