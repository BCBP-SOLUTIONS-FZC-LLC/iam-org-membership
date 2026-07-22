package service

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

// DeptMembershipService owns P-9/P-10/P-11.
type DeptMembershipService struct {
	deptMemberships port.DeptMembershipRepository
	memberships     port.MembershipRepository
	tenantDepts     port.TenantDepartmentRepository
	workflow        port.WorkflowClient
	cache           port.Cache
	txRunner        port.TxRunner
}

func NewDeptMembershipService(
	dm port.DeptMembershipRepository,
	m port.MembershipRepository,
	td port.TenantDepartmentRepository,
	wf port.WorkflowClient,
	cache port.Cache,
	txRunner port.TxRunner,
) *DeptMembershipService {
	return &DeptMembershipService{
		deptMemberships: dm, memberships: m, tenantDepts: td, workflow: wf, cache: cache, txRunner: txRunner,
	}
}

// ListByDepartment is P-9 — dept members by level.
func (s *DeptMembershipService) ListByDepartment(ctx context.Context, tenantID, deptID uuid.UUID) ([]domain.DeptMembership, error) {
	return s.deptMemberships.ListByDepartment(ctx, tenantID, deptID)
}

// Assign is P-10 — assign a user to a dept at a level. Idempotent when
// level unchanged. Dept must be activated for the tenant (fk_dm_tenant_dept
// enforces this at DB level).
func (s *DeptMembershipService) Assign(ctx context.Context, tenantID, userID, deptID uuid.UUID, level domain.DeptRole, actorID uuid.UUID) (*domain.DeptMembership, error) {
	if level != domain.DeptPreparator && level != domain.DeptReviewer && level != domain.DeptApprover {
		return nil, domain.NewError(domain.ErrValidation, "invalid role_level").
			WithDetails(map[string]any{"code": "invalid_role_level"})
	}
	// Ensure tenant has activated the department.
	td, err := s.tenantDepts.Find(ctx, tenantID, deptID)
	if err != nil {
		return nil, err
	}
	if !td.IsActive {
		return nil, domain.NewError(domain.ErrDepartmentNotActiveForTenant, "department not active for tenant")
	}
	m, err := s.memberships.FindByUserID(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	// Detect grant-vs-level-change so we emit the right event type.
	var previous *domain.DeptMembership
	existing, _ := s.deptMemberships.ListByUser(ctx, tenantID, userID)
	for i := range existing {
		if existing[i].DepartmentID == deptID {
			previous = &existing[i]
			break
		}
	}
	var dm *domain.DeptMembership
	err = s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		out, err := s.deptMemberships.Assign(txCtx, tenantID, userID, deptID, m.ID, level, actorID)
		if err != nil {
			return err
		}
		dm = out
		pub, _ := port.EventPublisherFromContext(txCtx)
		if pub == nil {
			return nil
		}
		if previous == nil {
			// Fresh grant.
			return pub.EnqueueCtx(txCtx, &domain.DomainEvent{
				Type: domain.EventDepartmentMembershipGranted, TenantID: tenantID,
				Subject: userID.String(), Actor: actorID.String(),
				Data: domain.DepartmentMembershipGrantedPayload{
					UserID: userID, TenantID: tenantID, DepartmentID: deptID, Level: level, ActorID: actorID,
				},
			})
		}
		if previous.RoleLevel != level {
			// Level change.
			return pub.EnqueueCtx(txCtx, &domain.DomainEvent{
				Type: domain.EventDepartmentMembershipLevelChanged, TenantID: tenantID,
				Subject: userID.String(), Actor: actorID.String(),
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
// §8.8.4: if the user is currently a delegate for any active delegation
// scoped to this dept, the removal is gated by WFI-11 dept-scope delegate
// impact. Phase 2 stub returns 0 workflows; Phase 4 wires real check.
func (s *DeptMembershipService) Remove(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.DeptMembership, error) {
	// WFI-11 gate — Phase 2 stub returns 0 active.
	impact, err := s.workflow.GetDelegateImpact(ctx, tenantID, userID, nil)
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
		return pub.EnqueueCtx(txCtx, &domain.DomainEvent{
			Type: domain.EventDepartmentMembershipRevoked, TenantID: tenantID,
			Subject: userID.String(),
			// Actor unavailable at this call site (P-11 handler doesn't carry it into service).
			// Phase 6 refactor: thread actorID through. Meanwhile emit with empty actor.
			Data: domain.DepartmentMembershipRevokedPayload{
				UserID: userID, TenantID: tenantID, DepartmentID: deptID,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx, tenantID)
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
