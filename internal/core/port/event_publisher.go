package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
)

// EventPublisher enqueues events into the transactional outbox. The insert
// MUST share the same transaction as the business write so state and event
// commit atomically (EVT-10, CONS-1..4).
//
// Callers never pass a pgx.Tx. TxRunner.RunInTx stores the open transaction
// on ctx; the eventbus adapter reads it via postgres.TxFromContext and
// writes outbox_events on that tx.
type EventPublisher interface {
	Enqueue(ctx context.Context, event *domain.DomainEvent) error
}

type contextPublisherKey struct{}

// EventPublisherFromContext retrieves the publisher injected by
// TxRunner.RunInTx.
func EventPublisherFromContext(ctx context.Context) (EventPublisher, bool) {
	p, ok := ctx.Value(contextPublisherKey{}).(EventPublisher)
	return p, ok
}

// WithEventPublisher stores a publisher in ctx. Production TxRunner injects
// the eventbus Publisher after attaching the running tx; tests inject fakes.
func WithEventPublisher(ctx context.Context, p EventPublisher) context.Context {
	return context.WithValue(ctx, contextPublisherKey{}, p)
}
