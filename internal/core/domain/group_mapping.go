package domain

import (
	"time"

	"github.com/google/uuid"
)

// GroupDeptRoleMapping is a Keycloak group → dept_role_level mapping
// (§4.2, §16 A25 rename of the former group_role_mappings).
type GroupDeptRoleMapping struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	KeycloakGroupName string
	RoleCode          DeptRole
	RecordVersion     int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// GroupTenantRoleMapping is a Keycloak group → tenant_role mapping
// (§16 A25 new). 'member' barred by chk_gtrm_no_member (GTRM-6).
type GroupTenantRoleMapping struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	KeycloakGroupName string
	RoleCode          TenantRoleCode // elevated only
	RecordVersion     int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// GroupDeptMapping is a Keycloak group → department activation mapping
// (§4.2). One group can activate multiple depts.
type GroupDeptMapping struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	KeycloakGroupName string
	DepartmentID      uuid.UUID
	RecordVersion     int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}
