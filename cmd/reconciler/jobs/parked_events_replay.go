package jobs

import (
	"context"
	"fmt"
)

// ParkedEventsReplay is the EVT-18 backstop (CD-50). I-1 replays a tenant's
// parked events as soon as it creates the row; this sweep replays the ones
// left behind — a crash between I-1's commit and its replay, or a replay
// that failed. It only visits tenants whose row now exists; events for a
// tenant that never got one stay parked and show up on the
// iam_org_membership_parked_tenant_events_stale gauge.
func ParkedEventsReplay(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	if jctx.ParkedReplayer == nil {
		jctx.Logger.Warn("parked-events-replay: no replayer wired — skipping")
		return res, nil
	}
	tenantIDs, err := jctx.Reconciler.ListTenantsWithParkedEvents(ctx, jctx.BatchLimit)
	if err != nil {
		return res, fmt.Errorf("list tenants with parked events: %w", err)
	}
	for _, tenantID := range tenantIDs {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Attempted++
		n, err := jctx.ParkedReplayer.ReplayParkedSweep(ctx, tenantID)
		if err != nil {
			res.Failed++
			jctx.Logger.Warn("parked-events-replay: replay failed — next run retries",
				"tenant_id", tenantID, "replayed", n, "error", err.Error())
			continue
		}
		res.Succeeded++
	}
	jctx.Logger.Info("parked-events-replay complete",
		"tenants", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed)
	return res, nil
}
