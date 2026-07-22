package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// OperatorService owns O-1..O-7. Every method assumes handler-layer AUTH-6
// gate (platform_operator role). RLS still applies unless the caller
// uses the org_membership_migrator connection — for O-* operations we
// typically want the app pool to see all tenants, which under
// org_membership_app means the operator can only ever mutate global
// catalog rows (plans, departments) OR their own tenant. The specific
// case of O-4 (feature-flags on any tenant) and O-7 (reassign ownership)
// mutate a specific tenant — the handler sets that tenant's id in the
// GUCSet before calling in.
type OperatorService struct {
	pool     *pgcommon.Pool
	plans    port.PlanRepository
	depts    port.DepartmentRepository
	tenants  port.TenantRepository
	tenRoles port.TenantRoleRepository
	memBs    port.MembershipRepository
	cache    port.Cache
}

func NewOperatorService(
	pool *pgcommon.Pool,
	plans port.PlanRepository, depts port.DepartmentRepository,
	tenants port.TenantRepository, roles port.TenantRoleRepository,
	memberships port.MembershipRepository, cache port.Cache,
) *OperatorService {
	return &OperatorService{pool: pool, plans: plans, depts: depts, tenants: tenants, tenRoles: roles, memBs: memberships, cache: cache}
}

// ── O-1..O-3 Departments (global catalog) ───────────────────────────────

func (s *OperatorService) CreateDepartment(ctx context.Context, code, name string, isSystem bool) (*domain.Department, error) {
	if code == "" || name == "" {
		return nil, domain.NewError(domain.ErrValidation, "code and name are required")
	}
	return s.depts.Insert(ctx, &domain.Department{
		Code:     code,
		Name:     name,
		IsSystem: isSystem,
		IsActive: true,
	})
}

func (s *OperatorService) PatchDepartment(ctx context.Context, id uuid.UUID, name *string, isActive *bool, expectedVersion int64) (*domain.Department, error) {
	// D-9/D-11 (system dept retirement) is blocked at the DB level by
	// chk_system_department_active — surfaces as a CHECK violation which
	// bubbles up as a raw error. Map it explicitly here for a clean 422.
	d, err := s.depts.Update(ctx, id, name, isActive, expectedVersion)
	if err != nil {
		if isCheckViolation(err, "chk_system_department_active") {
			return nil, domain.NewError(domain.ErrSystemDepartmentCannotBeRetired, "system department cannot be retired")
		}
		return nil, err
	}
	return d, nil
}

// DeleteDepartmentBlocked always returns 405-equivalent (OP-3).
func (s *OperatorService) DeleteDepartmentBlocked() error {
	return domain.NewError(domain.ErrValidation, "departments cannot be deleted; retire via is_active=false").
		WithDetails(map[string]any{"code": "cannot_delete_system_department"})
}

// ── O-4 Feature flags on a tenant ──────────────────────────────────────

func (s *OperatorService) SetFeatureFlags(ctx context.Context, tenantID uuid.UUID, flags map[string]any) (*domain.Tenant, error) {
	if flags == nil {
		flags = map[string]any{}
	}
	// PLAN-6(d): scalars only (defense — no nested objects/arrays).
	for k, v := range flags {
		switch v.(type) {
		case string, bool, float64, int, int64, nil:
		default:
			return nil, domain.NewError(domain.ErrValidation, "feature flag values must be scalars").
				WithDetails(map[string]any{"code": "invalid_feature_value", "key": k})
		}
	}
	flagsJSON, _ := json.Marshal(flags)

	// Direct SQL — the tenant repository doesn't expose SetFeatureFlags,
	// and operator writes deliberately bypass the app-scoped patch path.
	var updated *domain.Tenant
	err := pgcommon.RunInTx(ctx, s.pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET feature_flags = $2::jsonb WHERE id = $1 AND deleted_at IS NULL`, tenantID, string(flagsJSON))
		return err
	})
	if err != nil {
		return nil, err
	}
	updated, err = s.tenants.FindByID(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if s.cache != nil {
		_ = s.cache.Delete(ctx, cacheKeyTenant(tenantID))
	}
	return updated, nil
}

// ── O-5/O-6 Plans catalog ──────────────────────────────────────────────

func (s *OperatorService) ListPlans(ctx context.Context) ([]domain.Plan, error) {
	return s.plans.List(ctx)
}

func (s *OperatorService) PatchPlan(ctx context.Context, code domain.TenantPlan, patch *domain.PlanPatch) (*domain.Plan, error) {
	p, err := s.plans.Update(ctx, code, patch)
	if err != nil {
		return nil, err
	}
	if s.cache != nil {
		_ = s.cache.Delete(ctx, "om:plans")
	}
	return p, nil
}

// ── O-7 Reassign owner ─────────────────────────────────────────────────

func (s *OperatorService) ReassignOwner(ctx context.Context, tenantID, newOwnerUserID uuid.UUID, actorID uuid.UUID) (*domain.TenantRole, error) {
	t, err := s.tenants.FindByID(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if t.Status == domain.StatusOffboarded {
		return nil, domain.NewError(domain.ErrTenantOffboarded, "tenant is offboarded")
	}
	// New owner must be an active member.
	mem, err := s.memBs.FindByUserID(ctx, tenantID, newOwnerUserID)
	if err != nil || mem.Status != domain.MembershipActive {
		return nil, domain.NewError(domain.ErrInvalidOwnerCandidate, "new owner is not an active member")
	}
	// Grant tenant_owner (idempotent via uq_tenant_roles_active partial unique).
	tr, err := s.tenRoles.Grant(ctx, &domain.TenantRole{
		TenantID: tenantID, UserID: newOwnerUserID,
		TenantMembershipID: mem.ID, RoleCode: domain.RoleTenantOwner, GrantedBy: actorID,
	})
	if err != nil {
		return nil, err
	}
	// Clear ownerless_since via bypass path.
	_ = pgcommon.RunInTx(ctx, s.pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET ownerless_since = NULL WHERE id = $1`, tenantID)
		return err
	})
	if s.cache != nil {
		_ = s.cache.Delete(ctx, cacheKeyTenant(tenantID))
	}
	return tr, nil
}

// isCheckViolation is a best-effort matcher for named CHECK constraints.
// pgconn.PgError.ConstraintName carries the name; we match by substring so
// downstream code can spot D-11 vs generic CHECK.
func isCheckViolation(err error, name string) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if msg == "" {
		return false
	}
	// Cheap contains check — the LLD's DomainError wrap doesn't preserve
	// the raw PgError.ConstraintName field, so the substring match on the
	// error message is the pragmatic choice for Phase 2b.
	return contains(msg, name)
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && indexOf(s, substr) >= 0
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// Suppress unused reference to time (present for future policy checks).
var _ = time.Now
