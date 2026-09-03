package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
)

// MembershipProjectionRow is the raw I-8 hot-path join result — everything
// AuthZService.readFromDB used to assemble by hand against *pgcommon.Pool
// directly. Deliberately NOT the full MembershipProjection: cross-service
// composition (TR-7's derived "member" role, department-code enrichment
// via port.DepartmentCatalogReader, and PLAN-6's effective-feature-flags
// merge via port.PlanCatalogReader) is business logic that belongs in
// AuthZService, not in a Postgres adapter — this type carries only what a
// single joined SQL query can produce on its own.
//
// Roles is the raw elevated-grant set from tenant_roles — it does NOT
// include the derived "member" role (TR-7 injects that at the service
// layer). Departments' Code field is left zero-valued — the service fills
// it in from the department catalog. TenantFeatureFlags is the tenant's
// raw override delta (already unmarshalled from jsonb), not yet merged
// with the plan's baseline.
type MembershipProjectionRow struct {
	MembershipStatus     domain.MembershipStatus
	TenantPlan           domain.TenantPlan
	SubscriptionStatus   domain.SubscriptionStatus
	Locale               string
	MFAFreshnessSeconds  int
	LocalAccountsEnabled bool
	TenantFeatureFlags   map[string]any
	Roles                []domain.TenantRoleCode
	Departments          []domain.DeptMembershipView
}

// AuthZRepository backs I-8 (§8.3, the AuthZ Enrichment hot path — SLO
// 15 ms cache hit / 30 ms miss). FindMembershipProjection runs the
// tenant_memberships ⋈ tenants ⋈ tenant_roles ⋈ dept_memberships join in
// ONE round trip — the SLO does not tolerate splitting this across four
// separate repository calls, so it stays a single purpose-built query
// rather than reusing TenantRepository/MembershipRepository/etc.
type AuthZRepository interface {
	// FindMembershipProjection returns (nil, nil) — not an error — when
	// the caller has no tenant_memberships row (active or suspended) for
	// (tenantID, userID). AuthZService translates that nil into
	// domain.ErrMemberNotFound (404); the repository layer does not know
	// about that HTTP-facing distinction.
	FindMembershipProjection(ctx context.Context, tenantID, userID uuid.UUID) (*MembershipProjectionRow, error)
}
