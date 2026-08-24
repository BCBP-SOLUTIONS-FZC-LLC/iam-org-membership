package domain

import (
	"github.com/google/uuid"
)

// GroupDeptRoleMapping is a Keycloak group → dept_role_level mapping.
// Owned by the Group Mapping Service; this is the read-through cache's
// resolution-result type (om:grm), populated from GroupMappingClient.Resolve
// (ADR-0007 §5, §6.2) — not a locally-owned table.
type GroupDeptRoleMapping struct {
	TenantID          uuid.UUID
	KeycloakGroupName string
	RoleCode          DeptRole
}

// GroupTenantRoleMapping is a Keycloak group → tenant_role mapping
// ('member' excluded — elevated roles only). Owned by the Group Mapping
// Service; this is the read-through cache's resolution-result type
// (om:gtrm), populated from GroupMappingClient.Resolve.
type GroupTenantRoleMapping struct {
	TenantID          uuid.UUID
	KeycloakGroupName string
	RoleCode          TenantRoleCode // elevated only
}

// GroupDeptMapping is a Keycloak group → department activation mapping.
// One group can activate multiple depts. Owned by the Group Mapping
// Service; this is the read-through cache's resolution-result type
// (om:gdm), populated from GroupMappingClient.Resolve.
type GroupDeptMapping struct {
	TenantID          uuid.UUID
	KeycloakGroupName string
	DepartmentID      uuid.UUID
}
