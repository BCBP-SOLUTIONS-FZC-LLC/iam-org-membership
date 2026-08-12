package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── stubs (whitebox, package-private) ──────────────────────────────────

type ffPassthroughTxRunner struct {
	tx     pgx.Tx
	runErr error
}

func (r *ffPassthroughTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if r.runErr != nil {
		return r.runErr
	}
	// Inject the tx via the service-layer key so pgadapterTxFromContext can
	// read it — mirroring what postgres.TxRunner does in production.
	return fn(WithTx(ctx, r.tx))
}

type ffTx struct {
	pgx.Tx
	execFn     func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	queryRowFn func(ctx context.Context, sql string, args ...any) pgx.Row
}

func (f *ffTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if f.execFn != nil {
		return f.execFn(ctx, sql, args...)
	}
	return pgconn.CommandTag{}, nil
}

func (f *ffTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if f.queryRowFn != nil {
		return f.queryRowFn(ctx, sql, args...)
	}
	// Default: simulate not-found (no row) for the OL probe query.
	return &ffNoRow{}
}

// ffNoRow is a pgx.Row that always returns ErrNoRows on Scan.
type ffNoRow struct{}

func (r *ffNoRow) Scan(dest ...any) error { return pgx.ErrNoRows }

type ffTenantRepo struct {
	findByIDFn func(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)
}

func (r *ffTenantRepo) FindByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return r.findByIDFn(ctx, id)
}
func (r *ffTenantRepo) FindByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return r.FindByID(ctx, id)
}
func (r *ffTenantRepo) Update(context.Context, uuid.UUID, *domain.TenantPatch) (*domain.Tenant, error) {
	return nil, nil
}
func (r *ffTenantRepo) SetRealmSyncPending(context.Context, uuid.UUID) error { return nil }
func (r *ffTenantRepo) Insert(context.Context, *domain.Tenant) (*domain.Tenant, bool, error) {
	return nil, false, nil
}

type ffCache struct {
	deleteCalls []string
}

func (c *ffCache) Get(context.Context, string) ([]byte, error)      { return nil, nil }
func (c *ffCache) MGet(context.Context, []string) ([][]byte, error) { return nil, nil }
func (c *ffCache) Set(context.Context, string, []byte, time.Duration) error {
	return nil
}
func (c *ffCache) SetNX(context.Context, string, []byte, time.Duration) (bool, error) {
	return false, nil
}
func (c *ffCache) Delete(_ context.Context, keys ...string) error {
	c.deleteCalls = append(c.deleteCalls, keys...)
	return nil
}
func (c *ffCache) Health(context.Context) error { return nil }
func (c *ffCache) Close() error                 { return nil }

var _ port.Cache = (*ffCache)(nil)
var _ port.TenantRepository = (*ffTenantRepo)(nil)

// buildOperatorWithPool wires an OperatorService for the SetFeatureFlags
// tests. Only the tenants repo, tx runner, cache are populated.
func buildOperatorWithPool(tenants port.TenantRepository, tr port.TxRunner, cache port.Cache) *OperatorService {
	return &OperatorService{
		tenants: tenants, txRunner: tr, cache: cache,
	}
}

// ── SetFeatureFlags — validation branch ─────────────────────────────────

func TestOperator_SetFeatureFlags_NestedObjectRejected(t *testing.T) {
	svc := buildOperatorWithPool(nil, nil, nil)
	// Use an allow-listed key so the scalar-value check is what fires, not
	// the allow-list check (which runs first).
	_, err := svc.SetFeatureFlags(context.Background(), uuid.New(),
		map[string]any{"custom_branding": map[string]string{"k": "v"}}, 1)

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
	assert.Equal(t, "invalid_feature_value", de.Details["code"])
	assert.Equal(t, "custom_branding", de.Details["key"])
}

func TestOperator_SetFeatureFlags_ArrayRejected(t *testing.T) {
	svc := buildOperatorWithPool(nil, nil, nil)
	_, err := svc.SetFeatureFlags(context.Background(), uuid.New(),
		map[string]any{"sso_enabled": []int{1, 2, 3}}, 1)

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "invalid_feature_value", de.Details["code"])
}

func TestOperator_SetFeatureFlags_UnknownKeyRejected(t *testing.T) {
	svc := buildOperatorWithPool(nil, nil, nil)
	// LLD O-4: keys must be in the allow-list; a typo like "sso_enable"
	// (missing 'd') is rejected with 400 unknown_feature_flag rather than
	// silently stored as a dead override.
	_, err := svc.SetFeatureFlags(context.Background(), uuid.New(),
		map[string]any{"sso_enable": true}, 1)

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
	assert.Equal(t, "unknown_feature_flag", de.Details["code"])
	assert.Equal(t, "sso_enable", de.Details["key"])
}

func TestOperator_SetFeatureFlags_AllScalarTypesAccepted(t *testing.T) {
	tenantID := uuid.New()
	tenants := &ffTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id}, nil
		},
	}
	// Exec must report 1 row affected so the optimistic-lock branch does
	// not misfire in the mocked path.
	tx := &ffTx{execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}}
	tr := &ffPassthroughTxRunner{tx: tx}
	svc := buildOperatorWithPool(tenants, tr, nil)

	_, err := svc.SetFeatureFlags(context.Background(), tenantID, map[string]any{
		"sso_enabled":           true,
		"custom_branding":       "logo",
		"require_mfa_all_users": true,
	}, 1)
	require.NoError(t, err)
}

// ── SetFeatureFlags — happy path invokes SQL exec on the injected tx ───

func TestOperator_SetFeatureFlags_HappyPathUpdatesTenant(t *testing.T) {
	tenantID := uuid.New()
	var gotSQL string
	tx := &ffTx{
		execFn: func(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
			gotSQL = sql
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	tenants := &ffTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, RecordVersion: 2}, nil
		},
	}
	cache := &ffCache{}
	svc := buildOperatorWithPool(tenants, &ffPassthroughTxRunner{tx: tx}, cache)

	got, err := svc.SetFeatureFlags(context.Background(), tenantID, map[string]any{"sso_enabled": true}, 2)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Contains(t, gotSQL, "UPDATE tenants SET feature_flags")
	assert.Contains(t, gotSQL, "record_version = $3")
	assert.Contains(t, gotSQL, "deleted_at IS NULL")
	assert.EqualValues(t, 2, got.RecordVersion)
	// Cache eviction on success — the tenant row was mutated.
	assert.Contains(t, cache.deleteCalls, "om:tenant:"+tenantID.String())
}

// ── SetFeatureFlags — nil flags map is defaulted to {} ─────────────────

func TestOperator_SetFeatureFlags_NilMapDefaultsToEmpty(t *testing.T) {
	tenants := &ffTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id}, nil
		},
	}
	tx := &ffTx{execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}}
	svc := buildOperatorWithPool(tenants, &ffPassthroughTxRunner{tx: tx}, nil)
	_, err := svc.SetFeatureFlags(context.Background(), uuid.New(), nil, 1)
	require.NoError(t, err, "nil flags map must be treated as empty, not rejected")
}

// ── SetFeatureFlags — tx-unavailable branch (defense in depth) ─────────

func TestOperator_SetFeatureFlags_TxUnavailableSurfaces(t *testing.T) {
	// TxRunner returns fn(ctx) WITHOUT injecting a tx → pgadapterTxFromContext
	// finds nothing → conflict error.
	tr := &noInjectTxRunner{}
	svc := buildOperatorWithPool(nil, tr, nil)
	_, err := svc.SetFeatureFlags(context.Background(), uuid.New(), map[string]any{}, 1)
	assert.ErrorIs(t, err, domain.ErrConflict)
}

// ── SetFeatureFlags — sql exec failure propagates ──────────────────────

func TestOperator_SetFeatureFlags_ExecErrorPropagates(t *testing.T) {
	execErr := errors.New("db down")
	tx := &ffTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	svc := buildOperatorWithPool(nil, &ffPassthroughTxRunner{tx: tx}, nil)
	_, err := svc.SetFeatureFlags(context.Background(), uuid.New(), map[string]any{}, 1)
	assert.ErrorIs(t, err, execErr)
}

// ── SetFeatureFlags — post-commit FindByID failure surfaces ────────────

func TestOperator_SetFeatureFlags_TenantReadErrorAfterUpdateSurfaces(t *testing.T) {
	tenants := &ffTenantRepo{
		findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
			return nil, errors.New("tenant gone")
		},
	}
	tx := &ffTx{execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}}
	svc := buildOperatorWithPool(tenants, &ffPassthroughTxRunner{tx: tx}, nil)
	_, err := svc.SetFeatureFlags(context.Background(), uuid.New(), map[string]any{"sso_enabled": true}, 1)
	assert.ErrorContains(t, err, "tenant gone")
}

// noInjectTxRunner is a TxRunner that calls fn(ctx) WITHOUT injecting a
// tx via service.WithTx — used to exercise the "tx unavailable" error
// path in SetFeatureFlags / SetRealmFields.
type noInjectTxRunner struct{}

func (noInjectTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(ctx)
}
