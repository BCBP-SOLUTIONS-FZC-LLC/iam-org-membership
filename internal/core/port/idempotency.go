package port

import (
	"context"
	"time"
)

// Inbox is the exactly-once consumer port backing the processed_events
// table (§9.2 IDEMP-1..4), implemented over platform-events' pkg/inbox.
//
// ProcessOnce claims (consumer, eventID) and runs fn in ONE transaction:
// the claim (INSERT … ON CONFLICT DO NOTHING) is the first statement, so a
// concurrent delivery of the same event blocks on the claim's row lock and
// then sees it as a duplicate, instead of both deliveries passing a
// pre-check and applying the projection twice. fn's ctx carries that
// transaction (and the tx-bound event publisher), so every repository call
// and outbox enqueue inside fn joins it; the EVT-14 recency guard's row lock,
// the projection, the EVT-16 relay and the claim commit or roll back
// together. If fn returns an error nothing is recorded and the message is
// retried.
//
// duplicate is true when the event was already processed; fn was not run.
// eventID must be a UUID (every platform-events envelope ID is).
type Inbox interface {
	ProcessOnce(ctx context.Context, consumer, eventID, eventType string, fn func(txCtx context.Context) error) (duplicate bool, err error)
}

// InboxPruner deletes one consumer's processed_events rows older than
// retention, in batches of batch rows until none are left (PE-1).
type InboxPruner interface {
	Prune(ctx context.Context, consumer string, retention time.Duration, batch int) (int64, error)
}
