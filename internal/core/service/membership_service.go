package service

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

// MembershipService owns P-4/P-5/P-7/P-27/P-28. P-6 (invite) and P-8
// (remove) live in InvitationService and are gated by SEAT-1 / WFI-3.
type MembershipService struct {
	memberships     port.MembershipRepository
	roles           port.TenantRoleRepository
	deptMemberships port.DeptMembershipRepository
	tenants         port.TenantRepository
	invitations     port.InvitationRepository
	cache           port.Cache
	rp              port.RealmProvisionerClient
	txRunner        port.TxRunner
	seatOverageDays int
}

func NewMembershipService(
	memberships port.MembershipRepository,
	roles port.TenantRoleRepository,
	deptMemberships port.DeptMembershipRepository,
	tenants port.TenantRepository,
	invitations port.InvitationRepository,
	cache port.Cache,
	rp port.RealmProvisionerClient,
	txRunner port.TxRunner,
	seatOverageDays int,
) *MembershipService {
	return &MembershipService{
		memberships: memberships, roles: roles, deptMemberships: deptMemberships,
		tenants: tenants, invitations: invitations,
		cache: cache, rp: rp, txRunner: txRunner, seatOverageDays: seatOverageDays,
	}
}

// List is P-4 — keyset-paginated. Hydrates tenant_roles per row and injects
// the derived "member" role (TR-7).
func (s *MembershipService) List(ctx context.Context, tenantID uuid.UUID, cursor *domain.MembershipListCursor, limit int) (*domain.MembershipListPage, error) {
	page, err := s.memberships.List(ctx, tenantID, cursor, limit)
	if err != nil {
		return nil, err
	}
	// N+1 role lookup — acceptable for Phase 2b (Phase 3 optimises via joined query).
	for i := range page.Items {
		mem := &page.Items[i]
		trs, err := s.roles.ListByUser(ctx, tenantID, mem.Membership.UserID)
		if err != nil {
			return nil, err
		}
		codes := []domain.TenantRoleCode{domain.RoleMember} // TR-7 derived injection
		for _, tr := range trs {
			codes = append(codes, tr.RoleCode)
		}
		mem.TenantRoles = codes
	}
	return page, nil
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
// RevokeUserSessions best-effort (fail-open, TTL backstop). Only on
// suspend — reactivate doesn't need it.
func (s *MembershipService) SetStatus(ctx context.Context, tenantID, userID uuid.UUID, status domain.MembershipStatus, expectedVersion int64) (*domain.TenantMembership, error) {
	m, err := s.memberships.SetStatus(ctx, tenantID, userID, status, expectedVersion)
	if err != nil {
		return nil, err
	}
	s.invalidateMember(ctx, tenantID)

	// AUTH-8: privilege reduction → best-effort RP session revoke.
	if status == domain.MembershipSuspended {
		// Phase 4 wires the real HTTP client; Phase 2 stub is a no-op.
		_ = s.rp.RevokeUserSessions(ctx, tenantID, userID)
	}
	return m, nil
}

// ReconcileRoles is P-28 — full-replacement multi-role reconcile.
// Enforces TM-8 last-owner guard: if the actor is stripping the tenant_owner
// role from the last active owner, return 422 last_owner_removal.
func (s *MembershipService) ReconcileRoles(ctx context.Context, tenantID, userID uuid.UUID, desired []domain.TenantRoleCode, actorID uuid.UUID) ([]domain.TenantRole, []domain.TenantRole, error) {
	// Validate: no 'member' in desired set (TR-7).
	for _, code := range desired {
		if code == domain.RoleMember {
			return nil, nil, domain.NewError(domain.ErrValidation, "member cannot be granted; it is derived").
				WithDetails(map[string]any{"code": "invalid_role"})
		}
		if !code.IsElevated() {
			return nil, nil, domain.NewError(domain.ErrValidation, "unknown role_code").
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

	// TM-8 last-owner guard: if user is currently tenant_owner and desired
	// set omits tenant_owner, check whether they're the last active owner.
	if _, hadOwner := currentSet[domain.RoleTenantOwner]; hadOwner {
		if _, keepsOwner := desiredSet[domain.RoleTenantOwner]; !keepsOwner {
			ownerCount, err := s.roles.CountActiveOwners(ctx, tenantID)
			if err != nil {
				return nil, nil, err
			}
			if ownerCount <= 1 {
				// TM-8: dedicated sentinel maps to 422 (domain rule per §17).
				return nil, nil, domain.NewError(domain.ErrLastOwnerRemoval, "cannot remove the last tenant_owner")
			}
		}
	}

	// Compute grants + revokes inside one tx so state writes + events
	// commit atomically (§16 A14: one event per role_code, EVT-10).
	var granted []domain.TenantRole
	var revoked []domain.TenantRole
	err = s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		pub, _ := port.EventPublisherFromContext(txCtx)
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
				_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
					Type:     domain.EventTenantRoleGranted,
					TenantID: tenantID,
					Subject:  userID.String(),
					Actor:    actorID.String(),
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
				_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
					Type:     domain.EventTenantRoleRevoked,
					TenantID: tenantID,
					Subject:  userID.String(),
					Actor:    actorID.String(),
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

	// AUTH-8: if we revoked any role, best-effort session revoke.
	if len(revoked) > 0 {
		_ = s.rp.RevokeUserSessions(ctx, tenantID, userID)
	}
	return granted, revoked, nil
}

// SeatUsage is P-27/I-11. Reads active_users + pending_invitations vs
// licensed_seats. over_cap = active + pending > licensed_seats.
func (s *MembershipService) SeatUsage(ctx context.Context, tenantID uuid.UUID) (*domain.SeatUsage, error) {
	t, err := s.tenants.FindByID(ctx, tenantID)
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
		OverCap:            active+pending > t.LicensedSeats,
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
