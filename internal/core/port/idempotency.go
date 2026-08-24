package port

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// IdempotencyStore is the persistence port backing the processed_events
// table (§9.2 IDEMP-2/4), named and shaped to mirror iam-user-profile's
// port.IdempotencyStore — with one deliberate difference in MarkProcessedInTx.
//
// iam-user-profile's store is decoupled from the caller's transaction by
// design: IsProcessed and MarkProcessed are two independent calls, and
// MarkProcessed is invoked only after the business side effect has fully
// succeeded (check-then-act, mark-last). That is safe there because the
// only consumer's handler (ScrubTenant) is itself idempotent, so a crash
// between the business write and the mark simply leaves the event
// unmarked for a safe redelivery.
//
// This service's tenant-lifecycle consumer additionally runs the EVT-14
// recency guard: a row-locked comparison of the event's timestamp against
// tenants.last_event_at, which must observe a consistent view alongside
// the projection update and the dedup write, or a concurrent/out-of-order
// delivery could race past it. MarkProcessedInTx therefore takes the
// caller's already-open pgx.Tx so the dedup write commits or rolls back
// atomically with the row lock and the projection update in the same
// transaction — a decoupled, post-hoc mark (as IsProcessed/MarkProcessed
// alone would give) is not sufficient here.
type IdempotencyStore interface {
	// IsProcessed reports whether eventID has already been recorded as
	// processed by consumer. A standalone read, not tied to any caller
	// transaction — used as a cheap pre-check to skip a lock acquisition
	// on an obvious replay before the main transaction opens.
	IsProcessed(ctx context.Context, consumer, eventID string) (bool, error)

	// MarkProcessedInTx records eventID as processed by consumer inside
	// tx, so the write commits or rolls back atomically with whatever
	// business state change tx also contains. Safe to call more than once
	// for the same (consumer, eventID) pair (ON CONFLICT DO NOTHING).
	MarkProcessedInTx(ctx context.Context, tx pgx.Tx, consumer, eventID string) error
}
