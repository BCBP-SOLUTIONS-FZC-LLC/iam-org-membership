package service

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

// GroupMappingService owns P-14/P-15/P-16/P-17/P-29 and the I-10 JIT SAML
// resolution flow (§8.5/GTRM-4 — additive-only).
type GroupMappingService struct {
	repo        port.GroupMappingRepository
	memberships port.MembershipRepository
	roles       port.TenantRoleRepository
	deptMems    port.DeptMembershipRepository
	txRunner    port.TxRunner
	cache       port.Cache
}

func NewGroupMappingService(
	repo port.GroupMappingRepository,
	memberships port.MembershipRepository,
	roles port.TenantRoleRepository,
	deptMems port.DeptMembershipRepository,
	txRunner port.TxRunner,
	cache port.Cache,
) *GroupMappingService {
	return &GroupMappingService{
		repo: repo, memberships: memberships, roles: roles, deptMems: deptMems,
		txRunner: txRunner, cache: cache,
	}
}

// JITResult is the response envelope for I-10.
type JITResult struct {
	AssignedDepts      []uuid.UUID             `json:"assigned_depts"`
	GrantedTenantRoles []domain.TenantRoleCode `json:"granted_tenant_roles"`
}

// AssignFromGroups is I-10 (§8.5): resolve the caller's Keycloak group
// assertions against the three mapping tables and apply the JIT changes.
// GTRM-4 additive-only: matched tenant_roles that aren't already actively
// held are inserted; a role no longer implied by the current group set is
// NEVER revoked (revocation stays an explicit P-28 action). Dept
// memberships follow §8.4's event-selection rule (Granted / LevelChanged /
// no-event) already implemented in DeptMembershipRepository.Assign.
func (s *GroupMappingService) AssignFromGroups(ctx context.Context, tenantID, userID uuid.UUID, groupNames []string) (*JITResult, error) {
	if len(groupNames) == 0 {
		return &JITResult{}, nil
	}
	// Fetch all mappings for the tenant. These tables are small (Keycloak
	// group counts are per-tenant O(10s)); Go-side filter is simpler than
	// pushing an ANY($1) filter into the repo interface.
	dmMaps, err := s.repo.ListDeptMappings(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	drMaps, err := s.repo.ListDeptRoleMappings(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	trMaps, err := s.repo.ListTenantRoleMappings(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	groupSet := map[string]struct{}{}
	for _, g := range groupNames {
		groupSet[g] = struct{}{}
	}

	// Resolve (dept, level) pairs: intersect group_dept_mappings + group_dept_role_mappings.
	// The two together are the {group→dept} + {group→level} join per group.
	type deptRole struct {
		DepartmentID uuid.UUID
		Level        domain.DeptRole
	}
	var deptRoles []deptRole
	deptDedup := map[string]struct{}{}
	for _, dm := range dmMaps {
		if _, ok := groupSet[dm.KeycloakGroupName]; !ok {
			continue
		}
		for _, dr := range drMaps {
			if _, ok := groupSet[dr.KeycloakGroupName]; !ok {
				continue
			}
			// Both mappings must resolve from the same group name (LLD §8.5
			// "each resolved (dept, role)"): pair only when the same group
			// name provides both dept and role.
			if dm.KeycloakGroupName != dr.KeycloakGroupName {
				continue
			}
			key := dm.DepartmentID.String() + "|" + string(dr.RoleCode)
			if _, seen := deptDedup[key]; seen {
				continue
			}
			deptDedup[key] = struct{}{}
			deptRoles = append(deptRoles, deptRole{DepartmentID: dm.DepartmentID, Level: dr.RoleCode})
		}
	}

	// Resolve tenant roles: matched role_codes.
	tenantRoleDedup := map[domain.TenantRoleCode]struct{}{}
	for _, tr := range trMaps {
		if _, ok := groupSet[tr.KeycloakGroupName]; !ok {
			continue
		}
		if tr.RoleCode == domain.RoleMember {
			continue // TR-7: member is derived, GTRM-6 CHECK enforces at DB
		}
		tenantRoleDedup[tr.RoleCode] = struct{}{}
	}

	// Find the user's membership (required for FK/audit).
	mem, err := s.memberships.FindByUserID(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}

	res := &JITResult{}
	err = s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		pub, _ := port.EventPublisherFromContext(txCtx)

		// Snapshot the user's existing dept memberships so we can decide
		// per-(dept, level) whether to emit Granted (new/reactivated),
		// LevelChanged (active at different level), or no event (unchanged).
		// §8.5 / LLD rev 0.69 — TRG-3 no-op discipline.
		existingDepts, lerr := s.deptMems.ListByUser(txCtx, tenantID, userID)
		if lerr != nil {
			return lerr
		}
		priorByDept := map[uuid.UUID]domain.DeptRole{}
		for _, dm := range existingDepts {
			priorByDept[dm.DepartmentID] = dm.RoleLevel
		}

		for _, dr := range deptRoles {
			assigned, aerr := s.deptMems.Assign(txCtx, tenantID, userID, dr.DepartmentID, mem.ID, dr.Level, uuid.Nil)
			if aerr != nil {
				return aerr
			}
			res.AssignedDepts = append(res.AssignedDepts, assigned.DepartmentID)
			priorLevel, hadPrior := priorByDept[dr.DepartmentID]
			switch {
			case !hadPrior:
				// New (or reactivated after soft-delete) → Granted.
				if pub != nil {
					_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
						Type: domain.EventDepartmentMembershipGranted, TenantID: tenantID,
						Subject: userID.String(), Actor: "iam-system",
						Data: domain.DepartmentMembershipGrantedPayload{
							UserID: userID, TenantID: tenantID,
							DepartmentID: assigned.DepartmentID, Level: assigned.RoleLevel,
							ActorID: uuid.Nil,
						},
					})
				}
			case priorLevel != assigned.RoleLevel:
				// Active membership at a different level → LevelChanged.
				if pub != nil {
					_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
						Type: domain.EventDepartmentMembershipLevelChanged, TenantID: tenantID,
						Subject: userID.String(), Actor: "iam-system",
						Data: domain.DepartmentMembershipLevelChangedPayload{
							UserID: userID, TenantID: tenantID,
							DepartmentID:  assigned.DepartmentID,
							PreviousLevel: priorLevel,
							NewLevel:      assigned.RoleLevel,
							ActorID:       uuid.Nil,
						},
					})
				}
			default:
				// Unchanged — TRG-3 no-op, emit nothing.
			}
		}
		// Tenant roles — additive only (GTRM-4).
		existing, lerr := s.roles.ListByUser(txCtx, tenantID, userID)
		if lerr != nil {
			return lerr
		}
		held := map[domain.TenantRoleCode]struct{}{}
		for _, tr := range existing {
			held[tr.RoleCode] = struct{}{}
		}
		for code := range tenantRoleDedup {
			if _, alreadyHeld := held[code]; alreadyHeld {
				continue
			}
			granted, gerr := s.roles.Grant(txCtx, &domain.TenantRole{
				TenantID: tenantID, UserID: userID,
				TenantMembershipID: mem.ID, RoleCode: code, GrantedBy: uuid.Nil,
			})
			if gerr != nil {
				return gerr
			}
			res.GrantedTenantRoles = append(res.GrantedTenantRoles, granted.RoleCode)
			if pub != nil {
				_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
					Type: domain.EventTenantRoleGranted, TenantID: tenantID,
					Subject: userID.String(), Actor: "iam-system",
					Data: domain.TenantRoleGrantedPayload{
						UserID: userID, TenantID: tenantID, RoleCode: granted.RoleCode, ActorID: uuid.Nil,
					},
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if s.cache != nil {
		_ = s.cache.Delete(ctx, cacheKeyMembers(tenantID, 50), cacheKeySeatUsage(tenantID))
	}
	return res, nil
}

// ── Dept-role mappings (P-14/P-15) ─────────────────────────────────────

func (s *GroupMappingService) ListDeptRole(ctx context.Context, tenantID uuid.UUID) ([]domain.GroupDeptRoleMapping, error) {
	return s.repo.ListDeptRoleMappings(ctx, tenantID)
}

func (s *GroupMappingService) ReplaceDeptRole(ctx context.Context, tenantID uuid.UUID, mappings []domain.GroupDeptRoleMapping) ([]domain.GroupDeptRoleMapping, error) {
	for _, m := range mappings {
		if m.KeycloakGroupName == "" {
			return nil, domain.NewError(domain.ErrValidation, "keycloak_group_name is required")
		}
		if m.RoleCode != domain.DeptPreparator && m.RoleCode != domain.DeptReviewer && m.RoleCode != domain.DeptApprover {
			return nil, domain.NewError(domain.ErrValidation, "invalid role_code for dept-role mapping").
				WithDetails(map[string]any{"code": "invalid_role_level"})
		}
	}
	out, err := s.repo.ReplaceDeptRoleMappings(ctx, tenantID, mappings)
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx, tenantID)
	return out, nil
}

// ── Tenant-role mappings (P-29) ────────────────────────────────────────

func (s *GroupMappingService) ListTenantRole(ctx context.Context, tenantID uuid.UUID) ([]domain.GroupTenantRoleMapping, error) {
	return s.repo.ListTenantRoleMappings(ctx, tenantID)
}

func (s *GroupMappingService) ReplaceTenantRole(ctx context.Context, tenantID uuid.UUID, mappings []domain.GroupTenantRoleMapping) ([]domain.GroupTenantRoleMapping, error) {
	for _, m := range mappings {
		if m.KeycloakGroupName == "" {
			return nil, domain.NewError(domain.ErrValidation, "keycloak_group_name is required")
		}
		if !m.RoleCode.IsElevated() {
			// GTRM-6: chk_gtrm_no_member barred 'member' at DB level too.
			return nil, domain.NewError(domain.ErrValidation, "role_code must be tenant_owner/tenant_admin/tender_admin").
				WithDetails(map[string]any{"code": "invalid_role"})
		}
	}
	out, err := s.repo.ReplaceTenantRoleMappings(ctx, tenantID, mappings)
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx, tenantID)
	return out, nil
}

// ── Dept mappings (P-16/P-17) ──────────────────────────────────────────

func (s *GroupMappingService) ListDept(ctx context.Context, tenantID uuid.UUID) ([]domain.GroupDeptMapping, error) {
	return s.repo.ListDeptMappings(ctx, tenantID)
}

func (s *GroupMappingService) ReplaceDept(ctx context.Context, tenantID uuid.UUID, mappings []domain.GroupDeptMapping) ([]domain.GroupDeptMapping, error) {
	for _, m := range mappings {
		if m.KeycloakGroupName == "" {
			return nil, domain.NewError(domain.ErrValidation, "keycloak_group_name is required")
		}
		if m.DepartmentID == uuid.Nil {
			return nil, domain.NewError(domain.ErrValidation, "department_id is required").
				WithDetails(map[string]any{"code": "invalid_uuid"})
		}
	}
	out, err := s.repo.ReplaceDeptMappings(ctx, tenantID, mappings)
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx, tenantID)
	return out, nil
}

func (s *GroupMappingService) invalidate(ctx context.Context, tenantID uuid.UUID) {
	if s.cache == nil {
		return
	}
	_ = s.cache.Delete(ctx, cacheKeyGRM(tenantID), cacheKeyGDM(tenantID))
}
