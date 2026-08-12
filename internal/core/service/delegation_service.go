package service

import (
	"context"
	"strings"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
)

// DelegationService owns P-18/P-19/P-20/P-32/P-33. §8.6: availability-first — call
// UP SetAvailability BEFORE inserting the delegation row. Phase 2 stub UP
// returns nil so the flow proceeds; Phase 4 wires the real HTTP client
// with 4xx-on-race → 422 invalid_delegate translation.
//
// tenants is optional (nil in unit tests). When non-nil, per-tenant
// delegation_max_duration_days/delegation_review_window_days are read for
// P-19/P-32/P-33 validation (§16 A71, DEL-14). When nil, reviewWindowDays
// fallback is used and the max-duration span check is skipped.
type DelegationService struct {
	delegations      port.DelegationRepository
	memberships      port.MembershipRepository
	userProfile      port.UserProfileClient
	tenants          port.TenantRepository // optional; nil → use reviewWindowDays fallback
	txRunner         port.TxRunner
	reviewWindowDays int // fallback when tenants is nil
}

func NewDelegationService(d port.DelegationRepository, m port.MembershipRepository, up port.UserProfileClient, tenants port.TenantRepository, txRunner port.TxRunner, reviewWindowDays ...int) *DelegationService {
	rwd := 90
	if len(reviewWindowDays) > 0 && reviewWindowDays[0] > 0 {
		rwd = reviewWindowDays[0]
	}
	return &DelegationService{delegations: d, memberships: m, userProfile: up, tenants: tenants, txRunner: txRunner, reviewWindowDays: rwd}
}

func (s *DelegationService) List(ctx context.Context, tenantID uuid.UUID) ([]domain.Delegation, error) {
	return s.delegations.List(ctx, tenantID)
}

// Create is P-19. Validates DEL-1..DEL-8. Availability-first ordering.
func (s *DelegationService) Create(ctx context.Context, tenantID, delegatorID uuid.UUID, req DelegationCreateInput) (*domain.Delegation, error) {
	if delegatorID == req.DelegateID {
		return nil, domain.NewError(domain.ErrSelfDelegation, "cannot delegate to yourself")
	}
	scope := domain.DelegationScope(req.Scope)
	if scope != domain.ScopeAll && scope != domain.ScopeDepartment && scope != domain.ScopeTender {
		return nil, domain.NewError(domain.ErrValidation, "invalid scope").
			WithDetails(map[string]any{"code": "invalid_delegation_scope"})
	}
	if scope != domain.ScopeAll && req.ScopeID == nil {
		return nil, domain.NewError(domain.ErrScopeIDRequired, "scope_id is required for department/tender scope")
	}
	// DEL-2: scope=all ⇒ scope_id MUST be null (chk_scope_id DB constraint parity at service layer).
	if scope == domain.ScopeAll && req.ScopeID != nil {
		return nil, domain.NewError(domain.ErrValidation, "scope_id must be omitted when scope is all").
			WithDetails(map[string]any{"code": "invalid_scope_id"})
	}
	// DEL-10: reason capped at 500 chars (service-validated).
	if len(req.Reason) > 500 {
		return nil, domain.NewError(domain.ErrValidation, "reason must not exceed 500 characters").
			WithDetails(map[string]any{"code": "reason_too_long"})
	}
	starts := time.Now().UTC()
	if req.StartsAt != nil {
		starts = *req.StartsAt
	}
	// §16 A65: reject starts_at in the past (service-layer, before UP call, zero network cost).
	// Allow a 5-second clock-skew tolerance so clients with minor drift aren't rejected.
	const skewTolerance = 5 * time.Second
	if req.StartsAt != nil && starts.Before(time.Now().UTC().Add(-skewTolerance)) {
		return nil, domain.NewError(domain.ErrDelegationStartInPast, "starts_at must not be in the past")
	}
	// §16 A71, DEL-14: reject starts_at more than 1 year in the future.
	const maxFutureStart = 365 * 24 * time.Hour
	if req.StartsAt != nil && starts.After(time.Now().UTC().Add(maxFutureStart)) {
		return nil, domain.NewError(domain.ErrDelegationStartTooFarFuture, "starts_at must be within 1 year from now")
	}
	if req.EndsAt != nil && !req.EndsAt.After(starts) {
		return nil, domain.NewError(domain.ErrDelegationWindowInverted, "ends_at must be after starts_at")
	}
	// §16 A71, DEL-14: reject fixed-end spans exceeding tenant's delegation_max_duration_days.
	if req.EndsAt != nil && s.tenants != nil {
		if tenant, err := s.tenants.FindByID(ctx, tenantID); err == nil {
			maxSpan := time.Duration(tenant.DelegationMaxDurationDays) * 24 * time.Hour
			if req.EndsAt.Sub(starts) > maxSpan {
				return nil, domain.NewError(domain.ErrDelegationWindowTooLong,
					"delegation span exceeds the tenant's delegation_max_duration_days").
					WithDetails(map[string]any{"delegation_max_duration_days": tenant.DelegationMaxDurationDays})
			}
		}
	}
	// DEL-1: both parties must be active members.
	dgtMem, err := s.memberships.FindByUserID(ctx, tenantID, delegatorID)
	if err != nil {
		return nil, err
	}
	if dgtMem.Status != domain.MembershipActive {
		// Bug B-17 fix: delegator must also be active (DEL-1 requires both parties).
		return nil, domain.NewError(domain.ErrInvalidDelegate, "delegator is not an active member")
	}
	dtMem, err := s.memberships.FindByUserID(ctx, tenantID, req.DelegateID)
	if err != nil {
		return nil, domain.NewError(domain.ErrInvalidDelegate, "delegate is not an active member")
	}
	if dtMem.Status != domain.MembershipActive {
		return nil, domain.NewError(domain.ErrInvalidDelegate, "delegate is not an active member")
	}

	// §8.6 availability-first: call UP first. On UP failure/race, return
	// 422 invalid_delegate. Phase 2 stub always returns nil.
	// Clamp oooFrom to now() when starts_at is in the past — UP rejects past
	// ooo_from, but our service allows past starts_at (delegation is immediately
	// active per LLD). The OOO is starting now from UP's perspective.
	oooFrom := starts
	if oooFrom.Before(time.Now().UTC()) {
		oooFrom = time.Now().UTC()
	}
	oooUntil := req.EndsAt
	oooStatus := "ooo"
	if err := s.userProfile.SetAvailability(ctx, port.SetAvailabilityRequest{
		TenantID:   tenantID,
		UserID:     delegatorID,
		Status:     &oooStatus,
		OOOFrom:    &oooFrom,
		OOOUntil:   oooUntil,
		DelegateID: &req.DelegateID,
		Note:       req.Reason, // DEL-10: reason stored in org-membership, sent as note to UP dashboard
	}); err != nil {
		// §16 A65: UP returns 422 delegate_unavailable when the proposed delegate is OOO.
		if strings.Contains(err.Error(), "delegate_unavailable") {
			return nil, domain.NewError(domain.ErrDelegateUnavailable, "delegate is currently unavailable (OOO)")
		}
		return nil, domain.NewError(domain.ErrInvalidDelegate, "delegate validation failed via user profile").
			WithDetails(map[string]any{"code": "invalid_delegate"})
	}

	var d *domain.Delegation
	err = s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		var reviewDueAt *time.Time
		if req.EndsAt == nil {
			// Use tenant's delegation_review_window_days when available (§16 A71),
			// falling back to per-delegation override, then global fallback.
			windowDays := s.reviewWindowDays
			if s.tenants != nil {
				if tenant, tErr := s.tenants.FindByID(ctx, tenantID); tErr == nil {
					windowDays = tenant.DelegationReviewWindowDays
				}
			}
			due := starts.Add(time.Duration(windowDays) * 24 * time.Hour)
			reviewDueAt = &due
		}
		created, err := s.delegations.Insert(txCtx, &domain.Delegation{
			TenantID:              tenantID,
			DelegatorID:           delegatorID,
			DelegateID:            req.DelegateID,
			DelegatorMembershipID: dgtMem.ID,
			DelegateMembershipID:  dtMem.ID,
			Scope:                 scope,
			ScopeID:               req.ScopeID,
			Reason:                req.Reason,
			StartsAt:              starts,
			EndsAt:                req.EndsAt,
			ReviewDueAt:           reviewDueAt,
		})
		if err != nil {
			return err
		}
		d = created
		pub, _ := port.EventPublisherFromContext(txCtx)
		if pub == nil {
			return nil
		}
		evt := &domain.DomainEvent{
			Type: domain.EventDelegationStarted, TenantID: tenantID,
			Subject: created.ID.String(), Actor: delegatorID.String(),
			Data: domain.DelegationStartedPayload{
				DelegationID: created.ID, TenantID: tenantID,
				DelegatorID: delegatorID, DelegateID: req.DelegateID,
				Scope: scope, ScopeID: req.ScopeID, EndsAt: req.EndsAt,
				ActorID: delegatorID,
			},
		}
		if rc, ok := requestctx.FromContext(txCtx); ok {
			evt.IPAddress = rc.ClientIP
			evt.UserAgent = rc.UserAgent
		}
		return pub.EnqueueCtx(txCtx, evt)
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// Cancel is P-20. §8.7 pointer-clear: end delegation locally, then call UP
// with {delegate_id: null} (never {status: available} — UP owns that).
func (s *DelegationService) Cancel(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64) (*domain.Delegation, error) {
	d, err := s.delegations.FindByID(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	// Pointer-clear on UP first. Fail-open: log but proceed if UP is down —
	// the delegation-expiry cron will retry.
	_ = s.userProfile.SetAvailability(ctx, port.SetAvailabilityRequest{
		TenantID:      tenantID,
		UserID:        d.DelegatorID,
		ClearDelegate: true,
	})
	var ended *domain.Delegation
	err = s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		out, err := s.delegations.End(txCtx, tenantID, id, domain.DelegationCancelled, expectedVersion)
		if err != nil {
			return err
		}
		ended = out
		pub, _ := port.EventPublisherFromContext(txCtx)
		if pub == nil {
			return nil
		}
		evt := &domain.DomainEvent{
			Type: domain.EventDelegationEnded, TenantID: tenantID,
			Subject: id.String(), Actor: d.DelegatorID.String(),
			Data: domain.DelegationEndedPayload{
				DelegationID: id, TenantID: tenantID,
				DelegatorID: d.DelegatorID, DelegateID: d.DelegateID,
				Scope: d.Scope, ScopeID: d.ScopeID,
				EndedReason: domain.EndReasonCancelled,
				ActorID:     d.DelegatorID,
			},
		}
		if rc, ok := requestctx.FromContext(txCtx); ok {
			evt.IPAddress = rc.ClientIP
			evt.UserAgent = rc.UserAgent
		}
		return pub.EnqueueCtx(txCtx, evt)
	})
	if err != nil {
		return nil, err
	}
	return ended, nil
}

type DelegationCreateInput struct {
	DelegateID uuid.UUID
	Scope      string
	ScopeID    *uuid.UUID
	Reason     string // DEL-10: stored in delegations.reason + sent to UP as user_availability.note
	StartsAt   *time.Time
	EndsAt     *time.Time
}

// Extend is P-32. Pushes review_due_at forward by another window period.
// Only valid for open-ended delegations (ends_at IS NULL, DEL-13).
// extendDays, when provided, must be in [1, 180] (§16 A71, ErrExtendDaysOutOfRange).
// Priority order: caller-supplied extendDays > per-delegation review_window_days override > tenant default > global fallback.
func (s *DelegationService) Extend(ctx context.Context, tenantID, id uuid.UUID, extendDays *int, expectedVersion int64) (*domain.Delegation, error) {
	// §16 A71: validate extend_days range before any DB read.
	if extendDays != nil && (*extendDays < 1 || *extendDays > 180) {
		return nil, domain.NewError(domain.ErrExtendDaysOutOfRange, "extend_days must be between 1 and 180")
	}
	d, err := s.delegations.FindByID(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if d.Status != domain.DelegationActive {
		return nil, domain.NewError(domain.ErrDelegationNotFound, "delegation not found")
	}
	if d.EndsAt != nil {
		return nil, domain.NewError(domain.ErrDelegationNotOpenEnded, "extend only applies to open-ended delegations")
	}
	// Resolve window days: tenant default > global fallback, overridden by per-delegation row, then by caller.
	windowDays := s.reviewWindowDays
	if s.tenants != nil {
		if tenant, err := s.tenants.FindByID(ctx, tenantID); err == nil {
			windowDays = tenant.DelegationReviewWindowDays
		}
	}
	if d.ReviewWindowDays != nil {
		windowDays = *d.ReviewWindowDays
	}
	if extendDays != nil {
		windowDays = *extendDays
	}
	return s.delegations.ExtendReview(ctx, tenantID, id, windowDays, expectedVersion)
}

// Reassign is P-33. Ends the current delegation and creates a new one with the
// same parameters but a different delegate. Reuses the existing cancel + create flows verbatim.
func (s *DelegationService) Reassign(ctx context.Context, tenantID, actorID, id uuid.UUID, newDelegateID uuid.UUID, expectedVersion int64) (*domain.Delegation, error) {
	existing, err := s.delegations.FindByID(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if existing.Status != domain.DelegationActive {
		return nil, domain.NewError(domain.ErrDelegationNotFound, "delegation not found")
	}
	// End existing delegation.
	if _, err := s.Cancel(ctx, tenantID, id, expectedVersion); err != nil {
		return nil, err
	}
	// Create new delegation with same parameters but new delegate.
	return s.Create(ctx, tenantID, existing.DelegatorID, DelegationCreateInput{
		DelegateID: newDelegateID,
		Scope:      string(existing.Scope),
		ScopeID:    existing.ScopeID,
		Reason:     existing.Reason,
		StartsAt:   nil, // starts now
		EndsAt:     nil, // keep open-ended
	})
}
