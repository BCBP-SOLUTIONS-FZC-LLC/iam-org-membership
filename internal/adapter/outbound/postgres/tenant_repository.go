package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TenantRepository implements port.TenantRepository against Postgres.
// All reads and writes go through the RLS-scoped app pool; the tenants
// policy uses `id = current_setting('app.tenant_id')` so a caller can
// only touch their own row.
type TenantRepository struct {
	pool *pgcommon.Pool
}

var _ port.TenantRepository = (*TenantRepository)(nil)

func NewTenantRepository(pool *pgcommon.Pool) *TenantRepository {
	return &TenantRepository{pool: pool}
}

const tenantSelectColumns = `
	id, slug, name, plan, feature_flags, status,
	trial_ends_at, trial_reactivation_count, subscription_started_at,
	cancelled_at, last_event_at,
	realm_id, realm_type, keycloak_shard, mfa_freshness_seconds,
	local_accounts_enabled, realm_sync_pending, default_locale,
	licensed_seats, ownerless_since, overage_since,
	record_version, created_at, updated_at, deleted_at`

func (r *TenantRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	var t *domain.Tenant
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+tenantSelectColumns+` FROM tenants WHERE id = $1 AND deleted_at IS NULL`, id)
		found, scanErr := scanTenant(row)
		if scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				return domain.NewError(domain.ErrTenantNotFound, "tenant not found")
			}
			return scanErr
		}
		t = found
		return nil
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// FindByIDIncludingDeleted returns the tenant row regardless of deleted_at.
// Used only by iam-system internal paths (I-2 RP cleanup, reconcilers).
func (r *TenantRepository) FindByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	var t *domain.Tenant
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+tenantSelectColumns+` FROM tenants WHERE id = $1`, id)
		found, scanErr := scanTenant(row)
		if scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				return domain.NewError(domain.ErrTenantNotFound, "tenant not found")
			}
			return scanErr
		}
		t = found
		return nil
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// Update applies patch under optimistic-locking (CONC-1..4). The trigger
// touch_row owns record_version and updated_at — this SQL never sets them
// (TRG-1/TRG-2). WHERE record_version = $expected returns 0 rows on
// version mismatch; the code translates that to
// ErrOptimisticLockConflict + Details{record_version, updated_at}.
func (r *TenantRepository) Update(ctx context.Context, id uuid.UUID, patch *domain.TenantPatch) (*domain.Tenant, error) {
	if patch == nil {
		return nil, domain.NewError(domain.ErrValidation, "patch is required")
	}

	// Build the SET list from non-nil fields. FeatureFlags is ignored on
	// P-2 — operator-only via O-4.
	sets := make([]string, 0, 4)
	args := []any{id, patch.RecordVersion}
	next := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if patch.Name != nil {
		sets = append(sets, "name = "+next(*patch.Name))
	}
	if patch.DefaultLocale != nil {
		sets = append(sets, "default_locale = "+next(*patch.DefaultLocale))
	}
	if patch.LocalAccountsEnabled != nil {
		sets = append(sets, "local_accounts_enabled = "+next(*patch.LocalAccountsEnabled))
	}
	if patch.MFAFreshnessSeconds != nil {
		sets = append(sets, "mfa_freshness_seconds = "+next(*patch.MFAFreshnessSeconds))
	}
	if len(sets) == 0 {
		// Nothing to update — return the current row (idempotent PATCH).
		return r.FindByID(ctx, id)
	}

	sql := `UPDATE tenants SET ` + strings.Join(sets, ", ") +
		` WHERE id = $1 AND record_version = $2 AND deleted_at IS NULL RETURNING ` + tenantSelectColumns

	var out *domain.Tenant
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, sql, args...)
		found, scanErr := scanTenant(row)
		if scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				// Distinguish "not found" from "version conflict" by re-reading
				// the row without the version predicate. On a genuine 404 the
				// tenants policy returns 0 rows too — so we probe first.
				return r.optimisticConflictOrNotFound(ctx, tx, id)
			}
			return scanErr
		}
		out = found
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetRealmSyncPending marks realm_sync_pending=true for the realm-config-sync
// reconciler (T-15/§16 A58). Called when RP.PatchRealmConfig fails post-commit.
func (r *TenantRepository) SetRealmSyncPending(ctx context.Context, tenantID uuid.UUID) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx,
			`UPDATE tenants SET realm_sync_pending = true WHERE id = $1 AND deleted_at IS NULL`,
			tenantID)
		if err != nil {
			return err
		}
		if cmd.RowsAffected() == 0 {
			return domain.NewError(domain.ErrTenantNotFound, "tenant not found")
		}
		return nil
	})
}

// Insert honours LLD I-1 idempotency: INSERT ... ON CONFLICT (id) DO NOTHING
// RETURNING. If a row with the given id already exists the RETURNING is empty
// (no row scanned); we then fetch and return the existing row with
// wasCreated=false so the handler can respond 200 (idempotent replay) instead
// of 201 (fresh create). A slug collision on a different id still lands on
// uq_tenants_slug (23505) — mapped to ErrSlugAlreadyTaken so the caller sees
// 409 rather than a raw PgError.
func (r *TenantRepository) Insert(ctx context.Context, t *domain.Tenant) (*domain.Tenant, bool, error) {
	if t == nil {
		return nil, false, domain.NewError(domain.ErrValidation, "tenant is required")
	}
	featureFlagsJSON, err := json.Marshal(t.FeatureFlags)
	if err != nil {
		return nil, false, fmt.Errorf("marshal feature_flags: %w", err)
	}
	if len(t.FeatureFlags) == 0 {
		featureFlagsJSON = []byte(`{}`)
	}
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}

	var out *domain.Tenant
	var wasCreated bool
	err = withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO tenants (
				id, slug, name, plan, feature_flags, status,
				trial_ends_at, subscription_started_at, cancelled_at,
				realm_id, realm_type, keycloak_shard, mfa_freshness_seconds,
				local_accounts_enabled, default_locale, licensed_seats
			) VALUES (
				$1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16
			) ON CONFLICT (id) DO NOTHING
			RETURNING `+tenantSelectColumns,
			t.ID, t.Slug, t.Name, string(t.Plan), string(featureFlagsJSON), string(t.Status),
			t.TrialEndsAt, t.SubscriptionStartedAt, t.CancelledAt,
			t.RealmID, string(t.RealmType), t.KeycloakShard, t.MFAFreshnessSeconds,
			t.LocalAccountsEnabled, t.DefaultLocale, t.LicensedSeats)
		found, scanErr := scanTenant(row)
		if scanErr != nil {
			var pgErr *pgconn.PgError
			if errors.As(scanErr, &pgErr) && pgErr.Code == "23505" && strings.Contains(pgErr.ConstraintName, "slug") {
				return domain.NewError(domain.ErrSlugAlreadyTaken, "slug already taken")
			}
			if errors.Is(scanErr, pgx.ErrNoRows) {
				// ON CONFLICT (id) DO NOTHING — row already exists. Load it and
				// return with wasCreated=false so the handler serves 200.
				existing, existingErr := scanTenant(tx.QueryRow(ctx, `SELECT `+tenantSelectColumns+` FROM tenants WHERE id = $1 AND deleted_at IS NULL`, t.ID))
				if existingErr != nil {
					return existingErr
				}
				out = existing
				wasCreated = false
				return nil
			}
			return scanErr
		}
		out = found
		wasCreated = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, wasCreated, nil
}

// optimisticConflictOrNotFound probes the row without a version predicate
// so callers get the right error code (CONC-3 vs 404).
func (r *TenantRepository) optimisticConflictOrNotFound(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	row := tx.QueryRow(ctx, `SELECT record_version, updated_at FROM tenants WHERE id = $1 AND deleted_at IS NULL`, id)
	var currentVersion int64
	var updatedAt any
	if err := row.Scan(&currentVersion, &updatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.ErrTenantNotFound, "tenant not found")
		}
		return err
	}
	return domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict").WithDetails(map[string]any{
		"record_version": currentVersion,
		"updated_at":     updatedAt,
	})
}
