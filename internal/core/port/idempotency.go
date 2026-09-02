package port

import "context"

// IdempotencyStore is the persistence port backing the processed_events
// table (§9.2 IDEMP-2/4), named and shaped to mirror iam-user-profile's
// port.IdempotencyStore.
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
// delivery could race past it. MarkProcessed therefore joins the caller's
// already-open transaction (via TxRunner + withPool) so the dedup write
// commits or rolls back atomically with the row lock and the projection
// update — a decoupled, post-hoc mark is not sufficient here.
type IdempotencyStore interface {
	// IsProcessed reports whether eventID has already been recorded as
	// processed by consumer. A standalone read, not tied to any caller
	// transaction — used as a cheap pre-check to skip a lock acquisition
	// on an obvious replay before the main transaction opens.
	IsProcessed(ctx context.Context, consumer, eventID string) (bool, error)

	// MarkProcessed records eventID as processed by consumer. When called
	// inside TxRunner.RunInTx the write joins that transaction so it
	// commits or rolls back atomically with the projection. Safe to call
	// more than once for the same (consumer, eventID) pair
	// (ON CONFLICT DO NOTHING).
	MarkProcessed(ctx context.Context, consumer, eventID string) error
}
