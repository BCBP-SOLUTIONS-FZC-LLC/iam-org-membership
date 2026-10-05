package service

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
)

// TenantResolutionService owns PUB-1 (LLD §5.4/§8.11, IAM HLD §8.2.7): the
// pre-login, identifier-first lookup that tells an anonymous browser which
// Keycloak realm to authenticate against. Deliberately separate from
// TenantService (RLS-scoped, single-tenant P-1/P-2): this is an anonymous,
// pre-tenant read with a different pool-binding requirement. No cache
// (§6.1), no event, no write.
type TenantResolutionService struct {
	// tenants MUST be constructed against the BYPASSRLS sysPool — see
	// port.TenantRepository.ResolveByIdentifier. Against the RLS-scoped app
	// pool every lookup would silently come back Found=false.
	tenants port.TenantRepository
}

// NewTenantResolutionService builds a TenantResolutionService.
func NewTenantResolutionService(tenants port.TenantRepository) *TenantResolutionService {
	return &TenantResolutionService{tenants: tenants}
}

// Resolve returns the tenant/realm for identifier (a company slug or a
// pending-invitation email). No match is Found=false with a nil error.
// Validation of identifier's shape happens in the HTTP layer.
func (s *TenantResolutionService) Resolve(ctx context.Context, identifier string) (domain.TenantResolution, error) {
	return s.tenants.ResolveByIdentifier(ctx, identifier)
}
