package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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
	cancelled_at, suspension_source, last_event_at,
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
			if pgcommon.IsUniqueViolation(scanErr) && strings.Contains(pgcommon.ConstraintName(scanErr), "slug") {
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

// ListSubscriptionLapses is I-16 (§16 RP-C3). Cross-tenant — must be called
// against a BYPASSRLS-bound repository instance (sysPool), never the
// RLS-scoped app pool; see the port.TenantRepository doc comment.
func (r *TenantRepository) ListSubscriptionLapses(ctx context.Context, graceDays int) ([]domain.Tenant, error) {
	var out []domain.Tenant
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+tenantSelectColumns+`
			FROM tenants
			WHERE status = 'cancelled'
			  AND cancelled_at <= now() - ($1::int * interval '1 day')
			  AND deleted_at IS NULL
			ORDER BY cancelled_at ASC`, graceDays)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, scanErr := scanTenant(rows)
			if scanErr != nil {
				return scanErr
			}
			out = append(out, *t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (r *TenantRepository) LockByID(ctx context.Context, id uuid.UUID) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `SELECT id FROM tenants WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.NewError(domain.ErrTenantNotFound, "tenant not found")
		}
		return nil
	})
}

func (r *TenantRepository) LicensedSeatsForUpdate(ctx context.Context, id uuid.UUID) (int, error) {
	var n int
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT licensed_seats FROM tenants WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(&n)
	})
	return n, err
}

func (r *TenantRepository) SetFeatureFlags(ctx context.Context, id uuid.UUID, flags []byte, expectedVersion int64) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE tenants SET feature_flags = $2::jsonb WHERE id = $1 AND record_version = $3 AND deleted_at IS NULL`,
			id, string(flags), expectedVersion)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var current int64
			probeErr := tx.QueryRow(ctx,
				`SELECT record_version FROM tenants WHERE id = $1 AND deleted_at IS NULL`,
				id).Scan(&current)
			if probeErr != nil {
				return domain.NewError(domain.ErrTenantNotFound, "tenant not found")
			}
			return domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict").
				WithDetails(map[string]any{"record_version": current})
		}
		return nil
	})
}

func (r *TenantRepository) ClearOwnerlessSince(ctx context.Context, id uuid.UUID) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET ownerless_since = NULL WHERE id = $1`, id)
		return err
	})
}

func (r *TenantRepository) MarkOwnerlessIfUnset(ctx context.Context, id uuid.UUID) (bool, error) {
	var flipped bool
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE tenants SET ownerless_since = now() WHERE id = $1 AND ownerless_since IS NULL`, id)
		if err != nil {
			return err
		}
		flipped = tag.RowsAffected() > 0
		return nil
	})
	return flipped, err
}

func (r *TenantRepository) SetRealmFields(ctx context.Context, id uuid.UUID, realmID string, realmType domain.RealmType, shard string, recordVersion int64) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE tenants SET realm_id = $2, realm_type = $3, keycloak_shard = $4 WHERE id = $1 AND record_version = $5`,
			id, realmID, string(realmType), shard, recordVersion)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var current int64
			probe := tx.QueryRow(ctx, `SELECT record_version FROM tenants WHERE id = $1`, id)
			if perr := probe.Scan(&current); perr != nil {
				return domain.NewError(domain.ErrTenantNotFound, "tenant not found")
			}
			return domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict").
				WithDetails(map[string]any{"record_version": current})
		}
		return nil
	})
}

func (r *TenantRepository) LockSeatOccupancy(ctx context.Context, id uuid.UUID) (port.SeatOccupancy, error) {
	var occ port.SeatOccupancy
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT licensed_seats, overage_since FROM tenants
			WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, id).
			Scan(&occ.LicensedSeats, &occ.OverageSince); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM tenant_memberships
			WHERE tenant_id = $1 AND deleted_at IS NULL AND status = 'active'`, id).Scan(&occ.Active); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM pending_invitations
			WHERE tenant_id = $1 AND status = 'pending' AND expires_at > now()`, id).Scan(&occ.Pending)
	})
	return occ, err
}

func (r *TenantRepository) SetOverageSince(ctx context.Context, id uuid.UUID, since *time.Time) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET overage_since = $2 WHERE id = $1`, id, since)
		return err
	})
}

func (r *TenantRepository) LockForProjection(ctx context.Context, id uuid.UUID) (*port.TenantProjectionLock, error) {
	var lock *port.TenantProjectionLock
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		var status, plan string
		var lastEventAt *time.Time
		err := tx.QueryRow(ctx, `
			SELECT status, plan, last_event_at
			FROM tenants WHERE id = $1 AND deleted_at IS NULL
			FOR UPDATE`, id).Scan(&status, &plan, &lastEventAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		lock = &port.TenantProjectionLock{
			Status:      domain.SubscriptionStatus(status),
			Plan:        domain.TenantPlan(plan),
			LastEventAt: lastEventAt,
		}
		return nil
	})
	return lock, err
}

func (r *TenantRepository) SetLastEventAt(ctx context.Context, id uuid.UUID, t time.Time) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET last_event_at = $2 WHERE id = $1`, id, t)
		return err
	})
}

func (r *TenantRepository) ApplyLifecyclePatch(ctx context.Context, id uuid.UUID, patch port.TenantLifecyclePatch) (int64, error) {
	var n int64
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := execLifecyclePatch(ctx, tx, id, patch)
		if err != nil {
			return err
		}
		n = rows
		return nil
	})
	return n, err
}

func execLifecyclePatch(ctx context.Context, tx pgx.Tx, id uuid.UUID, patch port.TenantLifecyclePatch) (int64, error) {
	var (
		tag pgconn.CommandTag
		err error
	)
	switch patch.Op {
	case port.LifecycleSetRealm:
		tag, err = tx.Exec(ctx, `
			UPDATE tenants SET realm_id = $2, realm_type = $3, keycloak_shard = $4
			WHERE id = $1`, id, patch.RealmID, patch.RealmType, patch.KeycloakShard)
	case port.LifecycleActivatePaid:
		tag, err = tx.Exec(ctx, `
			UPDATE tenants SET status = 'active', subscription_started_at = now(), plan = $2,
			                    suspension_source = NULL
			WHERE id = $1`, id, string(patch.Plan))
	case port.LifecycleSetStatusClearSuspension:
		tag, err = tx.Exec(ctx, `UPDATE tenants SET status = $2, suspension_source = NULL WHERE id = $1`,
			id, string(patch.Status))
	case port.LifecycleTrialReactivate:
		tag, err = tx.Exec(ctx, `
			UPDATE tenants t
			SET status = 'trial',
			    trial_ends_at = now() + make_interval(days => $2),
			    trial_reactivation_count = trial_reactivation_count + 1,
			    suspension_source = NULL
			WHERE t.id = $1 AND t.trial_reactivation_count < 1`, id, patch.TrialDurationDays)
	case port.LifecycleSuspendBillingLapse:
		tag, err = tx.Exec(ctx, `
			UPDATE tenants SET status = 'suspended',
			                    suspension_source = $2,
			                    cancelled_at = COALESCE(cancelled_at, now())
			WHERE id = $1`, id, string(domain.SuspensionSourceBillingLapse))
	case port.LifecycleSuspendOperator:
		tag, err = tx.Exec(ctx, `
			UPDATE tenants SET status = 'suspended', suspension_source = $2
			WHERE id = $1`, id, string(domain.SuspensionSourceOperator))
	case port.LifecycleOffboard:
		tag, err = tx.Exec(ctx, `
			UPDATE tenants SET status = 'offboarded', deleted_at = now(),
			                    cancelled_at = COALESCE(cancelled_at, now()),
			                    suspension_source = NULL
			WHERE id = $1`, id)
	case port.LifecycleSetPlan:
		tag, err = tx.Exec(ctx, `UPDATE tenants SET plan = $2 WHERE id = $1`, id, string(patch.Plan))
	case port.LifecycleCancel:
		tag, err = tx.Exec(ctx, `
			UPDATE tenants SET status = 'cancelled', cancelled_at = COALESCE(cancelled_at, now()),
			                    suspension_source = NULL WHERE id = $1`, id)
	case port.LifecycleReactivatePaid:
		tag, err = tx.Exec(ctx, `
			UPDATE tenants SET status = 'active', cancelled_at = NULL, suspension_source = NULL
			WHERE id = $1 AND subscription_started_at IS NOT NULL`, id)
	case port.LifecycleReactivateTrial:
		tag, err = tx.Exec(ctx, `
			UPDATE tenants SET status = 'trial', cancelled_at = NULL, suspension_source = NULL
			WHERE id = $1 AND subscription_started_at IS NULL`, id)
	case port.LifecycleSetLicensedSeats:
		tag, err = tx.Exec(ctx, `UPDATE tenants SET licensed_seats = $2 WHERE id = $1`, id, patch.LicensedSeats)
	default:
		return 0, fmt.Errorf("unknown tenant lifecycle op %d", patch.Op)
	}
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// gdprWipeTables are Core's own tenant-scoped tables deleted on
// TenantOffboarded (§15.5) — the tenants row itself is only soft-deleted,
// so these are explicit deletes, never an ON DELETE CASCADE side effect.
var gdprWipeTables = []string{
	"pending_invitations",
	"dept_memberships",
	"tenant_roles",
	"tenant_memberships",
	"dept_role_labels",
	"tenant_departments",
}

func (r *TenantRepository) WipeTenantChildren(ctx context.Context, id uuid.UUID) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		for _, table := range gdprWipeTables {
			if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE tenant_id = $1`, id); err != nil {
				return fmt.Errorf("GDPR wipe: delete from %s: %w", table, err)
			}
		}
		return nil
	})
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
