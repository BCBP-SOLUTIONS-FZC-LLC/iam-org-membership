package service

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
)

// SeatOverageTransition is the outcome of ReevaluateSeatOverage.
type SeatOverageTransition int

const (
	// SeatOverageUnchanged: still over cap (marker kept) or still at/under
	// cap (marker stays NULL).
	SeatOverageUnchanged SeatOverageTransition = iota
	// SeatOverageStarted: usage first exceeded licensed_seats; overage_since
	// was set and TenantSeatOverageStarted enqueued.
	SeatOverageStarted
	// SeatOverageResolved: usage came back to at/under licensed_seats;
	// overage_since was cleared and TenantSeatOverageResolved enqueued.
	SeatOverageResolved
)

// SeatOverageSource names who triggered a re-evaluation, for the event's
// audit fields.
type SeatOverageSource struct {
	Actor     string // user id, or "iam-system" for consumers and jobs
	IPAddress string
	UserAgent string
}

// ReevaluateSeatOverage is SEAT-5: it locks the tenants row (FOR UPDATE),
// recomputes active members + unexpired pending invitations against
// licensed_seats, and moves the overage_since marker:
//
//	usage > licensed_seats and overage_since NULL     → set now(), emit TenantSeatOverageStarted
//	usage ≤ licensed_seats and overage_since not NULL → clear,     emit TenantSeatOverageResolved
//
// It MUST run inside the caller's RunInTx so the marker change and its event
// commit together with the mutation that caused it (EVT-10). Callers: every
// TenantSeatsChanged consume, P-8 / I-5 member removal, P-31 revoke, the I-3
// plain add, and the seat-overage-reconcile backstop job. The
// SeatOverageStarted metric is counted by the caller after commit (see
// CountSeatOverageStarted).
func ReevaluateSeatOverage(txCtx context.Context, tenants port.TenantRepository, tenantID uuid.UUID, src SeatOverageSource) (SeatOverageTransition, error) {
	if tenants == nil {
		return SeatOverageUnchanged, nil // collaborator not wired (unit-test constructions)
	}
	occ, err := tenants.LockSeatOccupancy(txCtx, tenantID)
	if err != nil {
		return SeatOverageUnchanged, err
	}
	overCap := occ.Active+occ.Pending > occ.LicensedSeats
	pub, _ := port.EventPublisherFromContext(txCtx)
	now := time.Now().UTC()

	switch {
	case overCap && occ.OverageSince == nil:
		if err := tenants.SetOverageSince(txCtx, tenantID, &now); err != nil {
			return SeatOverageUnchanged, err
		}
		if pub != nil {
			if err := pub.Enqueue(txCtx, &domain.DomainEvent{
				Type: domain.EventTenantSeatOverageStarted, TenantID: tenantID,
				Subject: tenantID.String(), Actor: src.Actor,
				IPAddress: src.IPAddress, UserAgent: src.UserAgent,
				Data: domain.TenantSeatOverageStartedPayload{
					TenantID: tenantID, LicensedSeats: occ.LicensedSeats,
					ActiveUsers: occ.Active, PendingInvitations: occ.Pending, OverageSince: now,
				},
			}); err != nil {
				return SeatOverageUnchanged, err
			}
		}
		return SeatOverageStarted, nil
	case !overCap && occ.OverageSince != nil:
		if err := tenants.SetOverageSince(txCtx, tenantID, nil); err != nil {
			return SeatOverageUnchanged, err
		}
		if pub != nil {
			if err := pub.Enqueue(txCtx, &domain.DomainEvent{
				Type: domain.EventTenantSeatOverageResolved, TenantID: tenantID,
				Subject: tenantID.String(), Actor: src.Actor,
				IPAddress: src.IPAddress, UserAgent: src.UserAgent,
				Data: domain.TenantSeatOverageResolvedPayload{TenantID: tenantID, ResolvedAt: now},
			}); err != nil {
				return SeatOverageUnchanged, err
			}
		}
		return SeatOverageResolved, nil
	default:
		return SeatOverageUnchanged, nil
	}
}

// CountSeatOverageStarted records a committed SeatOverageStarted transition.
// Call it after the transaction commits, so a rolled-back or retried
// transition is not counted.
func CountSeatOverageStarted(t SeatOverageTransition) {
	if t == SeatOverageStarted {
		metrics.IncSeatOverageStarted(metrics.SeatOverageCauseReconciler)
	}
}
