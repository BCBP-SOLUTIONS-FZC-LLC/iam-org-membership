package service

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
)

// MembershipService owns P-4/P-5/P-7/P-8/P-26/P-27/P-28/P-34. P-6 (invite) lives
// in InvitationService; P-11 (dept remove) in DeptMembershipService.
// RemoveUser is the shared cascade for P-8 (actor) and I-5 (system) per
// WFI-1.
type MembershipService struct {
	memberships     port.MembershipRepository
	roles           port.TenantRoleRepository
	deptMemberships port.DeptMembershipRepository
	tenants         port.TenantRepository
	invitations     port.InvitationRepository
	cache           port.Cache
	rp              port.RealmProvisionerClient
	tokenService    port.TokenServiceClient // AUTH-9 defense-in-depth — optional, see WithTokenServiceClient
	workflow        port.WorkflowClient
	txRunner        port.TxRunner
	logger          port.SlogStyleLogger
	seatOverageDays int
}

// NewMembershipService builds a MembershipService. logger may be nil — it
// then falls back to the top-level log/slog functions (port.SlogStyleLogger's
// zero-value behavior), preserving pre-injection behavior for callers/tests
// that don't wire one in. Production wiring (cmd/server/main.go) passes the
// same gincommon-backed Logger used for HTTP/consumer/outbound-client logs.
func NewMembershipService(
	memberships port.MembershipRepository,
	roles port.TenantRoleRepository,
	deptMemberships port.DeptMembershipRepository,
	tenants port.TenantRepository,
	invitations port.InvitationRepository,
	cache port.Cache,
	rp port.RealmProvisionerClient,
	workflow port.WorkflowClient,
	txRunner port.TxRunner,
	logger port.Logger,
	seatOverageDays int,
) *MembershipService {
	return &MembershipService{
		memberships: memberships, roles: roles, deptMemberships: deptMemberships,
		tenants: tenants, invitations: invitations,
		cache: cache, rp: rp, workflow: workflow, txRunner: txRunner,
		logger: port.NewSlogStyleLogger(logger), seatOverageDays: seatOverageDays,
	}
}

// WithTokenServiceClient injects the AUTH-9 defense-in-depth check. Optional
// — nil (the zero value) means the check is skipped entirely (isServiceAccountOrDegrade
// treats a nil client the same as a failed call: allow the operation), which
// is safe because the primary guarantee is structural (composite FK bar).
func (s *MembershipService) WithTokenServiceClient(c port.TokenServiceClient) *MembershipService {
	s.tokenService = c
	return s
}

// isServiceAccountOrDegrade calls Token Service's TS-5 lookup and, on
// failure, degrades to false (allow the operation) rather than blocking the
// whole ReconcileRoles call — AUTH-9 is defense-in-depth on top of the
// structural composite-FK bar (TR-8/DM-4), not the primary guarantee.
func (s *MembershipService) isServiceAccountOrDegrade(ctx context.Context, tenantID, userID uuid.UUID) bool {
	if s.tokenService == nil {
		return false
	}
	isServiceAccount, err := s.tokenService.IsServiceAccount(ctx, tenantID, userID)
	if err != nil {
		s.logger.WarnContext(ctx, "membership: IsServiceAccount call failed — degrading to allow (AUTH-9 is defense-in-depth, not the primary guarantee)",
			"tenant_id", tenantID, "user_id", userID, "error", err.Error())
		return false
	}
	return isServiceAccount
}

// DelegateImpactAdvisory carries the WFI-13 advisory attached to a P-7
// suspension response when the user is a delegate on active workflows.
// `Checked=false` means the Workflow Service call failed (fail-open); a
// nil advisory means the user had no active delegations to warn about.
type DelegateImpactAdvisory struct {
	Checked         bool        `json:"checked"`
	ActiveWorkflows int         `json:"active_workflows,omitempty"`
	WorkflowIDs     []uuid.UUID `json:"workflow_ids,omitempty"`
}

// SetStatusResult wraps the updated membership plus the optional WFI-13
// advisory. The advisory is populated only on suspend paths (§8.8.5).
type SetStatusResult struct {
	Membership     *domain.TenantMembership
	DelegateImpact *DelegateImpactAdvisory
}

// List is P-4 — keyset-paginated. Hydrates tenant_roles per row and injects
// the derived "member" role (TR-7).
func (s *MembershipService) List(ctx context.Context, tenantID uuid.UUID, cursor *domain.MembershipListCursor, limit int) (*domain.MembershipListPage, error) {
	page, err := s.memberships.List(ctx, tenantID, cursor, limit)
	if err != nil {
		return nil, err
	}
	// N+1 role + dept lookup — acceptable for Phase 2b (Phase 3 optimises via joined query).
	for i := range page.Items {
		mem := &page.Items[i]
		trs, err := s.roles.ListByUser(ctx, tenantID, mem.Membership.UserID)
		if err != nil {
			return nil, err
		}
		// TR-7: "member" is derived at read time — never stored in tenant_roles.
		codes := []domain.TenantRoleCode{domain.RoleMember}
		for _, tr := range trs {
			codes = append(codes, tr.RoleCode)
		}
		mem.TenantRoles = codes
		dms, err := s.deptMemberships.ListByUser(ctx, tenantID, mem.Membership.UserID)
		if err != nil {
			return nil, err
		}
		views := make([]domain.DeptMembershipView, len(dms))
		for j, d := range dms {
			views[j] = domain.DeptMembershipView{DepartmentID: d.DepartmentID, RoleLevel: d.RoleLevel}
		}
		mem.Departments = views
	}
	return page, nil
}

// CheckActiveMembership backs the new internal GET
// /tenants/:id/members/:user_id/exists route, added for iam-tender-acl's
// grant-time membership-existence check (that service's ADR-0007 Wave 3
// extraction, Phase 3 — see iam-tender-acl/O_AND_M_DELTA.md §4). It is
// deliberately a thin FindByUserID wrapper, not a call to Get(): Get()
// also resolves tenant roles and department memberships, which this
// existence check has no use for and which iam-tender-acl calls
// synchronously on every ACL grant (LLD §7.6.2's ≤50ms-p99 budget) —
// the extra queries Get() performs are pure overhead here.
func (s *MembershipService) CheckActiveMembership(ctx context.Context, tenantID, userID uuid.UUID) (*domain.TenantMembership, error) {
	return s.memberships.FindByUserID(ctx, tenantID, userID)
}

// Get is P-5 — single member with roles + dept memberships.
func (s *MembershipService) Get(ctx context.Context, tenantID, userID uuid.UUID) (*domain.MembershipListItem, error) {
	m, err := s.memberships.FindByUserID(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	trs, err := s.roles.ListByUser(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	// TR-7: "member" is derived at read time — never stored in tenant_roles.
	codes := []domain.TenantRoleCode{domain.RoleMember}
	for _, tr := range trs {
		codes = append(codes, tr.RoleCode)
	}
	dms, err := s.deptMemberships.ListByUser(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	views := make([]domain.DeptMembershipView, len(dms))
	for i, d := range dms {
		views[i] = domain.DeptMembershipView{DepartmentID: d.DepartmentID, RoleLevel: d.RoleLevel}
	}
	return &domain.MembershipListItem{Membership: *m, TenantRoles: codes, Departments: views}, nil
}

// SetStatus is P-7 (suspend/reactivate). AUTH-8: after commit, call RP
// RevokeUserSessions best-effort (fail-open, TTL backstop). On suspend we
// also run the WFI-13 §8.8.5 advisory: a best-effort GetDelegateImpact
// whose result rides the 200 response — never a 409, never blocking.
// A Workflow Service 5xx/timeout is logged and omitted (fail-open); an
// urgent security freeze must not depend on Workflow availability.
//
// TM-13: if the target user holds tenant_owner, the suspend is wrapped in a
// RunInTx with SELECT FOR UPDATE on the tenants row, then TM-8 is checked
// inside the lock — same serialisation pattern as P-8 and P-28.
func (s *MembershipService) SetStatus(ctx context.Context, tenantID, userID uuid.UUID, status domain.MembershipStatus, expectedVersion int64) (*SetStatusResult, error) {
	var m *domain.TenantMembership

	if status == domain.MembershipSuspended {
		// TM-13: check whether the target holds tenant_owner before deciding
		// whether to take the serialising lock.
		trs, err := s.roles.ListByUser(ctx, tenantID, userID)
		if err != nil {
			return nil, err
		}
		isOwner := false
		for _, tr := range trs {
			if tr.RoleCode == domain.RoleTenantOwner {
				isOwner = true
				break
			}
		}

		if isOwner {
			// Owner-affecting suspend: take TM-13 tenant row lock, then check
			// TM-8 last-owner guard inside the lock (same pattern as P-8/P-28).
			var txErr error
			err = s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
				if err := s.tenants.LockByID(txCtx, tenantID); err != nil {
					return err
				}
				owners, err := s.roles.CountActiveOwners(txCtx, tenantID)
				if err != nil {
					return err
				}
				if owners <= 1 {
					return domain.NewError(domain.ErrLastOwnerRemoval,
						"cannot suspend the last tenant_owner")
				}
				m, txErr = s.memberships.SetStatus(txCtx, tenantID, userID, status, expectedVersion)
				return txErr
			})
			if err != nil {
				return nil, err
			}
		} else {
			var err error
			m, err = s.memberships.SetStatus(ctx, tenantID, userID, status, expectedVersion)
			if err != nil {
				return nil, err
			}
		}
	} else {
		var err error
		m, err = s.memberships.SetStatus(ctx, tenantID, userID, status, expectedVersion)
		if err != nil {
			return nil, err
		}
	}

	s.invalidateMember(ctx, tenantID)

	res := &SetStatusResult{Membership: m}

	if status == domain.MembershipSuspended {
		// AUTH-8: privilege reduction → best-effort RP session revoke.
		if s.rp != nil {
			_ = s.rp.RevokeUserSessions(ctx, tenantID, userID)
		}

		// WFI-13 (§8.8.5): advisory-only delegate-impact check.
		if s.workflow != nil {
			impact, ierr := s.workflow.GetDelegateImpact(ctx, tenantID, userID, nil)
			if ierr != nil {
				// Fail-open: log, count as unchecked, do not surface.
				if metrics.DelegateSuspendImpact != nil {
					metrics.DelegateSuspendImpact.WithLabelValues("false").Inc()
				}
				s.logger.Warn("WFI-13 delegate-impact advisory: workflow-service unavailable — suspend still committed",
					"tenant_id", tenantID, "user_id", userID, "error", ierr.Error())
				res.DelegateImpact = &DelegateImpactAdvisory{Checked: false}
			} else if impact.ActiveWorkflows > 0 {
				if metrics.DelegateSuspendImpact != nil {
					metrics.DelegateSuspendImpact.WithLabelValues("true").Inc()
				}
				s.logger.Info("delegate_suspend_impact — advisory fired on P-7",
					"tenant_id", tenantID, "user_id", userID,
					"active_workflows", impact.ActiveWorkflows)
				res.DelegateImpact = &DelegateImpactAdvisory{
					Checked:         true,
					ActiveWorkflows: impact.ActiveWorkflows,
					WorkflowIDs:     impact.WorkflowIDs,
				}
			}
		}
	}
	return res, nil
}

// ReconcileRoles is P-28 — full-replacement multi-role reconcile.
// Enforces TM-8 last-owner guard: if the actor is stripping the tenant_owner
// role from the last active owner, return 422 last_owner_removal.
func (s *MembershipService) ReconcileRoles(ctx context.Context, tenantID, userID uuid.UUID, desired []domain.TenantRoleCode, actorID uuid.UUID) ([]domain.TenantRole, []domain.TenantRole, error) {
	// AUTH-9 defense-in-depth: reject a service-account target before any
	// other check. In normal operation this can never fire — the automation
	// principal never acquires a tenant_memberships row, so the membership
	// lookup below would already reject it with 404 member_not_found — but
	// this returns the more specific 403 rather than relying solely on that
	// structural side effect.
	if s.isServiceAccountOrDegrade(ctx, tenantID, userID) {
		return nil, nil, domain.NewError(domain.ErrServiceAccountNotGrantable,
			"service accounts cannot be granted a tenant role")
	}
	// Validate: no 'member' in desired set (TR-7). Use ErrInvalidRole (→ 422)
	// not ErrValidation (→ 400) per LLD §17 invalid_role taxonomy.
	for _, code := range desired {
		if code == domain.RoleMember {
			return nil, nil, domain.NewError(domain.ErrInvalidRole, "member cannot be granted; it is derived").
				WithDetails(map[string]any{"code": "invalid_role"})
		}
		if !code.IsElevated() {
			return nil, nil, domain.NewError(domain.ErrInvalidRole, "unknown role_code").
				WithDetails(map[string]any{"code": "invalid_role"})
		}
	}

	m, err := s.memberships.FindByUserID(ctx, tenantID, userID)
	if err != nil {
		return nil, nil, err
	}

	current, err := s.roles.ListByUser(ctx, tenantID, userID)
	if err != nil {
		return nil, nil, err
	}
	currentSet := map[domain.TenantRoleCode]domain.TenantRole{}
	for _, tr := range current {
		currentSet[tr.RoleCode] = tr
	}
	desiredSet := map[domain.TenantRoleCode]struct{}{}
	for _, code := range desired {
		desiredSet[code] = struct{}{}
	}

	// TM-8 pre-check outside tx: fast-path rejection avoids acquiring the lock.
	// BUG-P28-1 fix: authoritative re-check with SELECT FOR UPDATE is inside the tx.
	strippingOwner := false
	if _, hadOwner := currentSet[domain.RoleTenantOwner]; hadOwner {
		if _, keepsOwner := desiredSet[domain.RoleTenantOwner]; !keepsOwner {
			strippingOwner = true
			ownerCount, err := s.roles.CountActiveOwners(ctx, tenantID)
			if err != nil {
				return nil, nil, err
			}
			if ownerCount <= 1 {
				return nil, nil, domain.NewError(domain.ErrLastOwnerRemoval, "cannot remove the last tenant_owner")
			}
		}
	}
	// GAP-P28-2: if the desired set is empty ([]) and the user held elevated
	// roles, they become a plain member. This is allowed by LLD — only TM-8
	// (last owner) is a hard block. Logged for observability but not rejected.

	// Compute grants + revokes inside one tx so state writes + events
	// commit atomically (§16 A14: one event per role_code, EVT-10).
	var granted []domain.TenantRole
	var revoked []domain.TenantRole
	err = s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		// BUG-P28-1: TM-13 SELECT FOR UPDATE + TM-8 re-check inside the tx
		// so two concurrent P-28 calls stripping different owners serialize
		// correctly and the last-owner invariant holds under contention.
		if strippingOwner {
			if err := s.tenants.LockByID(txCtx, tenantID); err != nil {
				return err
			}
			owners, err := s.roles.CountActiveOwners(txCtx, tenantID)
			if err != nil {
				return err
			}
			if owners <= 1 {
				return domain.NewError(domain.ErrLastOwnerRemoval, "cannot remove the last tenant_owner")
			}
		}
		pub, _ := port.EventPublisherFromContext(txCtx)
		var rcIP, rcUA string
		if rc, ok := requestctx.FromContext(txCtx); ok {
			rcIP = rc.ClientIP
			rcUA = rc.UserAgent
		}
		for code := range desiredSet {
			if _, ok := currentSet[code]; ok {
				continue
			}
			tr, err := s.roles.Grant(txCtx, &domain.TenantRole{
				TenantID: tenantID, UserID: userID,
				TenantMembershipID: m.ID, RoleCode: code, GrantedBy: actorID,
			})
			if err != nil {
				return err
			}
			granted = append(granted, *tr)
			if pub != nil {
				_ = pub.Enqueue(txCtx, &domain.DomainEvent{
					Type:      domain.EventTenantRoleGranted,
					TenantID:  tenantID,
					Subject:   userID.String(),
					Actor:     actorID.String(),
					IPAddress: rcIP,
					UserAgent: rcUA,
					Data: domain.TenantRoleGrantedPayload{
						UserID: userID, TenantID: tenantID, RoleCode: code, ActorID: actorID,
					},
				})
			}
		}
		for code := range currentSet {
			if _, ok := desiredSet[code]; ok {
				continue
			}
			tr, err := s.roles.Revoke(txCtx, tenantID, userID, code)
			if err != nil {
				return err
			}
			revoked = append(revoked, *tr)
			if pub != nil {
				_ = pub.Enqueue(txCtx, &domain.DomainEvent{
					Type:      domain.EventTenantRoleRevoked,
					TenantID:  tenantID,
					Subject:   userID.String(),
					Actor:     actorID.String(),
					IPAddress: rcIP,
					UserAgent: rcUA,
					Data: domain.TenantRoleRevokedPayload{
						UserID: userID, TenantID: tenantID, RoleCode: code, ActorID: actorID,
					},
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	s.invalidateMember(ctx, tenantID)
	if s.cache != nil {
		_ = s.cache.Delete(ctx, cacheKeyMemberships(tenantID, userID))
	}

	// AUTH-8: if we revoked any role, best-effort session revoke.
	if len(revoked) > 0 && s.rp != nil {
		_ = s.rp.RevokeUserSessions(ctx, tenantID, userID)
	}
	return granted, revoked, nil
}

// RemovalAction is the P-26 request body's action field.
type RemovalAction string

const (
	RemovalReplaceDelegate RemovalAction = "replace_delegate"
	RemovalStopWorkflows   RemovalAction = "stop_workflows"
)

// RemoveUser is P-8 — the tenant-facing user removal. Shared with I-5 via
// WFI-1 (both entry points call this method so the delegate-impact
// pre-check applies uniformly).
//
// Flow (§8.8, LLD line 3388-3405):
//  1. WorkflowClient.GetDelegateImpact — if active_workflows > 0, refuse
//     409 workflow_resolution_required (WFI-3, no state change, no event).
//  2. TM-13: SELECT ... FOR UPDATE on tenants inside RunInTx so two
//     concurrent owner-removals serialize on the same row (§16 A44).
//  3. TM-8: inside the lock, refuse 422 last_owner_removal if this would
//     drop active tenant_owner count to zero on the actor path (P-8).
//  4. Cascade: soft-delete tenant_roles, dept_memberships; emit
//     TenantRoleRevoked + DepartmentMembershipRevoked for every affected
//     row, plus one MembershipRevoked signal (ADR-0008 §6.4, LLD §15.2.2)
//     so the Delegation and Tender-ACL services asynchronously end this
//     user's rows in their own databases — atomically (EVT-10).
//  5. Soft-delete tenant_memberships.
//  6. AUTH-8: post-commit best-effort RP RevokeUserSessions (fail-open).
func (s *MembershipService) RemoveUser(ctx context.Context, tenantID, userID, actorID uuid.UUID) error {
	// Step 1 — WFI-3 pre-check (outside tx, before any state change).
	if s.workflow != nil {
		impact, werr := s.workflow.GetDelegateImpact(ctx, tenantID, userID, nil)
		if werr != nil {
			// Hard dependency (WFI-8): a 5xx here surfaces as 503, matching
			// the LLD's "the freeze must never depend on Workflow" only
			// applies to suspend (WFI-13). Removal blocks on Workflow.
			return domain.NewError(domain.ErrWorkflowServiceUnavailable, "workflow service unavailable")
		}
		if impact.ActiveWorkflows > 0 {
			return domain.NewError(domain.ErrWorkflowResolutionRequired, "active workflows depend on this delegate").
				WithDetails(map[string]any{
					"active_workflows": impact.ActiveWorkflows,
					"workflow_ids":     impact.WorkflowIDs,
					"allowed_actions":  []string{string(RemovalReplaceDelegate), string(RemovalStopWorkflows)},
				})
		}
	}

	err := s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		// Step 2 — TM-13 lock (LLD line 1183).
		if err := s.tenants.LockByID(txCtx, tenantID); err != nil {
			return err
		}
		mem, err := s.memberships.FindByUserID(txCtx, tenantID, userID)
		if err != nil {
			return err
		}
		trs, err := s.roles.ListByUser(txCtx, tenantID, userID)
		if err != nil {
			return err
		}
		wasOwner := false
		for _, tr := range trs {
			if tr.RoleCode == domain.RoleTenantOwner {
				wasOwner = true
				break
			}
		}
		// Step 3 — TM-8 actor-path last-owner refusal (§8.8, LLD 1178).
		if wasOwner {
			owners, err := s.roles.CountActiveOwners(txCtx, tenantID)
			if err != nil {
				return err
			}
			if owners <= 1 {
				return domain.NewError(domain.ErrLastOwnerRemoval, "cannot remove the last tenant_owner")
			}
		}

		pub, _ := port.EventPublisherFromContext(txCtx)
		var rcIP, rcUA string
		if rc, ok := requestctx.FromContext(txCtx); ok {
			rcIP = rc.ClientIP
			rcUA = rc.UserAgent
		}

		// Step 4 — cascade + events. Order chosen so downstream authz
		// state (roles, delegations) is revoked before the membership row
		// disappears; consumers see a consistent sequence.
		revokedRoles, err := s.roles.SoftDeleteAllForUser(txCtx, tenantID, userID)
		if err != nil {
			return err
		}
		for _, r := range revokedRoles {
			if pub != nil {
				if err := pub.Enqueue(txCtx, &domain.DomainEvent{
					Type: domain.EventTenantRoleRevoked, TenantID: tenantID,
					Subject: userID.String(), Actor: actorID.String(),
					IPAddress: rcIP, UserAgent: rcUA,
					Data: domain.TenantRoleRevokedPayload{
						UserID: userID, TenantID: tenantID, RoleCode: r.RoleCode, ActorID: actorID,
					},
				}); err != nil {
					return err
				}
			}
		}
		revokedDepts, err := s.deptMemberships.SoftDeleteAllForUser(txCtx, tenantID, userID)
		if err != nil {
			return err
		}
		for _, d := range revokedDepts {
			if pub != nil {
				if err := pub.Enqueue(txCtx, &domain.DomainEvent{
					Type: domain.EventDepartmentMembershipRevoked, TenantID: tenantID,
					Subject: userID.String(), Actor: actorID.String(),
					IPAddress: rcIP, UserAgent: rcUA,
					Data: domain.DepartmentMembershipRevokedPayload{
						UserID: userID, TenantID: tenantID, DepartmentID: d.DepartmentID, ActorID: actorID,
					},
				}); err != nil {
					return err
				}
			}
		}
		// ADR-0008 (§6.4): the delegation and tender-ACL cascades that used
		// to run inline here (s.delegations.SoftDeleteForUser + per-row
		// DelegationEnded{delegate_removed}; s.acls.SoftDeleteForUser)
		// moved to the Delegation Service's and Tender-ACL Service's own
		// async consumers, both of which subscribe to this single shared
		// MembershipRevoked emission (LLD §15.2.2) and run their own
		// cascades. Emitted unconditionally — not gated on whether the
		// user actually held any delegation/ACL rows; both consumers are
		// idempotent regardless.
		if pub != nil {
			if err := pub.Enqueue(txCtx, &domain.DomainEvent{
				Type: domain.EventMembershipRevoked, TenantID: tenantID,
				Subject: userID.String(), Actor: actorID.String(),
				IPAddress: rcIP, UserAgent: rcUA,
				Data: domain.MembershipRevokedPayload{
					TenantID: tenantID, UserID: userID, ActorID: actorID,
				},
			}); err != nil {
				return err
			}
		}
		// Step 5 — the membership row itself.
		if err := s.memberships.SoftDelete(txCtx, tenantID, userID, mem.RecordVersion); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}

	s.invalidateMember(ctx, tenantID)

	// Step 6 — AUTH-8 best-effort session revoke.
	if s.rp != nil {
		_ = s.rp.RevokeUserSessions(ctx, tenantID, userID)
	}
	return nil
}

// RemovalResolution is P-26 (§8.8.3). Resolves a blocked P-8 by either
// (a) reassigning the user's active delegations to a replacement user, or
// (b) stopping all workflows the user is delegate on — then the caller
// retries P-8. This method does NOT itself remove the user (that's P-8);
// it clears the workflow-side dependency and returns success so the
// caller can re-issue DELETE and race-safety-re-check (WFI-6).
func (s *MembershipService) RemovalResolution(ctx context.Context, tenantID, userID uuid.UUID, action RemovalAction, replacementUserID *uuid.UUID, actorID uuid.UUID) error {
	if s.workflow == nil {
		return domain.NewError(domain.ErrWorkflowServiceUnavailable, "workflow service not configured")
	}
	switch action {
	case RemovalReplaceDelegate:
		if replacementUserID == nil {
			return domain.NewError(domain.ErrValidation, "replacement_user_id is required for replace_delegate")
		}
		if *replacementUserID == userID {
			return domain.NewError(domain.ErrInvalidReplacement, "replacement cannot be the same user being removed").
				WithDetails(map[string]any{"code": "invalid_replacement"})
		}
		// WFI-5: the replacement must be an active member of this tenant.
		rep, err := s.memberships.FindByUserID(ctx, tenantID, *replacementUserID)
		if err != nil || rep.Status != domain.MembershipActive {
			return domain.NewError(domain.ErrInvalidReplacement, "replacement is not an active member").
				WithDetails(map[string]any{"code": "invalid_replacement"})
		}
		if err := s.workflow.ReassignDelegate(ctx, tenantID, userID, *replacementUserID, nil); err != nil {
			return domain.NewError(domain.ErrWorkflowServiceUnavailable, "workflow service unavailable")
		}
	case RemovalStopWorkflows:
		if err := s.workflow.CancelByDelegate(ctx, tenantID, userID, nil); err != nil {
			return domain.NewError(domain.ErrWorkflowServiceUnavailable, "workflow service unavailable")
		}
	default:
		return domain.NewError(domain.ErrInvalidAction, "action must be replace_delegate or stop_workflows")
	}
	return nil
}

// ValidateAndEmitAssigneeOverride is the I-13 (§5.4/A55) service method:
// authorize the actor (tender_admin or higher), validate the new assignee
// against the Workflow-supplied (department_id, required_level), then emit
// TenderAssigneeOverridden via the outbox (§7.3, OVR-1). Persists nothing
// else — the override record is Workflow-owned (§2.2/A32(d)).
//
// Returns 403 insufficient_role if actor lacks tender_admin (or higher).
// Returns 422 assignee_ineligible if the new assignee is not an active
// member holding required_level in department_id.
func (s *MembershipService) ValidateAndEmitAssigneeOverride(ctx context.Context, tenantID, tenderID, newUserID, departmentID uuid.UUID, requiredLevel domain.DeptRole, actorID uuid.UUID) error {
	// AUTH-3 defense-in-depth: actor must hold tender_admin, tenant_admin,
	// or tenant_owner in this tenant. LLD §5.4 I-13 step 1.
	actorRoles, err := s.roles.ListByUser(ctx, tenantID, actorID)
	if err != nil {
		return domain.NewError(domain.ErrInsufficientRole, "actor role check failed")
	}
	hasElevated := false
	for _, tr := range actorRoles {
		if tr.RoleCode == domain.RoleTenderAdmin ||
			tr.RoleCode == domain.RoleTenantAdmin ||
			tr.RoleCode == domain.RoleTenantOwner {
			hasElevated = true
			break
		}
	}
	if !hasElevated {
		return domain.NewError(domain.ErrInsufficientRole, "actor must hold tender_admin, tenant_admin, or tenant_owner")
	}

	item, err := s.Get(ctx, tenantID, newUserID)
	if err != nil {
		return domain.NewError(domain.ErrAssigneeIneligible, "assignee is not a member of this tenant")
	}
	if item.Membership.Status != domain.MembershipActive {
		return domain.NewError(domain.ErrAssigneeIneligible, "assignee is not an active member")
	}
	eligible := false
	for _, d := range item.Departments {
		if d.DepartmentID == departmentID && d.RoleLevel.Satisfies(requiredLevel) {
			eligible = true
			break
		}
	}
	if !eligible {
		return domain.NewError(domain.ErrAssigneeIneligible, "assignee not at required level in department")
	}

	// Emit the event inside a tx so the outbox insert commits atomically
	// (EVT-10 — even though nothing else is written on this path, the
	// outbox pattern still requires a tx).
	return s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		pub, _ := port.EventPublisherFromContext(txCtx)
		if pub == nil {
			return nil
		}
		evt := &domain.DomainEvent{
			Type: domain.EventTenderAssigneeOverridden, TenantID: tenantID,
			Subject: tenderID.String(), Actor: actorID.String(),
			Data: domain.TenderAssigneeOverriddenPayload{
				TenderID: tenderID, TenantID: tenantID,
				UserID: newUserID, ActorID: actorID,
			},
		}
		if rc, ok := requestctx.FromContext(txCtx); ok {
			evt.IPAddress = rc.ClientIP
			evt.UserAgent = rc.UserAgent
		}
		return pub.Enqueue(txCtx, evt)
	})
}

// ResetUserMFA is P-34 (§16 OQ-8/F6) — POST .../members/:user_id/reset-mfa.
// Validates the target is an active member, then calls
// RealmProvisionerClient.ResetMFA (RP-9) synchronously to clear the user's
// TOTP/WebAuthn credentials. Fail-CLOSED, unlike RevokeUserSessions'
// best-effort posture (AUTH-8): there is no reconciler for "eventually
// reset MFA", so an RP-9 failure surfaces as realm_provisioner_unavailable
// (503) and nothing is recorded — the caller must retry. On success, emits
// MFAReset (§7.3) via the outbox for the Audit Log consumer, matching RP's
// confirmed HLD §8.2.6 flow ("Audit Log records MFAReset with actor and
// target").
//
// Role authorization (tenant_admin/tenant_owner) is enforced by the HTTP
// handler (requireTenantAdmin), the same split P-8/P-28 use — this method
// does not re-check it. The actor's own MFA step-up, also part of RP's
// confirmed flow, is a gateway/AuthZ Enrichment concern: pkg/requestctx
// carries no actor-MFA-freshness signal, so O&M's own code has nothing to
// check here (§16 OQ-8).
func (s *MembershipService) ResetUserMFA(ctx context.Context, tenantID, userID, actorID uuid.UUID) error {
	mem, err := s.memberships.FindByUserID(ctx, tenantID, userID)
	if err != nil {
		return err
	}
	if mem.Status != domain.MembershipActive {
		return domain.NewError(domain.ErrMemberNotActive, "member is not active")
	}

	if s.rp == nil {
		return domain.NewError(domain.ErrRealmProvisionerUnavailable, "realm provisioner not configured")
	}
	if err := s.rp.ResetMFA(ctx, tenantID, userID); err != nil {
		return domain.NewError(domain.ErrRealmProvisionerUnavailable, "realm provisioner unavailable")
	}

	return s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		pub, _ := port.EventPublisherFromContext(txCtx)
		if pub == nil {
			return nil
		}
		evt := &domain.DomainEvent{
			Type: domain.EventMFAReset, TenantID: tenantID,
			Subject: userID.String(), Actor: actorID.String(),
			Data: domain.MFAResetPayload{TenantID: tenantID, UserID: userID, ActorID: actorID},
		}
		if rc, ok := requestctx.FromContext(txCtx); ok {
			evt.IPAddress = rc.ClientIP
			evt.UserAgent = rc.UserAgent
		}
		return pub.Enqueue(txCtx, evt)
	})
}

// SeatUsage is P-27/I-11. Reads active_users + pending_invitations vs
// licensed_seats. over_cap = active + pending >= licensed_seats (SEAT-3: at-cap is overage).
func (s *MembershipService) SeatUsage(ctx context.Context, tenantID uuid.UUID) (*domain.SeatUsage, error) {
	t, err := s.tenants.FindByIDIncludingDeleted(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	active, err := s.memberships.CountActive(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	pending, err := s.invitations.CountPending(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	usage := &domain.SeatUsage{
		TenantID:           tenantID,
		ActiveUsers:        active,
		PendingInvitations: pending,
		LicensedSeats:      t.LicensedSeats,
		OverCap:            active+pending >= t.LicensedSeats,
		OverageSince:       t.OverageSince,
	}
	if t.OverageSince != nil && s.seatOverageDays > 0 {
		end := t.OverageSince.Add(time.Duration(s.seatOverageDays) * 24 * time.Hour)
		usage.GraceEndsAt = &end
	}
	return usage, nil
}

func (s *MembershipService) invalidateMember(ctx context.Context, tenantID uuid.UUID) {
	if s.cache == nil {
		return
	}
	_ = s.cache.Delete(ctx,
		cacheKeyMembers(tenantID, 50),
		cacheKeySeatUsage(tenantID),
	)
}
