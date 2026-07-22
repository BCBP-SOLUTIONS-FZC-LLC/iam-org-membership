// Package port declares the interfaces the service layer requires from its
// adapters. Everything in this package must depend only on core/domain and
// stdlib/third-party value types — never on concrete adapters.
package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/jackc/pgx/v5"
)

// EventPublisher enqueues events into the transactional outbox using the
// caller's active pgx.Tx. The insert MUST share the same transaction as the
// business write so state and event commit atomically (EVT-10, CONS-1..4).
type EventPublisher interface {
	Enqueue(ctx context.Context, tx pgx.Tx, event *domain.DomainEvent) error
}
