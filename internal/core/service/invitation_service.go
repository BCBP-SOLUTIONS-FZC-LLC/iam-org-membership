package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// normalizeEmail trims surrounding whitespace and lowercases the address so
// invite-time and accept-time lookups match regardless of client casing.
// PI-4 matches acceptance by (tenant_id, email) as a fallback to keycloak_user_id;
// without this normalisation a mixed-case invite vs lowercase REGISTER webhook
// (or the reverse) silently misses the pending row and I-3 degrades to plain-add.
func normalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// InvitationService owns P-6/P-30/P-31 and the I-3 acceptance path.
// §8.10 two-step invite→accept with SEAT-1 cap enforcement (TM-13 locking
// discipline: FOR UPDATE on tenants inside RunInTx).
type InvitationService struct {
	invites          port.InvitationRepository
	memberships      port.MembershipRepository
	roles            port.TenantRoleRepository
	deptMems         port.DeptMembershipRepository
	tenants          port.TenantRepository
	rp               port.RealmProvisionerClient
	cache            port.Cache
	txRunner         port.TxRunner
	logger           *slog.Logger
	expiryDays       int
	reinviteCooldown time.Duration // PI-11: 0 = disabled
	maxPerHour       int           // PI-12: 0 = disabled
}

func NewInvitationService(
	inv port.InvitationRepository,
	m port.MembershipRepository,
	roles port.TenantRoleRepository,
	deptMems port.DeptMembershipRepository,
	t port.TenantRepository,
	rp port.RealmProvisionerClient,
	cache port.Cache,
	txRunner port.TxRunner,
	logger *slog.Logger,
	expiryDays int,
) *InvitationService {
	if expiryDays <= 0 {
		expiryDays = 7
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &InvitationService{
		invites: inv, memberships: m, roles: roles, deptMems: deptMems,
		tenants: t, rp: rp, cache: cache, txRunner: txRunner, logger: logger, expiryDays: expiryDays,
	}
}

// WithReinviteCooldown sets the PI-11 per-email cooldown. 0 disables.
func (s *InvitationService) WithReinviteCooldown(d time.Duration) *InvitationService {
	s.reinviteCooldown = d
	return s
}

// WithMaxInvitesPerHour sets the PI-12 per-tenant hourly ceiling. 0 disables.
func (s *InvitationService) WithMaxInvitesPerHour(n int) *InvitationService {
	s.maxPerHour = n
	return s
}

func (s *InvitationService) List(ctx context.Context, tenantID uuid.UUID) ([]domain.PendingInvitation, error) {
	return s.invites.List(ctx, tenantID)
}

// Invite is P-6. Enforces SEAT-1 cap: active + pending ≤ licensed_seats.
// Returns 409 seat_limit_reached at/above cap. Returns 409
// invitation_already_exists on duplicate email.
func (s *InvitationService) Invite(ctx context.Context, tenantID uuid.UUID, req InvitationInput, actorID uuid.UUID) (*domain.PendingInvitation, error) {
	req.Email = normalizeEmail(req.Email)
	if req.Email == "" {
		return nil, domain.NewError(domain.ErrValidation, "email is required")
	}
	// GAP-P6-2: basic RFC 5322 structural check — must contain exactly one @
	// with non-empty local and domain parts.
	if !isValidEmail(req.Email) {
		return nil, domain.NewError(domain.ErrValidation, "email is not a valid address")
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

	// PI-11: per-email re-invite cooldown (§16 A41). Checked pre-flight before
	// the RP call so a refused invite creates no Keycloak user and sends no email.
	if s.reinviteCooldown > 0 {
		mostRecent, err := s.invites.MostRecentCreatedAt(ctx, tenantID, req.Email)
		if err != nil {
			return nil, err
		}
		if !mostRecent.IsZero() {
			elapsed := time.Since(mostRecent)
			if elapsed < s.reinviteCooldown {
				remaining := s.reinviteCooldown - elapsed
				return nil, domain.NewError(domain.ErrReinviteTooSoon, "re-invite too soon").
					WithDetails(map[string]any{
						"retry_after_seconds": int(remaining.Seconds()),
					})
			}
		}
	}

	// PI-12: per-tenant hourly invite ceiling (§16 A41). Checked pre-flight.
	if s.maxPerHour > 0 {
		since := time.Now().UTC().Add(-time.Hour)
		count, err := s.invites.CountCreatedInWindow(ctx, tenantID, since)
		if err != nil {
			return nil, err
		}
		if count >= s.maxPerHour {
			return nil, domain.NewError(domain.ErrInviteRateLimited, "invite rate limit reached").
				WithDetails(map[string]any{
					"retry_after_seconds": 3600,
				})
		}
	}

	// Cheap pre-flight SEAT-1 check outside the tx — saves an RP round-trip
	// when the cap is already reached (SEAT-1 is re-enforced under FOR UPDATE
	// below; this pre-check is advisory only).
	if err := s.preflightSeatCheck(ctx, tenantID); err != nil {
		return nil, err
	}

	// Create Keycloak user via RP (external call, outside the tx — never
	// hold a Postgres tx across a network call, CONS-2).
	rpResp, err := s.rp.CreateInvitedUser(ctx, port.CreateInvitedUserRequest{
		TenantID: tenantID,
		Email:    req.Email,
		FullName: req.FullName,
	})
	if err != nil {
		return nil, domain.NewError(domain.ErrRealmProvisionerUnavailable, "realm provisioner unavailable")
	}

	// SEAT-1/TM-13 (LLD line 815, 40): transactional cap re-check under
	// SELECT ... FOR UPDATE on the tenants row so two concurrent Invite
	// calls cannot both slip past a stale under-cap count. Compensating
	// RP DeleteUser on race (CONS-2 §8.10) — best-effort; on RP failure
	// mark for the invitation-kc-cleanup reconciler (PI-9).
	var created *domain.PendingInvitation
	err = s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		tx, ok := pgadapterTxFromContext(txCtx)
		if !ok {
			return domain.NewError(domain.ErrConflict, "tx unavailable")
		}
		var licensedSeats int
		if err := tx.QueryRow(txCtx, `SELECT licensed_seats FROM tenants WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, tenantID).Scan(&licensedSeats); err != nil {
			return err
		}
		var active, pending int
		if err := tx.QueryRow(txCtx, `SELECT count(*) FROM tenant_memberships WHERE tenant_id = $1 AND status = 'active' AND deleted_at IS NULL`, tenantID).Scan(&active); err != nil {
			return err
		}
		if err := tx.QueryRow(txCtx, `SELECT count(*) FROM pending_invitations WHERE tenant_id = $1 AND status = 'pending' AND expires_at > now()`, tenantID).Scan(&pending); err != nil {
			return err
		}
		if active+pending >= licensedSeats {
			return domain.NewError(domain.ErrSeatLimitReached, "seat limit reached").WithDetails(map[string]any{
				"licensed_seats":      licensedSeats,
				"active_users":        active,
				"pending_invitations": pending,
			})
		}
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
		row, ierr := s.invites.Insert(txCtx, inv)
		if ierr != nil {
			return ierr
		}
		created = row
		return nil
	})
	if err != nil {
		// Compensating cleanup: the KC user was created but we didn't stage
		// a pending_invitations row, so nothing points to it — best-effort
		// delete it now. PI-9 kc_cleanup_pending is the durable analogue
		// (would need a persistent orphan-KC-users tombstone table to hold
		// the reference; not modeled today, so a fail-open RP DeleteUser
		// is the current compensation).
		if delErr := s.rp.DeleteUser(ctx, tenantID, rpResp.KeycloakUserID); delErr != nil {
			s.logger.Warn("SEAT-1 lost race — compensating RP DeleteUser failed",
				"tenant_id", tenantID, "keycloak_user_id", rpResp.KeycloakUserID, "error", delErr.Error())
		}
		return nil, err
	}

	if s.cache != nil {
		_ = s.cache.Delete(ctx, cacheKeySeatUsage(tenantID))
	}
	return created, nil
}

func (s *InvitationService) preflightSeatCheck(ctx context.Context, tenantID uuid.UUID) error {
	t, err := s.tenants.FindByID(ctx, tenantID)
	if err != nil {
		return err
	}
	active, err := s.memberships.CountActive(ctx, tenantID)
	if err != nil {
		return err
	}
	pending, err := s.invites.CountPending(ctx, tenantID)
	if err != nil {
		return err
	}
	if active+pending >= t.LicensedSeats {
		return domain.NewError(domain.ErrSeatLimitReached, "seat limit reached").WithDetails(map[string]any{
			"licensed_seats":      t.LicensedSeats,
			"active_users":        active,
			"pending_invitations": pending,
		})
	}
	return nil
}

// AddFromRegister is I-3 (§8.10 acceptance + JIT plain add). Called by the
// Event Consumer's REGISTER webhook. If a matching pending_invitations row
// exists (by keycloak_user_id first, then by email), flip it to accepted
// and materialise the tenant_membership + queued initial_tenant_roles +
// initial_dept_mappings in the same tx (PI-4). Otherwise perform a plain
// add (no invitation). Emits TenantRoleGranted + DepartmentMembershipGranted
// per applied grant (§7.3, EVT-10).
//
// PI-10 idempotency: the (tenant_id, user_id) uq_tm_active_user + the
// pending→accepted flip make webhook redelivery a safe no-op.
// SEAT-4: on acceptance we re-check SEAT-1 under FOR UPDATE; a decrease in
// licensed_seats between Invite (which passed) and Accept can trip the cap
// — in that case we still let acceptance proceed (SEAT-3 keeps existing
// users; only new invites are blocked), matching the LLD's SEAT-4 posture.
func (s *InvitationService) AddFromRegister(ctx context.Context, tenantID, userID uuid.UUID, keycloakUserID uuid.UUID, email string) (*domain.TenantMembership, error) {
	// Look for a matching pending invitation.
	email = normalizeEmail(email)
	var pending *domain.PendingInvitation
	var err error
	pending, err = s.invites.FindPendingByKeycloakUser(ctx, tenantID, keycloakUserID)
	if err != nil {
		return nil, err
	}
	if pending == nil && email != "" {
		pending, err = s.invites.FindPendingByEmail(ctx, tenantID, email)
		if err != nil {
			return nil, err
		}
	}
	if pending == nil {
		// Neither keycloak_user_id nor email matched an outstanding invitation;
		// I-3 falls through to plain-add (PI-4 additive branch). Log so operators
		// can distinguish "invited user onboarded normally" from "phantom accept"
		// where the acceptance branch was skipped and initial_tenant_roles /
		// initial_dept_mappings were never applied.
		s.logger.Warn("I-3 plain-add: no matching pending invitation, initial roles/depts NOT applied",
			"tenant_id", tenantID,
			"user_id", userID,
			"keycloak_user_id", keycloakUserID,
			"email_supplied", email != "",
		)
	}

	var mem *domain.TenantMembership
	err = s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		tx, ok := pgadapterTxFromContext(txCtx)
		if !ok {
			return domain.NewError(domain.ErrConflict, "tx unavailable")
		}
		// TM-13 lock on tenants for the entire acceptance.
		if _, err := tx.Exec(txCtx, `SELECT id FROM tenants WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, tenantID); err != nil {
			return err
		}

		// PI-4/PI-8 (§8.10 line 1651-1656): re-lock the candidate pending row
		// inside the tx via FOR UPDATE so accept-vs-revoke races serialize
		// deterministically instead of racing to a 409 optimistic-lock
		// conflict. If status flipped under us between the outer lookup
		// and this lock, treat as no-op (idempotent plain-add path).
		if pending != nil {
			var lockedStatus string
			var lockedVersion int64
			err := tx.QueryRow(txCtx,
				`SELECT status, record_version FROM pending_invitations WHERE id = $1 FOR UPDATE`,
				pending.ID).Scan(&lockedStatus, &lockedVersion)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				pending = nil // row vanished — proceed as plain add
			case err != nil:
				return err
			case lockedStatus != string(domain.InvitePending):
				pending = nil // already accepted/revoked/expired — plain add
			case !pending.ExpiresAt.IsZero() && !pending.ExpiresAt.After(time.Now().UTC()):
				// Invitation is status=pending but past expires_at (expiry cron
				// hasn't run yet). Per LLD I-3: acceptance is still honoured on a
				// best-effort basis, but the seat is re-checked under the tenant
				// FOR UPDATE lock (already held above) since expired invitations no
				// longer hold a seat (SEAT-1 counts status='pending' AND
				// expires_at > now() only). On over-cap → reject with 409.
				var occupied, licensed int
				if err := tx.QueryRow(txCtx, `
					SELECT
						(SELECT count(*) FROM tenant_memberships
						 WHERE tenant_id=$1 AND deleted_at IS NULL AND status='active') +
						(SELECT count(*) FROM pending_invitations
						 WHERE tenant_id=$1 AND status='pending' AND expires_at > now()),
						licensed_seats
					FROM tenants WHERE id=$1 AND deleted_at IS NULL`,
					tenantID).Scan(&occupied, &licensed); err != nil {
					return err
				}
				if occupied >= licensed {
					return domain.NewError(domain.ErrSeatLimitReached, "seat limit reached; expired invitation cannot be honoured")
				}
				pending.RecordVersion = lockedVersion
			default:
				pending.RecordVersion = lockedVersion
			}
		}

		// Insert or return existing tenant_membership (PI-10 idempotency
		// via uq_tm_active_user partial-unique).
		out, ierr := s.memberships.Insert(txCtx, &domain.TenantMembership{
			TenantID: tenantID, UserID: userID, Status: domain.MembershipActive,
		})
		if ierr != nil {
			return ierr
		}
		mem = out
		pub, _ := port.EventPublisherFromContext(txCtx)

		if pending != nil {
			// Flip invitation to accepted (PI-4).
			if _, err := s.invites.SetStatus(txCtx, tenantID, pending.ID, domain.InviteAccepted, pending.RecordVersion); err != nil {
				return err
			}
			// Apply queued initial tenant roles.
			for _, code := range pending.InitialTenantRoles {
				if !code.IsElevated() {
					continue // TR-7: member is derived
				}
				tr, gerr := s.roles.Grant(txCtx, &domain.TenantRole{
					TenantID: tenantID, UserID: userID,
					TenantMembershipID: mem.ID, RoleCode: code, GrantedBy: pending.InvitedBy,
				})
				if gerr != nil {
					return gerr
				}
				if pub != nil {
					_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
						Type: domain.EventTenantRoleGranted, TenantID: tenantID,
						Subject: userID.String(), Actor: pending.InvitedBy.String(),
						Data: domain.TenantRoleGrantedPayload{
							UserID: userID, TenantID: tenantID, RoleCode: tr.RoleCode, ActorID: pending.InvitedBy,
						},
					})
				}
			}
			// Apply queued initial dept mappings.
			for _, dm := range pending.InitialDeptMappings {
				assigned, aerr := s.deptMems.Assign(txCtx, tenantID, userID, dm.DepartmentID, mem.ID, dm.Level, pending.InvitedBy)
				if aerr != nil {
					return aerr
				}
				if pub != nil {
					_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
						Type: domain.EventDepartmentMembershipGranted, TenantID: tenantID,
						Subject: userID.String(), Actor: pending.InvitedBy.String(),
						Data: domain.DepartmentMembershipGrantedPayload{
							UserID: userID, TenantID: tenantID,
							DepartmentID: assigned.DepartmentID, Level: assigned.RoleLevel,
							ActorID: pending.InvitedBy,
						},
					})
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if s.cache != nil {
		_ = s.cache.Delete(ctx, cacheKeySeatUsage(tenantID))
	}
	return mem, nil
}

// Revoke is P-31. Sets status='revoked' and kc_cleanup_pending=true for the
// invitation-kc-cleanup reconciler (PI-9). Both updates are optimistic-
// locked (PI-8): SetStatus bumps record_version via TRG-1, and we pass the
// new version to SetKCCleanupPending so a concurrent accept can't overwrite
// the flag.
func (s *InvitationService) Revoke(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64) (*domain.PendingInvitation, error) {
	inv, err := s.invites.SetStatus(ctx, tenantID, id, domain.InviteRevoked, expectedVersion)
	if err != nil {
		return nil, err
	}
	// Mark for KC cleanup — reconciler in Phase 5 sweeps and calls RP DeleteUser.
	if err := s.invites.SetKCCleanupPending(ctx, tenantID, id, true, inv.RecordVersion); err != nil {
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

// isValidEmail is a lightweight structural check (GAP-P6-2): exactly one @,
// non-empty local and domain parts. Full RFC 5322 validation is out of scope.
func isValidEmail(s string) bool {
	at := strings.Index(s, "@")
	return at > 0 && at < len(s)-1 && !strings.Contains(s[at+1:], "@")
}
