package postgres

import (
	"context"
	"encoding/json"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AuthZRepository implements port.AuthZRepository against Postgres — the
// I-8 hot-path join (§8.3), moved here from core/service.AuthZService so
// the service layer depends only on a port, never on *pgcommon.Pool
// directly. All reads go through the RLS-scoped app pool exactly as
// before; only the layering changed, not the query, the transaction
// boundary, or the RLS GUC behavior.
type AuthZRepository struct {
	pool *pgcommon.Pool
}

var _ port.AuthZRepository = (*AuthZRepository)(nil)

func NewAuthZRepository(pool *pgcommon.Pool) *AuthZRepository {
	return &AuthZRepository{pool: pool}
}

// FindMembershipProjection runs the four-table join in one transaction.
// withPool's wrapConnErr classifies transport failures into
// ErrDependencyUnavailable exactly like every other repository in this
// package — the pre-migration code called pgcommon.RunInTx directly and
// so never got that classification on I-8.
func (r *AuthZRepository) FindMembershipProjection(ctx context.Context, tenantID, userID uuid.UUID) (*port.MembershipProjectionRow, error) {
	var out *port.MembershipProjectionRow
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		var (
			mStatus, tStatus, tPlan, tLocale string
			mfaFresh                         int
			localAccountsEnabled             bool
			tenantFeatureFlagsJSON           []byte
		)
		err := tx.QueryRow(ctx, `
			SELECT tm.status, t.status, t.plan, t.default_locale, t.mfa_freshness_seconds,
			       t.local_accounts_enabled, t.feature_flags
			FROM tenant_memberships tm
			JOIN tenants t ON t.id = tm.tenant_id
			WHERE tm.tenant_id = $1 AND tm.user_id = $2 AND tm.deleted_at IS NULL`,
			tenantID, userID,
		).Scan(&mStatus, &tStatus, &tPlan, &tLocale, &mfaFresh, &localAccountsEnabled, &tenantFeatureFlagsJSON)
		if err != nil {
			if err == pgx.ErrNoRows {
				return nil // out stays nil — caller (AuthZService) maps that to 404
			}
			return err
		}

		rows, err := tx.Query(ctx, `
			SELECT role_code FROM tenant_roles
			WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL
			ORDER BY role_code`,
			tenantID, userID)
		if err != nil {
			return err
		}
		var roles []domain.TenantRoleCode
		for rows.Next() {
			var rc string
			if err := rows.Scan(&rc); err != nil {
				rows.Close()
				return err
			}
			roles = append(roles, domain.TenantRoleCode(rc))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		depts := []domain.DeptMembershipView{}
		drows, err := tx.Query(ctx, `
			SELECT department_id, role_level FROM dept_memberships
			WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL`,
			tenantID, userID)
		if err != nil {
			return err
		}
		for drows.Next() {
			var view domain.DeptMembershipView
			var lvl string
			if err := drows.Scan(&view.DepartmentID, &lvl); err != nil {
				drows.Close()
				return err
			}
			view.RoleLevel = domain.DeptRole(lvl)
			depts = append(depts, view)
		}
		drows.Close()
		if err := drows.Err(); err != nil {
			return err
		}

		tenantFlags := map[string]any{}
		if len(tenantFeatureFlagsJSON) > 0 && string(tenantFeatureFlagsJSON) != "null" {
			if err := json.Unmarshal(tenantFeatureFlagsJSON, &tenantFlags); err != nil {
				return err
			}
		}

		out = &port.MembershipProjectionRow{
			MembershipStatus:     domain.MembershipStatus(mStatus),
			TenantPlan:           domain.TenantPlan(tPlan),
			SubscriptionStatus:   domain.SubscriptionStatus(tStatus),
			Locale:               tLocale,
			MFAFreshnessSeconds:  mfaFresh,
			LocalAccountsEnabled: localAccountsEnabled,
			TenantFeatureFlags:   tenantFlags,
			Roles:                roles,
			Departments:          depts,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
