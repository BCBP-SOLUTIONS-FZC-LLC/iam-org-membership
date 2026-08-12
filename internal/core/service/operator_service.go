package service

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
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
	txRunner port.TxRunner
}

func NewOperatorService(
	pool *pgcommon.Pool,
	plans port.PlanRepository, depts port.DepartmentRepository,
	tenants port.TenantRepository, roles port.TenantRoleRepository,
	memberships port.MembershipRepository, cache port.Cache,
	txRunner port.TxRunner,
) *OperatorService {
	return &OperatorService{
		pool: pool, plans: plans, depts: depts, tenants: tenants,
		tenRoles: roles, memBs: memberships, cache: cache, txRunner: txRunner,
	}
}

// ── O-1..O-3 Departments (global catalog) ───────────────────────────────

func (s *OperatorService) CreateDepartment(ctx context.Context, code, name string, isSystem bool) (*domain.Department, error) {
	if strings.TrimSpace(code) == "" || strings.TrimSpace(name) == "" {
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
	if name == nil && isActive == nil {
		return nil, domain.NewError(domain.ErrNoMutableField, "at least one of name or is_active must be provided").
			WithDetails(map[string]any{"code": "no_mutable_field"})
	}
	if name != nil && *name == "" {
		return nil, domain.NewError(domain.ErrValidation, "name must not be empty")
	}
	// D-9/D-11 (system dept retirement) is blocked at the DB level by
	// chk_system_department_active — surfaces as a CHECK violation which
	// bubbles up as a raw error. Map it explicitly here for a clean 422.
	d, err := s.depts.Update(ctx, id, name, isActive, expectedVersion)
	if err != nil {
		if isCheckViolation(err, "chk_system_department_active") {
			return nil, domain.NewError(domain.ErrSystemDepartmentCannotBeRetired, "system department cannot be retired")
		}
		if isCheckViolation(err, "system department name is immutable") {
			return nil, domain.NewError(domain.ErrFieldImmutable, "system department name is immutable").
				WithDetails(map[string]any{"code": "field_immutable", "field": "name"})
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

// featureFlagAllowlist is the closed set of tenant-level feature-flag keys
// operators may override via O-4. LLD §16 A18 requires an allow-list so a
// typo like "sso_enable" is rejected up front rather than stored as a dead
// override that looks configured but is never read. Kept in sync with the
// service-layer plan-defaults map (LLD O-4 spec, line 2029).
var featureFlagAllowlist = map[string]struct{}{
	"sso_enabled":           {},
	"custom_branding":       {},
	"require_mfa_all_users": {},
}

func (s *OperatorService) SetFeatureFlags(ctx context.Context, tenantID uuid.UUID, flags map[string]any, expectedVersion int64) (*domain.Tenant, error) {
	if flags == nil {
		flags = map[string]any{}
	}
	// LLD O-4: keys validated against a fixed allow-list (400 unknown_feature_flag)
	// before the scalar-value check so a typo lands on the more specific error.
	for k := range flags {
		if _, ok := featureFlagAllowlist[k]; !ok {
			return nil, domain.NewError(domain.ErrValidation, "unknown feature flag key").
				WithDetails(map[string]any{"code": "unknown_feature_flag", "key": k})
		}
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
	// LLD O-4 optimistic-lock contract (CONC-1): UPDATE ... WHERE id AND
	// record_version = $N — zero rows affected → 409 optimistic_lock_conflict.
	var updated *domain.Tenant
	err := s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		tx, ok := pgadapterTxFromContext(txCtx)
		if !ok {
			return domain.NewError(domain.ErrConflict, "tx unavailable")
		}
		tag, err := tx.Exec(txCtx,
			`UPDATE tenants SET feature_flags = $2::jsonb WHERE id = $1 AND record_version = $3 AND deleted_at IS NULL`,
			tenantID, string(flagsJSON), expectedVersion)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// Distinguish 404 (no such tenant) from 409 (version mismatch).
			var current int64
			probeErr := tx.QueryRow(txCtx,
				`SELECT record_version FROM tenants WHERE id = $1 AND deleted_at IS NULL`,
				tenantID).Scan(&current)
			if probeErr != nil {
				return domain.NewError(domain.ErrTenantNotFound, "tenant not found")
			}
			return domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict").
				WithDetails(map[string]any{"record_version": current})
		}
		return nil
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
	switch code {
	case domain.PlanStarter, domain.PlanPro, domain.PlanEnterprise:
	default:
		return nil, domain.NewError(domain.ErrPlanNotFound, "plan not found")
	}
	if patch.DisplayName == nil &&
		patch.WorkflowTemplateLimit == nil &&
		patch.TenderLimit == nil &&
		patch.TrialDurationDays == nil &&
		patch.SSOEnabled == nil &&
		patch.CustomBranding == nil &&
		patch.FeatureSet == nil {
		return nil, domain.NewError(domain.ErrNoMutableField, "at least one field must be provided").
			WithDetails(map[string]any{"code": "no_mutable_field"})
	}
	// Mirror DB CHECK constraints at the service layer so invalid values
	// surface as 400 validation_error rather than a 500 pgconn CHECK violation.
	if patch.WorkflowTemplateLimit != nil && *patch.WorkflowTemplateLimit != nil && **patch.WorkflowTemplateLimit < 0 {
		return nil, domain.NewError(domain.ErrValidation, "workflow_template_limit must be >= 0")
	}
	if patch.TenderLimit != nil && *patch.TenderLimit != nil && **patch.TenderLimit < 0 {
		return nil, domain.NewError(domain.ErrValidation, "tender_limit must be >= 0")
	}
	if patch.TrialDurationDays != nil && *patch.TrialDurationDays < 0 {
		return nil, domain.NewError(domain.ErrValidation, "trial_duration_days must be >= 0")
	}
	if patch.CustomBranding != nil {
		switch *patch.CustomBranding {
		case domain.BrandingNone, domain.BrandingLogo:
		default:
			return nil, domain.NewError(domain.ErrValidation, "custom_branding must be one of: none, logo")
		}
	}
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
	// Grant tenant_owner, clear ownerless_since, and emit TenantRoleGranted
	// in one tx via the TxRunner (which injects an EventPublisher into ctx
	// so outbox events land atomically per CONS-1). LLD §5.4 O-7 step 5
	// requires the event so AuthZ/Audit/Notification see the reassignment.
	var tr *domain.TenantRole
	err = s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		tx, ok := pgadapterTxFromContext(txCtx)
		if !ok {
			return domain.NewError(domain.ErrConflict, "tx unavailable")
		}
		granted, gerr := s.tenRoles.Grant(txCtx, &domain.TenantRole{
			TenantID: tenantID, UserID: newOwnerUserID,
			TenantMembershipID: mem.ID, RoleCode: domain.RoleTenantOwner, GrantedBy: actorID,
		})
		if gerr != nil {
			return gerr
		}
		tr = granted
		if _, uerr := tx.Exec(txCtx, `UPDATE tenants SET ownerless_since = NULL WHERE id = $1`, tenantID); uerr != nil {
			return uerr
		}
		pub, _ := port.EventPublisherFromContext(txCtx)
		if pub != nil {
			_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
				Type: domain.EventTenantRoleGranted, TenantID: tenantID,
				Subject: newOwnerUserID.String(), Actor: actorID.String(),
				Data: domain.TenantRoleGrantedPayload{
					UserID: newOwnerUserID, TenantID: tenantID,
					RoleCode: domain.RoleTenantOwner, ActorID: actorID,
				},
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Evict the affected user's membership cache so AuthZ Enrichment's next
	// I-8 lookup sees the new owner grant (LLD §5.4 O-7 step 6).
	if s.cache != nil {
		_ = s.cache.Delete(ctx, cacheKeyTenant(tenantID))
		_ = s.cache.Delete(ctx, cacheKeyMemberships(tenantID, newOwnerUserID))
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
