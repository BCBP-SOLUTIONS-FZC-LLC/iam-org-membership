package service

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

// InvitationService owns P-6/P-30/P-31. §8.10 two-step invite→accept with
// SEAT-1 cap enforcement.
type InvitationService struct {
	invites     port.InvitationRepository
	memberships port.MembershipRepository
	tenants     port.TenantRepository
	rp          port.RealmProvisionerClient
	cache       port.Cache
	expiryDays  int
}

func NewInvitationService(inv port.InvitationRepository, m port.MembershipRepository, t port.TenantRepository, rp port.RealmProvisionerClient, cache port.Cache, expiryDays int) *InvitationService {
	if expiryDays <= 0 {
		expiryDays = 7
	}
	return &InvitationService{invites: inv, memberships: m, tenants: t, rp: rp, cache: cache, expiryDays: expiryDays}
}

func (s *InvitationService) List(ctx context.Context, tenantID uuid.UUID) ([]domain.PendingInvitation, error) {
	return s.invites.List(ctx, tenantID)
}

// Invite is P-6. Enforces SEAT-1 cap: active + pending ≤ licensed_seats.
// Returns 409 seat_limit_reached at/above cap. Returns 409
// invitation_already_exists on duplicate email.
func (s *InvitationService) Invite(ctx context.Context, tenantID uuid.UUID, req InvitationInput, actorID uuid.UUID) (*domain.PendingInvitation, error) {
	if req.Email == "" {
		return nil, domain.NewError(domain.ErrValidation, "email is required")
	}
	if req.FullName == "" {
		return nil, domain.NewError(domain.ErrValidation, "full_name is required")
	}
	for _, r := range req.InitialTenantRoles {
		if !r.IsElevated() {
			return nil, domain.NewError(domain.ErrValidation, "initial role must be elevated (not 'member')").
				WithDetails(map[string]any{"code": "invalid_role"})
		}
	}

	// Existing pending for same email?
	existing, err := s.invites.FindPendingByEmail(ctx, tenantID, req.Email)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, domain.NewError(domain.ErrInvitationAlreadyExists, "invitation already exists for this email")
	}

	// SEAT-1: transactional cap check. In Phase 2b we do this outside a
	// FOR UPDATE lock; Phase 3 upgrades to a proper RunInTx with FOR UPDATE
	// on the tenants row for stronger concurrency guarantees.
	t, err := s.tenants.FindByID(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	active, err := s.memberships.CountActive(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	pending, err := s.invites.CountPending(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if active+pending >= t.LicensedSeats {
		return nil, domain.NewError(domain.ErrSeatLimitReached, "seat limit reached").WithDetails(map[string]any{
			"licensed_seats":      t.LicensedSeats,
			"active_users":        active,
			"pending_invitations": pending,
		})
	}

	// Create Keycloak user via RP (Phase 2 stub returns a random UUID).
	rpResp, err := s.rp.CreateInvitedUser(ctx, port.CreateInvitedUserRequest{
		TenantID: tenantID,
		Email:    req.Email,
		FullName: req.FullName,
	})
	if err != nil {
		return nil, domain.NewError(domain.ErrRealmProvisionerUnavailable, "realm provisioner unavailable")
	}

	// Stage the pending row.
	inv := &domain.PendingInvitation{
		TenantID:            tenantID,
		Email:               req.Email,
		FullName:            req.FullName,
		InitialTenantRoles:  req.InitialTenantRoles,
		InitialDeptMappings: req.InitialDeptMappings,
		InvitedBy:           actorID,
		KeycloakUserID:      &rpResp.KeycloakUserID,
		Status:              domain.InvitePending,
		ExpiresAt:           time.Now().UTC().Add(time.Duration(s.expiryDays) * 24 * time.Hour),
	}
	created, err := s.invites.Insert(ctx, inv)
	if err != nil {
		return nil, err
	}

	if s.cache != nil {
		_ = s.cache.Delete(ctx, cacheKeySeatUsage(tenantID))
	}
	return created, nil
}

// Revoke is P-31. Sets status='revoked' and kc_cleanup_pending=true for the
// invitation-kc-cleanup reconciler (PI-9).
func (s *InvitationService) Revoke(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64) (*domain.PendingInvitation, error) {
	inv, err := s.invites.SetStatus(ctx, tenantID, id, domain.InviteRevoked, expectedVersion)
	if err != nil {
		return nil, err
	}
	// Mark for KC cleanup — reconciler in Phase 5 sweeps and calls RP DeleteUser.
	if err := s.invites.SetKCCleanupPending(ctx, tenantID, id, true); err != nil {
		return nil, err
	}
	if s.cache != nil {
		_ = s.cache.Delete(ctx, cacheKeySeatUsage(tenantID))
	}
	return inv, nil
}

type InvitationInput struct {
	Email               string
	FullName            string
	InitialTenantRoles  []domain.TenantRoleCode
	InitialDeptMappings []domain.InvitationDeptMapping
}
