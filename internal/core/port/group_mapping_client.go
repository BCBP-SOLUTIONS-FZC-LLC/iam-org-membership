package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
)

// GroupMappingClient is the outbound HTTP client for the Group Mapping /
// JIT Config Service (group-mapping-jit-config), which owns the three
// Keycloak-group mapping tables as of ADR-0007 Wave 2. A single
// consolidated, mesh-only, mTLS-authenticated call (GM-I1) replaces the
// three local SELECTs I-10 used to issue against
// group_dept_role_mappings/group_tenant_role_mappings/group_dept_mappings.
//
// Like CatalogAdminClient, an unconfigured base URL or transport failure
// is NOT swallowed here — this client returns a plain error and lets the
// caller (service.GroupMappingService) decide resilience: cache, then
// stale-if-error cache, then explicit fail-open (ADR-0007 Action Item 4 —
// a SAML login must never fail because Group Mapping Service is down).
type GroupMappingClient interface {
	// ResolveGroups calls POST /internal/tenants/{tenant_id}/group-resolution
	// (GM-I1) with the caller's asserted Keycloak group names and returns
	// the resolved dept/dept-role/tenant-role mappings for tenantID.
	ResolveGroups(ctx context.Context, tenantID uuid.UUID, groups []string) (*GroupResolution, error)
}

// GroupResolution is GM-I1's response envelope. Each dimension is an
// empty (never nil) slice when nothing matches.
type GroupResolution struct {
	DeptMappings       []ResolvedDeptMapping
	DeptRoleMappings   []ResolvedDeptRoleMapping
	TenantRoleMappings []ResolvedTenantRoleMapping
}

// ResolvedDeptMapping mirrors GM-I1's dept_mappings[] entries.
type ResolvedDeptMapping struct {
	KeycloakGroupName string
	DepartmentID      uuid.UUID
}

// ResolvedDeptRoleMapping mirrors GM-I1's dept_role_mappings[] entries.
type ResolvedDeptRoleMapping struct {
	KeycloakGroupName string
	RoleCode          domain.DeptRole
}

// ResolvedTenantRoleMapping mirrors GM-I1's tenant_role_mappings[] entries.
type ResolvedTenantRoleMapping struct {
	KeycloakGroupName string
	RoleCode          domain.TenantRoleCode
}
