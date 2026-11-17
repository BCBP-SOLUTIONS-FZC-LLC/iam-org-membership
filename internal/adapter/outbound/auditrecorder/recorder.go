// Package auditrecorder implements port.AuditRecorder on platform-audit's
// transactional outbox (pkg/auditoutbox): entries are written into
// pending_audit_entries on the caller's transaction and delivered to
// iam-audit-log's AL-5 by the auditoutbox.Runner that cmd/server runs on
// the BYPASSRLS sysPool.
//
// It replaces the old fire-and-forget AL-5 HTTP client: an entry now exists
// if and only if the audited change committed (PA-INV-3), and an Audit Log
// outage only grows the backlog.
package auditrecorder

import (
	"context"
	"errors"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/audit"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
)

// ErrNoTransaction is returned when Record runs outside TxRunner.RunInTx.
var ErrNoTransaction = errors.New("auditrecorder: no transaction in context (call inside TxRunner.RunInTx)")

// Recorder is the port.AuditRecorder over an *auditoutbox.Enqueuer.
type Recorder struct {
	enq *auditoutbox.Enqueuer
}

var _ port.AuditRecorder = (*Recorder)(nil)

// New wraps enq. Build one Enqueuer per process in main.
func New(enq *auditoutbox.Enqueuer) *Recorder {
	return &Recorder{enq: enq}
}

// Record implements port.AuditRecorder.
func (r *Recorder) Record(ctx context.Context, e port.AuditEntry) error {
	tx, ok := port.TxFromContext(ctx)
	if !ok {
		return ErrNoTransaction
	}
	entry, err := toEntry(e)
	if err != nil {
		return err
	}
	return r.enq.Enqueue(ctx, tx, entry, e.IdempotencyKey)
}

// RecordBatch implements port.AuditRecorder.
func (r *Recorder) RecordBatch(ctx context.Context, entries []port.AuditEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, ok := port.TxFromContext(ctx)
	if !ok {
		return ErrNoTransaction
	}
	items := make([]auditoutbox.Item, 0, len(entries))
	for _, e := range entries {
		entry, err := toEntry(e)
		if err != nil {
			return err
		}
		items = append(items, auditoutbox.Item{Entry: entry, Key: e.IdempotencyKey})
	}
	return r.enq.EnqueueBatch(ctx, tx, items)
}

// toEntry maps the port entry onto audit.Entry. Validation (UUID actor ids,
// key length, metadata size) is the Enqueuer's; only a metadata encoding
// failure, a programming error, is returned here.
func toEntry(e port.AuditEntry) (audit.Entry, error) {
	out := audit.Entry{
		TenantID:        e.TenantID,
		EntryType:       e.EntryType,
		Action:          e.Action,
		Actor:           audit.Actor{Type: audit.ActorType(e.Actor.Type), ID: e.Actor.ID},
		OccurredAt:      e.OccurredAt,
		SourceEventType: e.SourceEventType,
		IPAddress:       e.IPAddress,
		UserAgent:       e.UserAgent,
	}
	if e.Target != nil {
		out.Target = &audit.Target{Type: e.Target.Type, ID: e.Target.ID}
	}
	if len(e.Metadata) > 0 {
		md, err := audit.Metadata(e.Metadata)
		if err != nil {
			return audit.Entry{}, fmt.Errorf("auditrecorder: %s metadata: %w", e.EntryType, err)
		}
		out.Metadata = md
	}
	return out, nil
}
