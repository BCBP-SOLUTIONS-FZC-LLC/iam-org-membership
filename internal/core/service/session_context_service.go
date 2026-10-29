package service

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
)

// readOnlyFeatureFlag is the flag AuthZ Enrichment appends to x-feature-flags
// when I-8's read_only is true (iam-authz-enrichment usecase/check.go
// buildHeaders). P-35 appends it the same way so the UI sees the flag set
// downstream services see.
const readOnlyFeatureFlag = "read_only"

// membershipProjector is the slice of AuthZService SessionContextService
// reads: the I-8 projection, om:memberships cache-through.
type membershipProjector interface {
	GetMembership(ctx context.Context, tenantID, userID uuid.UUID) (*MembershipProjection, error)
}

// tenantGetter is the slice of TenantService SessionContextService reads:
// the om:tenant cache-through row I-9 and I-14 serve from.
type tenantGetter interface {
	Get(ctx context.Context, tenantID uuid.UUID) (*domain.Tenant, error)
}

// SessionContext is P-35's view of the caller's own tenant context — the
// values AuthZ Enrichment injects as x-tenant-roles, x-departments, x-plan
// and x-feature-flags, plus the tenant's step-up window and locale.
type SessionContext struct {
	TenantID            uuid.UUID
	Roles               []domain.TenantRoleCode
	Departments         []domain.DeptMembershipView
	Plan                domain.TenantPlan
	FeatureFlags        []string
	ReadOnly            bool
	MFAFreshnessSeconds int
	Locale              string
}

// SessionContextService owns P-35 (GET /api/v1/me/session-context, LLD
// §5.4): the signed-in user's read of their own tenant context. It adds no
// query of its own — it composes two existing cache-through reads:
//
//   - the I-8 projection (AuthZService.GetMembership, om:memberships) for
//     roles, departments, plan, feature_flags and read_only, the exact
//     source AuthZ Enrichment builds its headers from;
//   - the tenant row (TenantService.Get, om:tenant) for
//     mfa_freshness_seconds and default_locale, the source of I-14 (which
//     AuthZ Enrichment's step-up gate acts on, §16 A72) and I-9. I-8's
//     own copies of those two fields are a per-user snapshot that a P-2
//     PATCH does not evict.
type SessionContextService struct {
	authz   membershipProjector
	tenants tenantGetter
}

// NewSessionContextService builds a SessionContextService. authz is the
// AuthZService (I-8) and tenants the TenantService (P-1/I-9/I-14).
func NewSessionContextService(authz membershipProjector, tenants tenantGetter) *SessionContextService {
	return &SessionContextService{authz: authz, tenants: tenants}
}

// Get returns userID's session context in tenantID.
//
//   - No membership row: I-8's own answer, 404 member_not_found. AuthZ
//     Enrichment denies such a caller at the gateway already
//     (DENY_UNRESOLVABLE_SUBJECT on I-8's 404), so this is defense in depth.
//   - A row that is not active (suspended): 403, the same answer
//     RequireActiveMembership gives on every tenant API (AUTH-1). I-8
//     itself returns such a row with its status, for AuthZ Enrichment to
//     act on; a caller-facing read must not hand roles to a suspended
//     member.
func (s *SessionContextService) Get(ctx context.Context, tenantID, userID uuid.UUID) (*SessionContext, error) {
	proj, err := s.authz.GetMembership(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	if proj.Status != domain.MembershipActive {
		msg := "member is not an active member of this tenant"
		if proj.Status == domain.MembershipSuspended {
			msg = "suspended members cannot access tenant APIs"
		}
		return nil, domain.NewError(domain.ErrInsufficientRole, msg)
	}
	t, err := s.tenants.Get(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return &SessionContext{
		TenantID:            tenantID,
		Roles:               nonNilRoles(proj.Roles),
		Departments:         nonNilDepts(proj.Departments),
		Plan:                proj.Plan,
		FeatureFlags:        sessionFeatureFlags(proj.FeatureFlags, proj.ReadOnly),
		ReadOnly:            proj.ReadOnly,
		MFAFreshnessSeconds: t.MFAFreshnessSeconds,
		Locale:              t.DefaultLocale,
	}, nil
}

// sessionFeatureFlags mirrors AuthZ Enrichment's x-feature-flags: I-8's
// enabled-flag list, plus "read_only" when the tenant is read-only. Unlike
// the header it never repeats a flag already in the list. Always a new
// slice, so the cached projection is never aliased.
func sessionFeatureFlags(flags []string, readOnly bool) []string {
	out := make([]string, 0, len(flags)+1)
	out = append(out, flags...)
	if !readOnly {
		return out
	}
	for _, f := range flags {
		if f == readOnlyFeatureFlag {
			return out
		}
	}
	return append(out, readOnlyFeatureFlag)
}

func nonNilRoles(r []domain.TenantRoleCode) []domain.TenantRoleCode {
	if r == nil {
		return []domain.TenantRoleCode{}
	}
	return r
}

func nonNilDepts(d []domain.DeptMembershipView) []domain.DeptMembershipView {
	if d == nil {
		return []domain.DeptMembershipView{}
	}
	return d
}
