package domain

import (
	"time"

	"github.com/google/uuid"
)

// Department is the global operator catalog row (§4.2, D-4/D-10/D-11).
// Never physically deleted; retired via is_active=false.
type Department struct {
	ID            uuid.UUID
	Code          string // immutable (D-10)
	Name          string
	IsSystem      bool // immutable (D-2)
	IsActive      bool
	RecordVersion int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// TenantDepartment is the per-tenant activation of a global catalog dept
// (§4.2). Composite PK (tenant_id, department_id).
type TenantDepartment struct {
	TenantID      uuid.UUID
	DepartmentID  uuid.UUID
	IsActive      bool
	RecordVersion int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
