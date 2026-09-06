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
// System-department retirement is enforced at the SERVICE layer (not DB) —
// the chk_system_department_active constraint was never migrated, so the
// is_system guard in SetActive is the sole enforcement point.
type DepartmentService struct {
	catalog     port.DepartmentCatalogReader
	tenantDepts port.TenantDepartmentRepository
	cache       port.Cache
}

func NewDepartmentService(catalog port.DepartmentCatalogReader, tenantDepts port.TenantDepartmentRepository, cache port.Cache) *DepartmentService {
	return &DepartmentService{catalog: catalog, tenantDepts: tenantDepts, cache: cache}
}

// ListForTenant returns ALL tenant_departments (active and inactive) joined
// with their catalog department. UI filters by is_active; P-3 LLD §5.4.
// BUG FIX: was calling ListActive (only is_active=true) — changed to List
// so deactivated depts still appear; is_active flag is advisory for the UI.
func (s *DepartmentService) ListForTenant(ctx context.Context, tenantID uuid.UUID) ([]TenantDepartmentView, error) {
	tds, err := s.tenantDepts.List(ctx, tenantID)
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
		d, err := s.catalog.DepartmentByID(ctx, td.DepartmentID)
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

// Activate implements P-24. Returns 201 on fresh create, 409 on conflict.
// Returns 422 if the global catalog entry is retired (is_active=false).
func (s *DepartmentService) Activate(ctx context.Context, tenantID, departmentID uuid.UUID) (*domain.TenantDepartment, bool, error) {
	// D-5 / TD-1: catalog entry must exist AND be globally active.
	dept, err := s.catalog.DepartmentByID(ctx, departmentID)
	if err != nil {
		return nil, false, err
	}
	if !dept.IsActive {
		return nil, false, domain.NewError(domain.ErrDepartmentRetired,
			"cannot activate a globally retired department")
	}
	td, err := s.tenantDepts.Activate(ctx, tenantID, departmentID)
	if err != nil {
		return nil, false, err // ErrDepartmentAlreadyActivated propagates → 409
	}
	s.invalidateCache(ctx, tenantID)
	return td, true, nil
}

// SetActive implements P-25. Two pre-flight checks run before the UPDATE:
//   - is_active=false: blocks system depts (is_system=true → 422)
//   - is_active=true: blocks globally retired depts (is_active=false → 422, TD-1/D-5)
func (s *DepartmentService) SetActive(ctx context.Context, tenantID, departmentID uuid.UUID, isActive bool, expectedVersion int64) (*domain.TenantDepartment, error) {
	dept, err := s.catalog.DepartmentByID(ctx, departmentID)
	if err != nil {
		return nil, err
	}
	if !isActive && dept.IsSystem {
		return nil, domain.NewError(domain.ErrSystemDepartmentCannotBeRetired,
			"system departments cannot be deactivated")
	}
	if isActive && !dept.IsActive {
		// TD-1/D-5: re-activation is blocked if the catalog entry is globally retired.
		return nil, domain.NewError(domain.ErrDepartmentRetired,
			"cannot reactivate a globally retired department")
	}
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
