package service

import (
	"context"
	"errors"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
)

// DeptMembershipService owns P-9/P-10/P-11.
type DeptMembershipService struct {
	deptMemberships port.DeptMembershipRepository
	memberships     port.MembershipRepository
	tenantDepts     port.TenantDepartmentRepository
	catalog         port.DepartmentCatalogReader // global catalog — D-5/TD-6 retired check
	delegationCheck port.DelegationCheckClient   // ADR-0008 v2 §6.4: Core→Delegation DLG-I3 call, replaces the local FindActiveDeptDelegateForUser lookup
	workflow        port.WorkflowClient
	cache           port.Cache
	txRunner        port.TxRunner
	log             port.SlogStyleLogger // optional — see WithLogger
}

func NewDeptMembershipService(
	dm port.DeptMembershipRepository,
	m port.MembershipRepository,
	td port.TenantDepartmentRepository,
	catalog port.DepartmentCatalogReader,
	delegationCheck port.DelegationCheckClient,
	wf port.WorkflowClient,
	cache port.Cache,
	txRunner port.TxRunner,
) *DeptMembershipService {
	return &DeptMembershipService{
		deptMemberships: dm, memberships: m, tenantDepts: td, catalog: catalog, delegationCheck: delegationCheck, workflow: wf, cache: cache, txRunner: txRunner,
	}
}

// WithLogger injects the shared gincommon-backed Logger so this service's
// degraded-path warnings flow through the same sink as HTTP/consumer/
// outbound-client logs instead of slog.Default(). Optional — the zero value
// falls back to the top-level slog functions.
func (s *DeptMembershipService) WithLogger(log port.Logger) *DeptMembershipService {
	s.log = port.NewSlogStyleLogger(log)
	return s
}

// deptDelegateOrDegrade calls the Delegation Service's DLG-I3 dept-delegate
// lookup and, on failure, degrades to nil (tenant-wide impact) rather than
// failing the whole Assign/Remove — LLD §11.5: "on a Delegation outage the
// gate degrades to tenant-wide impact (still correct, less precise)".
func (s *DeptMembershipService) deptDelegateOrDegrade(ctx context.Context, tenantID, userID, deptID uuid.UUID) *uuid.UUID {
	if s.delegationCheck == nil {
		return nil
	}
	id, err := s.delegationCheck.DeptDelegate(ctx, tenantID, userID, deptID)
	if err != nil {
		s.log.WarnContext(ctx, "deptmembership: DeptDelegate call failed — degrading to tenant-wide impact",
			"tenant_id", tenantID, "user_id", userID, "dept_id", deptID, "error", err.Error())
		return nil
	}
	return id
}

// ListByDepartment is P-9 — dept members by level.
// Verifies the department is activated for this tenant before listing (P9-VAL-04).
func (s *DeptMembershipService) ListByDepartment(ctx context.Context, tenantID, deptID uuid.UUID) ([]domain.DeptMembership, error) {
	if _, err := s.tenantDepts.Find(ctx, tenantID, deptID); err != nil {
		return nil, domain.NewError(domain.ErrDepartmentNotFound, "department not found or not activated for this tenant")
	}
	return s.deptMemberships.ListByDepartment(ctx, tenantID, deptID)
}

// Assign is P-10 — assign a user to a dept at a level. Idempotent when
// level unchanged. Dept must be activated for the tenant (fk_dm_tenant_dept
// enforces this at DB level).
func (s *DeptMembershipService) Assign(ctx context.Context, tenantID, userID, deptID uuid.UUID, level domain.DeptRole, actorID uuid.UUID) (*domain.DeptMembership, error) {
	if level != domain.DeptPreparator && level != domain.DeptReviewer && level != domain.DeptApprover {
		return nil, domain.NewError(domain.ErrInvalidRole, "invalid role_level").
			WithDetails(map[string]any{"code": "invalid_role_level"})
	}
	// D-5/TD-6 step 1: global catalog must be active (department_retired).
	if s.catalog != nil {
		dept, err := s.catalog.DepartmentByID(ctx, deptID)
		if err != nil {
			return nil, err
		}
		if !dept.IsActive {
			return nil, domain.NewError(domain.ErrDepartmentRetired,
				"cannot assign to a globally retired department")
		}
	}
	// D-5/TD-6 step 2: tenant must have activated the department (department_deactivated).
	// ErrDepartmentNotFound (no row in tenant_departments) = never activated → 422.
	td, err := s.tenantDepts.Find(ctx, tenantID, deptID)
	if err != nil {
		if errors.Is(err, domain.ErrDepartmentNotFound) {
			return nil, domain.NewError(domain.ErrDepartmentDeactivated,
				"department is not activated for this tenant")
		}
		return nil, err
	}
	if !td.IsActive {
		return nil, domain.NewError(domain.ErrDepartmentDeactivated,
			"department is deactivated for this tenant")
	}
	m, err := s.memberships.FindByUserID(ctx, tenantID, userID)
	if err != nil {
		if errors.Is(err, domain.ErrMemberNotFound) {
			// DM-2: user is not a tenant member at all → same 422 as inactive members
			return nil, domain.NewError(domain.ErrMemberNotActive, "grantee must be an active tenant member")
		}
		return nil, err
	}
	// DM-2 / BUG-P10-1: only an active tenant membership may receive new
	// dept assignments. Suspended members are rejected with 422 member_not_active.
	if m.Status != domain.MembershipActive {
		return nil, domain.NewError(domain.ErrMemberNotActive, "grantee must be an active tenant member")
	}
	var dm *domain.DeptMembership
	err = s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		// Best-effort snapshot for the WFI-9/12 delegate-impact gate below —
		// this check fails open on a race (WFI-9) so a stale read here is
		// acceptable. It must NOT be used to classify the Granted/LevelChanged/
		// no-op event below: that decision needs the atomically-accurate
		// `previous` value Assign() itself returns (see its doc comment / B15).
		var preflightPrevious *domain.DeptMembership
		snapshot, _ := s.deptMemberships.ListByUser(txCtx, tenantID, userID)
		for i := range snapshot {
			if snapshot[i].DepartmentID == deptID {
				preflightPrevious = &snapshot[i]
				break
			}
		}

		// GAP-P10-3 fix: WFI-9/WFI-12 — if the new level is lower than the
		// current level (a privilege decrease), check whether this user is an
		// active delegate on any dept-scoped delegations for this dept.
		// WFI-10: scope='all' delegations are excluded (handled by GetDelegateImpact
		// taking an optional delegation_id; we pass the specific dept delegation).
		// WFI-12: promotions (higher or equal level) never trigger.
		if preflightPrevious != nil && level.Rank() < preflightPrevious.RoleLevel.Rank() && s.workflow != nil {
			delegationID := s.deptDelegateOrDegrade(txCtx, tenantID, userID, deptID)
			if delegationID != nil {
				impact, err := s.workflow.GetDelegateImpact(txCtx, tenantID, userID, delegationID)
				if err != nil {
					// WFI-9 fail-open: workflow unavailable → allow level decrease
					_ = err
				} else if impact.ActiveWorkflows > 0 {
					return domain.NewError(domain.ErrWorkflowResolutionRequired,
						"active workflows depend on this delegate at the current level").
						WithDetails(map[string]any{
							"active_workflows": impact.ActiveWorkflows,
							"workflow_ids":     impact.WorkflowIDs,
							"allowed_actions":  []string{"replace_delegate", "stop_workflows"},
						})
				}
			}
		}

		out, previous, err := s.deptMemberships.Assign(txCtx, tenantID, userID, deptID, m.ID, level, actorID)
		if err != nil {
			return err
		}
		dm = out
		pub, _ := port.EventPublisherFromContext(txCtx)
		if pub == nil {
			return nil
		}
		var rcIP, rcUA string
		if rc, ok := requestctx.FromContext(txCtx); ok {
			rcIP = rc.ClientIP
			rcUA = rc.UserAgent
		}
		if previous == nil {
			// Fresh grant.
			return pub.Enqueue(txCtx, &domain.DomainEvent{
				Type: domain.EventDepartmentMembershipGranted, TenantID: tenantID,
				Subject: userID.String(), Actor: actorID.String(),
				IPAddress: rcIP, UserAgent: rcUA,
				Data: domain.DepartmentMembershipGrantedPayload{
					UserID: userID, TenantID: tenantID, DepartmentID: deptID, Level: level, ActorID: actorID,
				},
			})
		}
		if previous.RoleLevel != level {
			// Level change.
			return pub.Enqueue(txCtx, &domain.DomainEvent{
				Type: domain.EventDepartmentMembershipLevelChanged, TenantID: tenantID,
				Subject: userID.String(), Actor: actorID.String(),
				IPAddress: rcIP, UserAgent: rcUA,
				Data: domain.DepartmentMembershipLevelChangedPayload{
					UserID: userID, TenantID: tenantID, DepartmentID: deptID,
					PreviousLevel: previous.RoleLevel, NewLevel: level, ActorID: actorID,
				},
			})
		}
		// No-op — same level; skip emission (matches TRG-3 spirit).
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx, tenantID)
	return dm, nil
}

// Remove is P-11 — remove a user from a dept.
// §8.8.4 / WFI-11 (LLD rev 1.56): if the user is currently the delegate on an
// active delegation scoped to this department, we must pre-filter to the
// exact delegation row and pass its id to WorkflowClient.GetDelegateImpact
// so the workflow-side query is dept-scoped rather than tenant-wide.
// A `nil` delegation_id preserves today's tenant-wide behavior (§8.8 full
// removal), which is not what we want here.
func (s *DeptMembershipService) Remove(ctx context.Context, tenantID, userID, deptID uuid.UUID, actorID uuid.UUID) (*domain.DeptMembership, error) {
	delegationID := s.deptDelegateOrDegrade(ctx, tenantID, userID, deptID)
	impact, err := s.workflow.GetDelegateImpact(ctx, tenantID, userID, delegationID)
	if err == nil && impact.ActiveWorkflows > 0 {
		return nil, domain.NewError(domain.ErrWorkflowResolutionRequired, "active workflows depend on this delegate").
			WithDetails(map[string]any{
				"active_workflows": impact.ActiveWorkflows,
				"workflow_ids":     impact.WorkflowIDs,
				"allowed_actions":  []string{"replace_delegate", "stop_workflows"},
			})
	}
	var dm *domain.DeptMembership
	err = s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		out, rerr := s.deptMemberships.Remove(txCtx, tenantID, userID, deptID)
		if rerr != nil {
			return rerr
		}
		dm = out
		pub, _ := port.EventPublisherFromContext(txCtx)
		if pub == nil {
			return nil
		}
		evt := &domain.DomainEvent{
			Type: domain.EventDepartmentMembershipRevoked, TenantID: tenantID,
			Subject: userID.String(), Actor: actorID.String(),
			Data: domain.DepartmentMembershipRevokedPayload{
				UserID: userID, TenantID: tenantID, DepartmentID: deptID, ActorID: actorID,
			},
		}
		if rc, ok := requestctx.FromContext(txCtx); ok {
			evt.IPAddress = rc.ClientIP
			evt.UserAgent = rc.UserAgent
		}
		return pub.Enqueue(txCtx, evt)
	})
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx, tenantID)
	if s.cache != nil {
		_ = s.cache.Delete(ctx, cacheKeyDeptMembers(tenantID, deptID))
	}
	return dm, nil
}

func (s *DeptMembershipService) invalidate(ctx context.Context, tenantID uuid.UUID) {
	if s.cache == nil {
		return
	}
	_ = s.cache.Delete(ctx,
		cacheKeyMembers(tenantID, 50),
		cacheKeySeatUsage(tenantID),
	)
}
