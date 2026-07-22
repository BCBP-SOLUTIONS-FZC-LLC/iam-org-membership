package service

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

// DelegationService owns P-18/P-19/P-20. §8.6: availability-first — call
// UP SetAvailability BEFORE inserting the delegation row. Phase 2 stub UP
// returns nil so the flow proceeds; Phase 4 wires the real HTTP client
// with 4xx-on-race → 422 invalid_delegate translation.
type DelegationService struct {
	delegations port.DelegationRepository
	memberships port.MembershipRepository
	userProfile port.UserProfileClient
	txRunner    port.TxRunner
}

func NewDelegationService(d port.DelegationRepository, m port.MembershipRepository, up port.UserProfileClient, txRunner port.TxRunner) *DelegationService {
	return &DelegationService{delegations: d, memberships: m, userProfile: up, txRunner: txRunner}
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
	starts := time.Now().UTC()
	if req.StartsAt != nil {
		starts = *req.StartsAt
	}
	if req.EndsAt != nil && !req.EndsAt.After(starts) {
		return nil, domain.NewError(domain.ErrDelegationWindowInverted, "ends_at must be after starts_at")
	}
	// DEL-1: both parties must be active members.
	dgtMem, err := s.memberships.FindByUserID(ctx, tenantID, delegatorID)
	if err != nil {
		return nil, err
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
	oooFrom := starts
	oooUntil := req.EndsAt
	oooStatus := "ooo"
	if err := s.userProfile.SetAvailability(ctx, port.SetAvailabilityRequest{
		TenantID:   tenantID,
		UserID:     delegatorID,
		Status:     &oooStatus,
		OOOFrom:    &oooFrom,
		OOOUntil:   oooUntil,
		DelegateID: &req.DelegateID,
	}); err != nil {
		return nil, domain.NewError(domain.ErrInvalidDelegate, "delegate validation failed via user profile").
			WithDetails(map[string]any{"code": "invalid_delegate"})
	}

	var d *domain.Delegation
	err = s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
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
		})
		if err != nil {
			return err
		}
		d = created
		pub, _ := port.EventPublisherFromContext(txCtx)
		if pub == nil {
			return nil
		}
		return pub.EnqueueCtx(txCtx, &domain.DomainEvent{
			Type: domain.EventDelegationStarted, TenantID: tenantID,
			Subject: created.ID.String(), Actor: delegatorID.String(),
			Data: domain.DelegationStartedPayload{
				DelegationID: created.ID, TenantID: tenantID,
				DelegatorID: delegatorID, DelegateID: req.DelegateID,
				Scope: scope, ScopeID: req.ScopeID, EndsAt: req.EndsAt,
				ActorID: delegatorID,
			},
		})
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
		return pub.EnqueueCtx(txCtx, &domain.DomainEvent{
			Type: domain.EventDelegationEnded, TenantID: tenantID,
			Subject: id.String(), Actor: d.DelegatorID.String(),
			Data: domain.DelegationEndedPayload{
				DelegationID: id, TenantID: tenantID,
				DelegatorID: d.DelegatorID, DelegateID: d.DelegateID,
				Scope: d.Scope, ScopeID: d.ScopeID,
				EndedReason: domain.EndReasonCancelled,
				ActorID:     d.DelegatorID,
			},
		})
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
	Reason     string
	StartsAt   *time.Time
	EndsAt     *time.Time
}
