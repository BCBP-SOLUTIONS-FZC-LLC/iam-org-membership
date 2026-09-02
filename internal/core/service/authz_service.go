package service

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

// AuthZService owns I-8 (the hot path — every authenticated request goes
// through it). Returns the full membership projection AuthZ Enrichment
// uses to inject headers. See LLD §5.4 / §8.3 / §21.
//
// Depends only on port.AuthZRepository, never on *pgcommon.Pool directly
// (that would be a Clean Architecture violation — core/service importing
// a vendor DB type instead of a port). The four-table join itself lives
// in internal/adapter/outbound/postgres.AuthZRepository; this service owns
// the cross-port composition on top of it: TR-7's derived "member" role,
// department-code enrichment (port.DepartmentCatalogReader), and PLAN-6's
// effective-feature-flags merge (port.PlanCatalogReader) — none of which
// belong in a Postgres adapter.
type AuthZService struct {
	repo  port.AuthZRepository
	plans port.PlanCatalogReader
	depts port.DepartmentCatalogReader
	cache port.Cache
}

func NewAuthZService(repo port.AuthZRepository, plans port.PlanCatalogReader, depts port.DepartmentCatalogReader, cache port.Cache) *AuthZService {
	return &AuthZService{repo: repo, plans: plans, depts: depts, cache: cache}
}

// MembershipProjection is the I-8 response envelope.
//
// LLD rev 1.50 (§16 A53): field `subscription_status` is the authoritative
// name (matches billing/subscription vocabulary). Legacy `tenant_status`
// alias kept for one release so downstream consumers can migrate without
// a lock-step deploy; it will be removed once AuthZ Enrichment ships
// rev 1.50-compat.
//
// `read_only` is DERIVED at read time from subscription_status so AuthZ
// Enrichment can rebuild the `x-feature-flags` `read_only` claim on a
// cold cache-miss without additional round-trips (§16 A53 CACHE-9).
type MembershipProjection struct {
	UserID               uuid.UUID                   `json:"user_id"`
	TenantID             uuid.UUID                   `json:"tenant_id"`
	Status               domain.MembershipStatus     `json:"status"`
	Plan                 domain.TenantPlan           `json:"plan"`
	TenantStatus         domain.SubscriptionStatus   `json:"tenant_status"` // deprecated alias — see subscription_status
	SubscriptionStatus   domain.SubscriptionStatus   `json:"subscription_status"`
	ReadOnly             bool                        `json:"read_only"`
	Locale               string                      `json:"default_locale"`
	MFAFreshnessSeconds  int                         `json:"mfa_freshness_seconds"`
	LocalAccountsEnabled bool                        `json:"local_accounts_enabled"`
	Roles                []domain.TenantRoleCode     `json:"roles"`
	Departments          []domain.DeptMembershipView `json:"departments"`
	// FeatureFlags is the effective set (planDefaults(plan) ⊕
	// tenants.feature_flags, PLAN-6), projected as the sorted list of
	// currently-enabled flag names — matches LLD §5.4's documented I-8
	// shape. Corrected: previously an object keyed by flag name
	// (`effective_feature_flags`), which didn't match the LLD.
	FeatureFlags []string `json:"feature_flags"`
}

// readOnlyForStatus returns true for subscription states that must render
// the tenant as read-only in the UI. LLD §16 A53 (line 2394+2418):
// `read_only = true iff subscription_status='cancelled'`. Suspended and
// offboarded tenants are blocked at earlier gates (Keycloak returns 403
// on login; the tenant row is soft-deleted respectively), so they never
// reach this projection with a live user session — narrowing the check
// to match the spec literally.
func readOnlyForStatus(status domain.SubscriptionStatus) bool {
	return status == domain.StatusCancelled
}

// GetMembership implements I-8. Cache-through with 300 s ± 30 s jitter
// (CACHE-4). Miss/timeout/outage falls through to Postgres (CACHE-9 /
// I8-2). 404 when the caller has no membership (active or suspended) in the tenant.
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

// readFromDB composes the I-8 projection over port.AuthZRepository's raw
// join result. Arrays are COALESCE'd to [] so the response body never
// carries JSON null in the collection positions (I8-4).
func (s *AuthZService) readFromDB(ctx context.Context, tenantID, userID uuid.UUID) (*MembershipProjection, error) {
	row, err := s.repo.FindMembershipProjection(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil // 404 → caller (GetMembership) sees ErrMemberNotFound
	}

	// TR-7: "member" is derived at read time (never persisted in
	// tenant_roles). LLD §6.2 requires it to be injected first in the
	// effective role set so the gateway's x-tenant-roles header always
	// carries it.
	roles := append([]domain.TenantRoleCode{domain.RoleMember}, row.Roles...)

	// Department code lookup (LLD §5.4 I-8 response shape) — a single
	// om:departments cache read (CatalogService.Departments, 600s TTL),
	// not a per-department cross-service call; degrades to an empty
	// code (never fails I-8) on a cold-cache/Catalog-down intersection,
	// same posture as the feature-flags plan lookup below.
	deptCodes := map[uuid.UUID]string{}
	if s.depts != nil {
		if all, dErr := s.depts.Departments(ctx); dErr == nil {
			for _, d := range all {
				deptCodes[d.ID] = d.Code
			}
		}
	}
	depts := make([]domain.DeptMembershipView, len(row.Departments))
	for i, d := range row.Departments {
		d.Code = deptCodes[d.DepartmentID]
		depts[i] = d
	}

	// Effective feature flags = planDefaults(plan) ⊕ tenants.feature_flags (PLAN-6).
	// Errors from PlanByCode are deliberately swallowed (err == nil
	// gate below), unchanged from this method's pre-cutover behavior:
	// a catalog-admin-config outage degrades I-8 to "no plan
	// baseline" rather than failing the request — this service has
	// no caller relationship with the catalog service (LLD §3), and
	// must not newly acquire one via an unswallowed error here.
	effective := map[string]any{}
	plan, planErr := s.plans.PlanByCode(ctx, row.TenantPlan)
	if planErr == nil && plan != nil {
		for k, v := range plan.FeatureSet {
			effective[k] = v
		}
	}
	for k, v := range row.TenantFeatureFlags {
		effective[k] = v
	}

	// Project the effective map down to the sorted list of enabled flag
	// names (LLD §5.4 I-8 shape). A bool value must be true to count;
	// any other non-nil value is treated as "set" (this codebase's
	// feature_flags values are boolean in practice, but the domain type
	// is map[string]any, so this doesn't assume that).
	enabledFlags := make([]string, 0, len(effective))
	for k, v := range effective {
		switch b, ok := v.(bool); {
		case ok && b:
			enabledFlags = append(enabledFlags, k)
		case !ok && v != nil:
			enabledFlags = append(enabledFlags, k)
		}
	}
	sort.Strings(enabledFlags)

	return &MembershipProjection{
		UserID:               userID,
		TenantID:             tenantID,
		Status:               row.MembershipStatus,
		Plan:                 row.TenantPlan,
		TenantStatus:         row.SubscriptionStatus, // deprecated alias
		SubscriptionStatus:   row.SubscriptionStatus,
		ReadOnly:             readOnlyForStatus(row.SubscriptionStatus),
		Locale:               row.Locale,
		MFAFreshnessSeconds:  row.MFAFreshnessSeconds,
		LocalAccountsEnabled: row.LocalAccountsEnabled,
		Roles:                roles,
		Departments:          depts,
		FeatureFlags:         enabledFlags,
	}, nil
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
