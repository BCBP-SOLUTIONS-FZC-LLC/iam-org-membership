package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
)

// PlanRepository owns the global plans catalog (§16 A19). Reads are
// unauthenticated (list of tiers); writes are gated to platform_operator
// via O-6 (AUTH-6).
type PlanRepository interface {
	List(ctx context.Context) ([]domain.Plan, error)
	FindByCode(ctx context.Context, code domain.TenantPlan) (*domain.Plan, error)
	Update(ctx context.Context, code domain.TenantPlan, patch *domain.PlanPatch) (*domain.Plan, error)
}
