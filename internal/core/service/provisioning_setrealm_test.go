package service

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildProvisioningWithTxRunner wires just the txRunner field, which is
// all SetRealmFields uses now that it's routed through TxRunner.
func buildProvisioningWithTxRunner(tr *ffPassthroughTxRunner) *ProvisioningService {
	return &ProvisioningService{txRunner: tr}
}

// ── SetRealmFields — happy path ─────────────────────────────────────────

func TestProvisioning_SetRealmFields_UpdatesRealmColumns(t *testing.T) {
	tenantID := uuid.New()
	realmID, shard := "acme-realm", "shard-1"
	var gotSQL string
	var gotArgs []any
	tx := &ffTx{
		execFn: func(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
			gotSQL = sql
			gotArgs = args
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	svc := buildProvisioningWithTxRunner(&ffPassthroughTxRunner{tx: tx})

	err := svc.SetRealmFields(context.Background(), tenantID, realmID, domain.RealmType("dedicated"), shard)
	require.NoError(t, err)
	assert.Contains(t, gotSQL, "UPDATE tenants SET realm_id")
	require.Len(t, gotArgs, 4)
	assert.Equal(t, tenantID, gotArgs[0])
	assert.Equal(t, realmID, gotArgs[1])
	assert.Equal(t, "dedicated", gotArgs[2])
	assert.Equal(t, shard, gotArgs[3])
}

// ── SetRealmFields — tx-unavailable → conflict ─────────────────────────

func TestProvisioning_SetRealmFields_TxUnavailableSurfaces(t *testing.T) {
	svc := &ProvisioningService{txRunner: noInjectTxRunner{}}
	err := svc.SetRealmFields(context.Background(), uuid.New(), "r", domain.RealmType("shared"), "s")
	assert.ErrorIs(t, err, domain.ErrConflict)
}

// ── SetRealmFields — sql exec failure propagates ───────────────────────

func TestProvisioning_SetRealmFields_ExecErrorPropagates(t *testing.T) {
	execErr := errors.New("update failed")
	tx := &ffTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	svc := buildProvisioningWithTxRunner(&ffPassthroughTxRunner{tx: tx})
	err := svc.SetRealmFields(context.Background(), uuid.New(), "r", domain.RealmType("shared"), "s")
	assert.ErrorIs(t, err, execErr)
}
