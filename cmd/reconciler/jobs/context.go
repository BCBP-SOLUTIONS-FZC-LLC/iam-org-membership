// Package jobs holds the concrete reconciler bodies for each of the 8
// CronJobs (§13.1). Every job is package-visible so the top-level
// cmd/reconciler dispatcher can wire it into the --job registry without
// touching implementation details.
//
// Each job returns a Result summarising what happened (metrics + human
// output) plus an error. A non-nil error exits the process 1 for K8s to
// mark the CronJob run as failed; per-row errors accumulate into
// Result.Failed and are logged but do not fail the run (idempotency +
// safe-under-restart via §13.1 next-tick convergence).
package jobs

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// Context is the dependency bag every reconciler function accepts.
// Populated by cmd/reconciler/main.go from env + Postgres/AWS clients.
type Context struct {
	Pool             *pgcommon.Pool
	SysPool          *pgcommon.Pool              // BYPASSRLS pool (§4.4) — no GUCProvider, sees across every tenant
	OutboxPublisher  port.EventPublisher         // for jobs that emit events (SEAT-5 pair, DEL-7 delegate_removed)
	TxRunner         port.TxRunner               // for atomic state + event emit
	RealmProvisioner port.RealmProvisionerClient // PI-9 DeleteUser, T-15 PatchRealmConfig
	// Logger is the shared gincommon-backed Logger, wrapped for slog-style
	// call sites (Warn/Info(msg, "key", val, ...)) — every job file calls it
	// exactly as it called *slog.Logger before this switch.
	Logger port.SlogStyleLogger

	BatchLimit             int
	SeatOverageGraceDays   int // SEAT-5 grace_ends_at = overage_since + N days
	TrialGraceDays         int // trial-cleanup hard-delete threshold
	OutboxRetentionDays    int // outbox-prune window
	ProcessedEventsTTLDays int // PE-1 8-day retention (default)
}

// Result summarises a reconciler run.
type Result struct {
	Attempted int // rows the job examined
	Succeeded int // rows that converged
	Failed    int // rows that errored (bounded by BatchLimit; next tick retries)
	Skipped   int // rows already in terminal state (idempotent no-op)
}

// Func is the signature every job satisfies.
type Func func(ctx context.Context, jctx *Context) (Result, error)
