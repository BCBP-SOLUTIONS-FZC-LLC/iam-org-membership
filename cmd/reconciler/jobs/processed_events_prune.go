package jobs

import (
	"context"
	"time"
)

// ProcessedEventsPrune deletes processed_events rows older than the
// retention window (PE-1: default 8 days, strictly > 7-day SQS lifetime;
// IDEMP-4 backstopped by EVT-14/PI-10/IDEMP-3 for beyond-window duplicates)
// through platform-events' inbox.Store.Prune, once per consumer that
// records there. Prune deletes in batches of jctx.BatchLimit and loops until
// nothing expired is left, so a backlog never outgrows the daily run.
func ProcessedEventsPrune(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	if jctx.Inbox == nil {
		jctx.Logger.Warn("processed-events-prune: no inbox wired — skipping")
		return res, nil
	}
	ttlDays := jctx.ProcessedEventsTTLDays
	if ttlDays <= 0 {
		ttlDays = 8
	}
	retention := time.Duration(ttlDays) * 24 * time.Hour
	for _, consumer := range jctx.InboxConsumers {
		n, err := jctx.Inbox.Prune(ctx, consumer, retention, jctx.BatchLimit)
		res.Attempted += int(n)
		res.Succeeded += int(n)
		if err != nil {
			return res, err
		}
	}
	jctx.Logger.Info("processed-events-prune complete",
		"deleted", res.Succeeded, "ttl_days", ttlDays, "consumers", len(jctx.InboxConsumers))
	return res, nil
}
