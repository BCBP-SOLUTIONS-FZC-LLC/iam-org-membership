package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

// DepartmentService owns P-3 (list per-tenant active depts), P-24 (activate
// a catalog dept for the tenant), and P-25 (toggle is_active).
//
// AUTH-2 (tenant_admin / tenant_owner) is checked at the handler layer.
// System-department retirement (D-9/D-11) is enforced at the DB level by
// chk_system_department_active — a P-25 attempt to deactivate a system
// dept will surface as a check violation.
type DepartmentService struct {
	catalog     port.DepartmentRepository
	tenantDepts port.TenantDepartmentRepository
	cache       port.Cache
}

func NewDepartmentService(catalog port.DepartmentRepository, tenantDepts port.TenantDepartmentRepository, cache port.Cache) *DepartmentService {
	return &DepartmentService{catalog: catalog, tenantDepts: tenantDepts, cache: cache}
}

// ListForTenant returns active tenant_departments joined with their catalog
// department. Result is small (5 rows in a fresh tenant) so no cache TTL
// jitter; §6.1 om:tenant scoped cache is 600 s.
func (s *DepartmentService) ListForTenant(ctx context.Context, tenantID uuid.UUID) ([]TenantDepartmentView, error) {
	tds, err := s.tenantDepts.ListActive(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if len(tds) == 0 {
		return []TenantDepartmentView{}, nil
	}
	// Hydrate department names by looking up each id. In Phase 3 this can
	// become a single JOIN or a batched query; Phase 2's per-tenant dept
	// count is small (5) so N+1 is fine.
	out := make([]TenantDepartmentView, 0, len(tds))
	for _, td := range tds {
		d, err := s.catalog.FindByID(ctx, td.DepartmentID)
		if err != nil {
			return nil, err
		}
		out = append(out, TenantDepartmentView{
			DepartmentID:  td.DepartmentID,
			Code:          d.Code,
			Name:          d.Name,
			IsSystem:      d.IsSystem,
			IsActive:      td.IsActive,
			RecordVersion: td.RecordVersion,
		})
	}
	return out, nil
}

// Activate implements P-24. Idempotent: if already active, returns the
// existing row.
func (s *DepartmentService) Activate(ctx context.Context, tenantID, departmentID uuid.UUID) (*domain.TenantDepartment, error) {
	// Ensure the department exists in the global catalog first (otherwise the
	// FK error message is opaque).
	if _, err := s.catalog.FindByID(ctx, departmentID); err != nil {
		return nil, err
	}
	td, err := s.tenantDepts.Activate(ctx, tenantID, departmentID)
	if err != nil {
		return nil, err
	}
	s.invalidateCache(ctx, tenantID)
	return td, nil
}

// SetActive implements P-25. `false` for a system dept is blocked by
// chk_system_department_active at the DB level.
func (s *DepartmentService) SetActive(ctx context.Context, tenantID, departmentID uuid.UUID, isActive bool, expectedVersion int64) (*domain.TenantDepartment, error) {
	td, err := s.tenantDepts.SetActive(ctx, tenantID, departmentID, isActive, expectedVersion)
	if err != nil {
		return nil, err
	}
	s.invalidateCache(ctx, tenantID)
	return td, nil
}

// TenantDepartmentView is the projection returned by P-3.
type TenantDepartmentView struct {
	DepartmentID  uuid.UUID
	Code          string
	Name          string
	IsSystem      bool
	IsActive      bool
	RecordVersion int64
}

// ── cache ─────────────────────────────────────────────────────────────

// deptListCacheTTL — mirrors CACHE-5 short TTL for anything membership-adjacent.
const deptListCacheTTL = 600 * time.Second

func (s *DepartmentService) invalidateCache(ctx context.Context, tenantID uuid.UUID) {
	if s.cache == nil {
		return
	}
	// Departments read is folded into the om:tenant projection; evicting
	// om:tenant is sufficient for P-1 reads. Explicit key exists for
	// future dept-only projections.
	_ = s.cache.Delete(ctx, cacheKeyTenant(tenantID))
}

// suppress-unused: helpers earmarked for Phase 3+ list caching.
var (
	_ = deptListCacheTTL
	_ = json.Marshal
)
