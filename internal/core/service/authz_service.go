package service

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AuthZService owns I-8 (the hot path — every authenticated request goes
// through it). Returns the full membership projection AuthZ Enrichment
// uses to inject headers. See LLD §5.4 / §8.3 / §21.
type AuthZService struct {
	pool  *pgcommon.Pool
	plans port.PlanRepository
	cache port.Cache
}

func NewAuthZService(pool *pgcommon.Pool, plans port.PlanRepository, cache port.Cache) *AuthZService {
	return &AuthZService{pool: pool, plans: plans, cache: cache}
}

// MembershipProjection is the I-8 response envelope.
type MembershipProjection struct {
	UserID                uuid.UUID                   `json:"user_id"`
	TenantID              uuid.UUID                   `json:"tenant_id"`
	Status                domain.MembershipStatus     `json:"status"`
	Plan                  domain.TenantPlan           `json:"plan"`
	TenantStatus          domain.SubscriptionStatus   `json:"tenant_status"`
	Locale                string                      `json:"default_locale"`
	MFAFreshnessSeconds   int                         `json:"mfa_freshness_seconds"`
	LocalAccountsEnabled  bool                        `json:"local_accounts_enabled"`
	Roles                 []domain.TenantRoleCode     `json:"roles"`
	Departments           []domain.DeptMembershipView `json:"departments"`
	ActiveDelegations     []DelegationView            `json:"active_delegations"`
	EffectiveFeatureFlags map[string]any              `json:"effective_feature_flags"`
}

// DelegationView is the compact projection embedded in the I-8 response.
type DelegationView struct {
	DelegationID uuid.UUID              `json:"delegation_id"`
	DelegateID   uuid.UUID              `json:"delegate_id"`
	Scope        domain.DelegationScope `json:"scope"`
	ScopeID      *uuid.UUID             `json:"scope_id,omitempty"`
	EndsAt       *time.Time             `json:"ends_at,omitempty"`
}

// GetMembership implements I-8. Cache-through with 300 s ± 30 s jitter
// (CACHE-4). Miss/timeout/outage falls through to Postgres (CACHE-9 /
// I8-2). 404 when the caller has no active membership in the tenant.
func (s *AuthZService) GetMembership(ctx context.Context, tenantID, userID uuid.UUID) (*MembershipProjection, error) {
	if cached := s.getCached(ctx, tenantID, userID); cached != nil {
		return cached, nil
	}
	proj, err := s.readFromDB(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	if proj == nil {
		return nil, domain.NewError(domain.ErrMemberNotFound, "no active membership in this tenant")
	}
	s.setCached(ctx, proj)
	return proj, nil
}

// readFromDB executes the single joined query. Arrays are COALESCE'd to
// [] so the response body never carries JSON null in the collection
// positions (I8-4).
func (s *AuthZService) readFromDB(ctx context.Context, tenantID, userID uuid.UUID) (*MembershipProjection, error) {
	var proj *MembershipProjection

	err := pgcommon.RunInTx(ctx, s.pool, pgx.TxOptions{}, func(txCtx context.Context, tx pgx.Tx) error {
		// Membership + tenant fields joined.
		var (
			mStatus, tStatus, tPlan, tLocale string
			mfaFresh                         int
			localAccountsEnabled             bool
			tenantFeatureFlagsJSON           []byte
		)
		err := tx.QueryRow(txCtx, `
			SELECT tm.status, t.status, t.plan, t.default_locale, t.mfa_freshness_seconds,
			       t.local_accounts_enabled, t.feature_flags
			FROM tenant_memberships tm
			JOIN tenants t ON t.id = tm.tenant_id
			WHERE tm.tenant_id = $1 AND tm.user_id = $2 AND tm.deleted_at IS NULL AND tm.status = 'active'`,
			tenantID, userID,
		).Scan(&mStatus, &tStatus, &tPlan, &tLocale, &mfaFresh, &localAccountsEnabled, &tenantFeatureFlagsJSON)
		if err != nil {
			if err == pgx.ErrNoRows {
				return nil // 404 → caller sees ErrMemberNotFound
			}
			return err
		}

		// Elevated roles.
		roles := []domain.TenantRoleCode{domain.RoleMember} // TR-7 derived injection
		rows, err := tx.Query(txCtx, `
			SELECT role_code FROM tenant_roles
			WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL
			ORDER BY role_code`,
			tenantID, userID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				rows.Close()
				return err
			}
			roles = append(roles, domain.TenantRoleCode(r))
		}
		rows.Close()

		// Dept memberships.
		depts := []domain.DeptMembershipView{}
		drows, err := tx.Query(txCtx, `
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

		// Active delegations initiated by the caller (delegator-side).
		delegations := []DelegationView{}
		delrows, err := tx.Query(txCtx, `
			SELECT id, delegate_id, scope, scope_id, ends_at FROM delegations
			WHERE tenant_id = $1 AND delegator_id = $2 AND deleted_at IS NULL AND status = 'active'`,
			tenantID, userID)
		if err != nil {
			return err
		}
		for delrows.Next() {
			var dv DelegationView
			var scope string
			if err := delrows.Scan(&dv.DelegationID, &dv.DelegateID, &scope, &dv.ScopeID, &dv.EndsAt); err != nil {
				delrows.Close()
				return err
			}
			dv.Scope = domain.DelegationScope(scope)
			delegations = append(delegations, dv)
		}
		delrows.Close()

		// Effective feature flags = planDefaults(plan) ⊕ tenants.feature_flags (PLAN-6).
		effective := map[string]any{}
		plan, err := s.plans.FindByCode(ctx, domain.TenantPlan(tPlan))
		if err == nil && plan != nil {
			for k, v := range plan.FeatureSet {
				effective[k] = v
			}
		}
		tenantFlags := map[string]any{}
		if len(tenantFeatureFlagsJSON) > 0 && string(tenantFeatureFlagsJSON) != "null" {
			_ = json.Unmarshal(tenantFeatureFlagsJSON, &tenantFlags)
		}
		for k, v := range tenantFlags {
			effective[k] = v
		}

		proj = &MembershipProjection{
			UserID:                userID,
			TenantID:              tenantID,
			Status:                domain.MembershipStatus(mStatus),
			Plan:                  domain.TenantPlan(tPlan),
			TenantStatus:          domain.SubscriptionStatus(tStatus),
			Locale:                tLocale,
			MFAFreshnessSeconds:   mfaFresh,
			LocalAccountsEnabled:  localAccountsEnabled,
			Roles:                 roles,
			Departments:           depts,
			ActiveDelegations:     delegations,
			EffectiveFeatureFlags: effective,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return proj, nil
}

// ── cache with jitter ─────────────────────────────────────────────────

const (
	membershipCacheBaseTTL = 300 * time.Second
	membershipCacheJitter  = 30 * time.Second
)

func (s *AuthZService) getCached(ctx context.Context, tenantID, userID uuid.UUID) *MembershipProjection {
	if s.cache == nil {
		return nil
	}
	raw, err := s.cache.Get(ctx, cacheKeyMemberships(tenantID, userID))
	if err != nil || raw == nil {
		return nil
	}
	var proj MembershipProjection
	if err := json.Unmarshal(raw, &proj); err != nil {
		return nil
	}
	return &proj
}

func (s *AuthZService) setCached(ctx context.Context, proj *MembershipProjection) {
	if s.cache == nil || proj == nil {
		return
	}
	raw, err := json.Marshal(proj)
	if err != nil {
		return
	}
	// CACHE-4 jitter: +/- 10% around base to prevent stampede.
	//nolint:gosec // pseudo-random jitter — not security-sensitive
	jitter := time.Duration(rand.Int64N(int64(membershipCacheJitter*2))) - membershipCacheJitter
	ttl := membershipCacheBaseTTL + jitter
	_ = s.cache.Set(ctx, cacheKeyMemberships(proj.TenantID, proj.UserID), raw, ttl)
}

func cacheKeyMemberships(tenantID, userID uuid.UUID) string {
	return "om:memberships:" + tenantID.String() + ":" + userID.String()
}
