package service

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
)

// SubscriptionLapseService owns I-16 (§16 RP-C3 of the RP↔O&M alignment
// review): RP's subscription-lapse sweep has no way to learn a tenant's
// cancelled_at, so it polls this instead of tracking cancellation
// timestamps itself. Deliberately separate from TenantService (which owns
// the RLS-scoped, single-tenant P-1/P-2 flows) — this is a cross-tenant,
// system-level read with a different pool-binding requirement.
type SubscriptionLapseService struct {
	// tenants MUST be constructed against a BYPASSRLS pool (sysPool) — see
	// port.TenantRepository.ListSubscriptionLapses's doc comment. Against
	// the RLS-scoped app pool this would silently narrow to at most one row.
	tenants   port.TenantRepository
	graceDays int
}

// NewSubscriptionLapseService builds a SubscriptionLapseService. graceDays
// is O&M's own SUBSCRIPTION_GRACE_DAYS config (default 30, §15.5) — O&M
// does the grace-period math so the rule stays single-sourced instead of
// drifting between two services' independently-configured values.
func NewSubscriptionLapseService(tenants port.TenantRepository, graceDays int) *SubscriptionLapseService {
	return &SubscriptionLapseService{tenants: tenants, graceDays: graceDays}
}

// List returns every tenant past its subscription-cancellation grace
// period (status='cancelled', cancelled_at older than graceDays). Self-
// idempotent: once RP suspends a tenant (emitting TenantSuspended, which
// flips status to 'suspended'), that tenant naturally drops out of the
// next poll — no ack/cursor needed.
func (s *SubscriptionLapseService) List(ctx context.Context) ([]domain.Tenant, error) {
	return s.tenants.ListSubscriptionLapses(ctx, s.graceDays)
}
