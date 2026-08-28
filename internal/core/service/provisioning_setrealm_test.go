package service

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildProvisioningWithTxRunner wires just the txRunner field, which is
// all SetRealmFields uses now that it's routed through TxRunner.
func buildProvisioningWithTxRunner(tr *ffPassthroughTxRunner) *ProvisioningService {
	return &ProvisioningService{txRunner: tr}
}

// ffVersionRow is a pgx.Row that scans a single int64 (record_version).
type ffVersionRow struct{ v int64 }

func (r *ffVersionRow) Scan(dest ...any) error {
	if len(dest) > 0 {
		*dest[0].(*int64) = r.v
	}
	return nil
}

// ffErrScanRow is a pgx.Row that returns a specific non-ErrNoRows error on Scan.
type ffErrScanRow struct{ err error }

func (r *ffErrScanRow) Scan(...any) error { return r.err }

// ── SetRealmFields — happy path ─────────────────────────────────────────

func TestProvisioning_SetRealmFields_UpdatesRealmColumns(t *testing.T) {
	tenantID := uuid.New()
	realmID, shard := "acme-realm", "shard-1"
	var gotSQL string
	var gotArgs []any
	tx := &ffTx{
		queryRowFn: func(_ context.Context, sql string, args ...any) pgx.Row {
			gotSQL = sql
			gotArgs = args
			return &ffVersionRow{v: 2}
		},
	}
	svc := buildProvisioningWithTxRunner(&ffPassthroughTxRunner{tx: tx})

	newVer, err := svc.SetRealmFields(context.Background(), tenantID, realmID, domain.RealmType("dedicated"), shard, 1)
	require.NoError(t, err)
	assert.EqualValues(t, 2, newVer, "new record_version returned from RETURNING clause")
	assert.Contains(t, gotSQL, "UPDATE tenants SET realm_id")
	assert.Contains(t, gotSQL, "RETURNING record_version", "must use RETURNING to capture new version")
	assert.Contains(t, gotSQL, "record_version = $5", "CONC-4: record_version guard added (BUG-I2-2 fix)")
	require.Len(t, gotArgs, 5)
	assert.Equal(t, tenantID, gotArgs[0])
	assert.Equal(t, realmID, gotArgs[1])
	assert.Equal(t, "dedicated", gotArgs[2])
	assert.Equal(t, shard, gotArgs[3])
	assert.Equal(t, int64(1), gotArgs[4], "record_version passed as 5th arg")
}

// ── SetRealmFields — tx-unavailable → conflict ─────────────────────────

func TestProvisioning_SetRealmFields_TxUnavailableSurfaces(t *testing.T) {
	svc := &ProvisioningService{txRunner: noInjectTxRunner{}}
	_, err := svc.SetRealmFields(context.Background(), uuid.New(), "r", domain.RealmType("shared"), "s", 1)
	assert.ErrorIs(t, err, domain.ErrConflict)
}

// ── SetRealmFields — no rows affected → tenant_not_found (G12 fix) ─────

func TestProvisioning_SetRealmFields_NoRowsAffected_TenantNotFound(t *testing.T) {
	// Default ffTx.QueryRow returns ffNoRow (ErrNoRows) for both the UPDATE
	// RETURNING call and the probe SELECT — both returning ErrNoRows causes
	// the probe to fail its Scan, which maps to ErrTenantNotFound.
	tx := &ffTx{}
	svc := buildProvisioningWithTxRunner(&ffPassthroughTxRunner{tx: tx})
	_, err := svc.SetRealmFields(context.Background(), uuid.New(), "acme", domain.RealmType("dedicated"), "shard-1", 1)
	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "tenant_not_found", de.Code)
}

// ── SetRealmFields — sql exec failure propagates ───────────────────────

func TestProvisioning_SetRealmFields_ExecErrorPropagates(t *testing.T) {
	execErr := errors.New("update failed")
	tx := &ffTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			// Non-ErrNoRows error → propagated directly without probing.
			return &ffErrScanRow{err: execErr}
		},
	}
	svc := buildProvisioningWithTxRunner(&ffPassthroughTxRunner{tx: tx})
	_, err := svc.SetRealmFields(context.Background(), uuid.New(), "r", domain.RealmType("shared"), "s", 1)
	assert.ErrorIs(t, err, execErr)
}

// ── I2-TX-02: unique constraint violation (23505) propagates ───────────
//
// Test Case ID:      I2-TX-02
// Scenario:          PATCH /internal/tenants/:id when the realm_id being set
//
//	already exists for another tenant (unique constraint violation on
//	realm_id column) → raw pgconn.PgError{Code:"23505"} propagates as
//	500 internal_error (not mapped to a business error code).
//
// Coverage:          service.ProvisioningService.SetRealmFields → UPDATE returns
//
//	pgconn.PgError SQLSTATE 23505 → not ErrNoRows → propagated
//	directly without probing the tenant row.
func TestProvisioning_SetRealmFields_UniqueConstraintViolation_Propagates(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint \"tenants_realm_id_key\""}
	tx := &ffTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &ffErrScanRow{err: pgErr}
		},
	}
	svc := buildProvisioningWithTxRunner(&ffPassthroughTxRunner{tx: tx})
	_, err := svc.SetRealmFields(context.Background(), uuid.New(),
		"duplicate-realm", domain.RealmType("dedicated"), "shard-1", 1)

	require.Error(t, err)
	var gotPg *pgconn.PgError
	require.ErrorAs(t, err, &gotPg, "I2-TX-02: pgconn.PgError must propagate from SetRealmFields")
	assert.Equal(t, "23505", gotPg.Code, "SQLSTATE 23505 unique_violation")
}
