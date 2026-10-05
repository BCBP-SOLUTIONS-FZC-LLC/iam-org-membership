package consumer

import (
	"context"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
)

// dedupStore is the probe/record pair the unit-test stubs implement.
type dedupStore interface {
	IsProcessed(ctx context.Context, consumer, eventID string) (bool, error)
	MarkProcessed(ctx context.Context, consumer, eventID string) error
}

// fakeInbox is a port.Inbox for unit tests: a duplicate check, then fn and
// the record inside one tx-runner call — the same observable contract as
// postgres.InboxRepository (claim and fn commit together; a duplicate skips
// fn; an fn error records nothing).
type fakeInbox struct {
	tx    port.TxRunner
	dedup dedupStore
}

// newFakeInbox returns nil when both parts are nil, so tests that never
// reach the inbox keep passing a nil dependency.
func newFakeInbox(tx port.TxRunner, dedup dedupStore) port.Inbox {
	if tx == nil && dedup == nil {
		return nil
	}
	if tx == nil {
		tx = stubTx{}
	}
	return &fakeInbox{tx: tx, dedup: dedup}
}

func (f *fakeInbox) ProcessOnce(ctx context.Context, consumer, eventID, _ string, fn func(context.Context) error) (bool, error) {
	if f.dedup != nil {
		seen, err := f.dedup.IsProcessed(ctx, consumer, eventID)
		if err != nil {
			return false, err
		}
		if seen {
			return true, nil
		}
	}
	return false, f.tx.RunInTx(ctx, func(txCtx context.Context) error {
		if err := fn(txCtx); err != nil {
			return err
		}
		if f.dedup == nil {
			return nil
		}
		if err := f.dedup.MarkProcessed(txCtx, consumer, eventID); err != nil {
			return fmt.Errorf("mark processed: %w", err)
		}
		return nil
	})
}
