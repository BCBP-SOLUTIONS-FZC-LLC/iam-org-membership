package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

// GroupMappingService owns the I-10 JIT SAML resolution flow (§8.5/GTRM-4
// — additive-only). P-14/P-15/P-16/P-17/P-29 (the group-mapping admin
// CRUD endpoints) and their local tables have been fully cut over to
// Group Mapping Service (ADR-0007 Wave 2, group-mapping-jit-config-
// service-lld.md); this service is pre-production, so the cutover was
// done in one pass rather than the staged 410-Gone/soak sequence a live
// service would need. I-10's resolution step goes through
// om:grm/om:gdm/om:gtrm (+ 24h :stale fallbacks) backed by
// groupMappingClient's single consolidated GM-I1 call — see
// resolveMappings.
type GroupMappingService struct {
	memberships        port.MembershipRepository
	roles              port.TenantRoleRepository
	deptMems           port.DeptMembershipRepository
	txRunner           port.TxRunner
	cache              port.Cache
	groupMappingClient port.GroupMappingClient
}

func NewGroupMappingService(
	memberships port.MembershipRepository,
	roles port.TenantRoleRepository,
	deptMems port.DeptMembershipRepository,
	txRunner port.TxRunner,
	cache port.Cache,
	groupMappingClient port.GroupMappingClient,
) *GroupMappingService {
	return &GroupMappingService{
		memberships: memberships, roles: roles, deptMems: deptMems,
		txRunner: txRunner, cache: cache, groupMappingClient: groupMappingClient,
	}
}

const (
	groupResolutionCacheTTL      = 600 * time.Second
	groupResolutionStaleCacheTTL = 24 * time.Hour
)

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
	// Resolved via Group Mapping Service (GM-I1) behind the om:grm/gdm/gtrm
	// cache, not local SELECTs — see resolveMappings. These tables are
	// small (Keycloak group counts are per-tenant O(10s)); Go-side filter
	// is simpler than pushing an ANY($1) filter into the resolution call.
	dmMaps, drMaps, trMaps := s.resolveMappings(ctx, tenantID, groupNames)
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

		// Per-(dept, level) event classification (Granted / LevelChanged / no
		// event, TRG-3 no-op discipline, §8.5 / LLD rev 0.69) uses the
		// atomically-accurate `previous` Assign() itself returns — not a
		// separate pre-fetch, which would race the same way B15 did.
		for _, dr := range deptRoles {
			assigned, previous, aerr := s.deptMems.Assign(txCtx, tenantID, userID, dr.DepartmentID, mem.ID, dr.Level, uuid.Nil)
			if aerr != nil {
				return aerr
			}
			res.AssignedDepts = append(res.AssignedDepts, assigned.DepartmentID)
			switch {
			case previous == nil:
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
			case previous.RoleLevel != assigned.RoleLevel:
				// Active membership at a different level → LevelChanged.
				if pub != nil {
					_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
						Type: domain.EventDepartmentMembershipLevelChanged, TenantID: tenantID,
						Subject: userID.String(), Actor: "iam-system",
						Data: domain.DepartmentMembershipLevelChangedPayload{
							UserID: userID, TenantID: tenantID,
							DepartmentID:  assigned.DepartmentID,
							PreviousLevel: previous.RoleLevel,
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

// resolveMappings is I-10's mapping-resolution step (Document 3 §18 Stage
// 2): cache-hit path is unchanged behavior, just now backed by
// om:grm/om:gdm/om:gtrm populated from Group Mapping Service instead of a
// local join. Cache-miss calls groupMappingClient.ResolveGroups (GM-I1)
// and populates all three primary keys (600s TTL) + their :stale
// counterparts (24h TTL) on success.
//
// On a client-call failure, falls back to the :stale keys if present. If
// neither a fresh cache nor a stale-if-error snapshot is available (a
// genuinely cold tenant, or Group Mapping Service being down), this fails
// OPEN — ADR-0007 Action Item 4: a SAML login must never fail because
// this call failed, so an empty resolution is returned rather than an
// error. The caller proceeds with whatever it already resolved (nothing,
// in the fully-cold case) and the login still completes with a 200.
func (s *GroupMappingService) resolveMappings(ctx context.Context, tenantID uuid.UUID, groupNames []string) ([]domain.GroupDeptMapping, []domain.GroupDeptRoleMapping, []domain.GroupTenantRoleMapping) {
	if dm, dr, tr, ok := s.getCachedResolution(ctx, tenantID, false); ok {
		return dm, dr, tr
	}
	if s.groupMappingClient == nil {
		slog.WarnContext(ctx, "groupmapping: no GroupMappingClient configured — failing open with empty resolution",
			"tenant_id", tenantID)
		return nil, nil, nil
	}
	res, err := s.groupMappingClient.ResolveGroups(ctx, tenantID, groupNames)
	if err != nil {
		if dm, dr, tr, ok := s.getCachedResolution(ctx, tenantID, true); ok {
			slog.WarnContext(ctx, "groupmapping: ResolveGroups live call failed — serving stale-if-error fallback",
				"tenant_id", tenantID, "error", err.Error())
			return dm, dr, tr
		}
		slog.WarnContext(ctx, "groupmapping: ResolveGroups failed with no cached fallback — failing open with empty resolution",
			"tenant_id", tenantID, "error", err.Error())
		return nil, nil, nil
	}

	dm := make([]domain.GroupDeptMapping, len(res.DeptMappings))
	for i, d := range res.DeptMappings {
		dm[i] = domain.GroupDeptMapping{TenantID: tenantID, KeycloakGroupName: d.KeycloakGroupName, DepartmentID: d.DepartmentID}
	}
	dr := make([]domain.GroupDeptRoleMapping, len(res.DeptRoleMappings))
	for i, d := range res.DeptRoleMappings {
		dr[i] = domain.GroupDeptRoleMapping{TenantID: tenantID, KeycloakGroupName: d.KeycloakGroupName, RoleCode: d.RoleCode}
	}
	tr := make([]domain.GroupTenantRoleMapping, len(res.TenantRoleMappings))
	for i, t := range res.TenantRoleMappings {
		tr[i] = domain.GroupTenantRoleMapping{TenantID: tenantID, KeycloakGroupName: t.KeycloakGroupName, RoleCode: t.RoleCode}
	}
	s.setCachedResolution(ctx, tenantID, dm, dr, tr)
	return dm, dr, tr
}

// getCachedResolution reads all three om:grm/gdm/gtrm keys (or their
// :stale counterparts) via one MGet round trip. ok is true only when all
// three are present and valid JSON — a partial hit is treated as a full
// miss so a tenant is never served a mix of one dimension's stale data
// against another's fresher data (GM-I1 always refreshes all three
// together, so in practice they always expire in lockstep).
func (s *GroupMappingService) getCachedResolution(ctx context.Context, tenantID uuid.UUID, stale bool) ([]domain.GroupDeptMapping, []domain.GroupDeptRoleMapping, []domain.GroupTenantRoleMapping, bool) {
	if s.cache == nil {
		return nil, nil, nil, false
	}
	gdmKey, grmKey, gtrmKey := cacheKeyGDM(tenantID), cacheKeyGRM(tenantID), cacheKeyGTRM(tenantID)
	if stale {
		gdmKey, grmKey, gtrmKey = cacheKeyGDMStale(tenantID), cacheKeyGRMStale(tenantID), cacheKeyGTRMStale(tenantID)
	}
	raw, err := s.cache.MGet(ctx, []string{gdmKey, grmKey, gtrmKey})
	if err != nil || len(raw) != 3 || raw[0] == nil || raw[1] == nil || raw[2] == nil {
		return nil, nil, nil, false
	}
	var dm []domain.GroupDeptMapping
	var dr []domain.GroupDeptRoleMapping
	var tr []domain.GroupTenantRoleMapping
	if json.Unmarshal(raw[0], &dm) != nil || json.Unmarshal(raw[1], &dr) != nil || json.Unmarshal(raw[2], &tr) != nil {
		return nil, nil, nil, false
	}
	return dm, dr, tr, true
}

func (s *GroupMappingService) setCachedResolution(ctx context.Context, tenantID uuid.UUID, dm []domain.GroupDeptMapping, dr []domain.GroupDeptRoleMapping, tr []domain.GroupTenantRoleMapping) {
	if s.cache == nil {
		return
	}
	dmRaw, err1 := json.Marshal(dm)
	drRaw, err2 := json.Marshal(dr)
	trRaw, err3 := json.Marshal(tr)
	if err1 != nil || err2 != nil || err3 != nil {
		return
	}
	_ = s.cache.Set(ctx, cacheKeyGDM(tenantID), dmRaw, groupResolutionCacheTTL)
	_ = s.cache.Set(ctx, cacheKeyGRM(tenantID), drRaw, groupResolutionCacheTTL)
	_ = s.cache.Set(ctx, cacheKeyGTRM(tenantID), trRaw, groupResolutionCacheTTL)
	_ = s.cache.Set(ctx, cacheKeyGDMStale(tenantID), dmRaw, groupResolutionStaleCacheTTL)
	_ = s.cache.Set(ctx, cacheKeyGRMStale(tenantID), drRaw, groupResolutionStaleCacheTTL)
	_ = s.cache.Set(ctx, cacheKeyGTRMStale(tenantID), trRaw, groupResolutionStaleCacheTTL)
}
