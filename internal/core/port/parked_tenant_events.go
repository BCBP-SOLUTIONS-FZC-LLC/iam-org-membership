package port

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ParkedTenantEvent is a lifecycle/billing event that reached the consumer
// before its tenant row existed (CD-50, EVT-18). Envelope holds the event
// exactly as consumed, so a replay runs the same projection the original
// delivery would have.
type ParkedTenantEvent struct {
	EventID    uuid.UUID
	TenantID   uuid.UUID
	EventType  string
	OccurredAt time.Time
	Envelope   []byte
	ParkedAt   time.Time
}

// ParkedTenantEventStore is the pending_tenant_events table. Every method
// runs under the tenant's own RLS GUC and joins the caller's transaction
// when ctx carries one.
type ParkedTenantEventStore interface {
	// Park stores ev. A redelivery of an already-parked event is a no-op.
	Park(ctx context.Context, ev ParkedTenantEvent) error

	// ListForTenant returns the tenant's parked events in replay order
	// (occurred_at, then event_id).
	ListForTenant(ctx context.Context, tenantID uuid.UUID) ([]ParkedTenantEvent, error)

	// Claim deletes one parked event inside the caller's transaction and
	// reports whether this caller removed it. false means another replay
	// already took it.
	Claim(ctx context.Context, eventID uuid.UUID) (bool, error)

	// LockTenantProvisioning takes a transaction-scoped advisory lock on the
	// tenant id. I-1 holds it while creating the row and the consumer holds
	// it while deciding to park, so an event can never be parked after I-1's
	// replay has already run.
	LockTenantProvisioning(ctx context.Context, tenantID uuid.UUID) error
}

// ParkedTenantEventReplayer applies a tenant's parked events now that its
// row exists. Implemented by the event consumer; called by I-1 right after
// it creates a tenant and by the parked-events-replay CronJob.
type ParkedTenantEventReplayer interface {
	ReplayParked(ctx context.Context, tenantID uuid.UUID) (replayed int, err error)
}
