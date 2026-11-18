package postgres

import (
	"context"
	"encoding/json"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AuthZRepository implements port.AuthZRepository against Postgres — the
// I-8 hot-path single-query join (§8.3, PERF-1).
type AuthZRepository struct {
	pool *pgcommon.Pool
}

var _ port.AuthZRepository = (*AuthZRepository)(nil)

func NewAuthZRepository(pool *pgcommon.Pool) *AuthZRepository {
	return &AuthZRepository{pool: pool}
}

// FindMembershipProjection runs the four-table join in ONE round-trip using
// LEFT JOIN + array_agg/json_agg aggregation (PERF-1 fix). Previously this
// was 3 sequential queries inside a transaction, costing 3 PgBouncer
// round-trips per I-8 cache miss. Now it is a single QueryRow — one
// round-trip regardless of how many roles or dept memberships the user has.
// Departments come from a LATERAL jsonb_agg: each department once, ordered
// by department_id, however many elevated roles the user holds (BUG-19).
//
// NULL aggregates (user has no roles / no dept memberships) are normalised
// to empty slices so the service layer never sees nil collections.
func (r *AuthZRepository) FindMembershipProjection(ctx context.Context, tenantID, userID uuid.UUID) (*port.MembershipProjectionRow, error) {
	var out *port.MembershipProjectionRow
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		var (
			mStatus, tStatus, tPlan, tLocale, tResidencyMode string
			mfaFresh                                         int
			localAccountsEnabled                             bool
			tenantFeatureFlagsJSON                           []byte
			rolesJSON                                        []byte // array_agg result as JSON array
			deptsJSON                                        []byte // jsonb_agg result (LATERAL, BUG-19)
		)

		err := tx.QueryRow(ctx, `
			SELECT
				tm.status,
				t.status,
				t.plan,
				t.default_locale,
				t.mfa_freshness_seconds,
				t.local_accounts_enabled,
				t.residency_mode,
				t.feature_flags,
				COALESCE(
					to_json(
						array_agg(DISTINCT tr.role_code ORDER BY tr.role_code)
						FILTER (WHERE tr.role_code IS NOT NULL)
					),
					'[]'::json
				) AS roles,
				depts_agg.depts AS departments
			FROM tenant_memberships tm
			JOIN tenants t
				ON t.id = tm.tenant_id
			LEFT JOIN tenant_roles tr
				ON  tr.tenant_id  = tm.tenant_id
				AND tr.user_id    = tm.user_id
				AND tr.deleted_at IS NULL
			-- BUG-19: departments are aggregated once per membership in a
			-- LATERAL, not joined beside tenant_roles, so N roles no longer
			-- repeat every department N times. jsonb, not json: json has no
			-- equality operator, and the value is in the GROUP BY.
			LEFT JOIN LATERAL (
				SELECT COALESCE(
					jsonb_agg(
						jsonb_build_object(
							'department_id', dm.department_id,
							'role_level',    dm.role_level
						) ORDER BY dm.department_id
					),
					'[]'::jsonb
				) AS depts
				FROM dept_memberships dm
				WHERE dm.tenant_id  = tm.tenant_id
				  AND dm.user_id    = tm.user_id
				  AND dm.deleted_at IS NULL
			) depts_agg ON true
			WHERE tm.tenant_id  = $1
			  AND tm.user_id    = $2
			  AND tm.deleted_at IS NULL
			  AND tm.status <> 'left' -- TM-9: a removed member is not a member
			GROUP BY
				tm.status, t.status, t.plan, t.default_locale,
				t.mfa_freshness_seconds, t.local_accounts_enabled, t.residency_mode, t.feature_flags,
				depts_agg.depts`,
			tenantID, userID,
		).Scan(
			&mStatus, &tStatus, &tPlan, &tLocale,
			&mfaFresh, &localAccountsEnabled, &tResidencyMode, &tenantFeatureFlagsJSON,
			&rolesJSON, &deptsJSON,
		)
		if err != nil {
			if err == pgx.ErrNoRows {
				return nil // out stays nil — AuthZService maps that to 404
			}
			return err
		}

		// Unmarshal roles: JSON array of strings e.g. ["tenant_admin","tender_admin"]
		var roleStrs []string
		if err := json.Unmarshal(rolesJSON, &roleStrs); err != nil {
			return err
		}
		roles := make([]domain.TenantRoleCode, len(roleStrs))
		for i, rc := range roleStrs {
			roles[i] = domain.TenantRoleCode(rc)
		}

		// Unmarshal departments: JSON array of {department_id, role_level}
		var deptRows []struct {
			DepartmentID uuid.UUID `json:"department_id"`
			RoleLevel    string    `json:"role_level"`
		}
		if err := json.Unmarshal(deptsJSON, &deptRows); err != nil {
			return err
		}
		depts := make([]domain.DeptMembershipView, len(deptRows))
		for i, d := range deptRows {
			depts[i] = domain.DeptMembershipView{
				DepartmentID: d.DepartmentID,
				RoleLevel:    domain.DeptRole(d.RoleLevel),
			}
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
			ResidencyMode:        domain.ResidencyMode(tResidencyMode),
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
