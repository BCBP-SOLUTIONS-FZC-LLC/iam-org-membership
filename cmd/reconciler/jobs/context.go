// Package jobs holds the concrete reconciler bodies for each of the 7
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
)

// Metrics is the minimal recorder seam reconciler jobs need for counters
// that must be incremented on failure, not just registered — same shape as
// iam-delegation's jobs.Context.Metrics. Satisfied by metrics.Recorder.
// Optional: nil means metrics are skipped, so existing callers/tests that
// don't wire it keep working.
type Metrics interface {
	IncRealmSyncFailed(stage string)
}

// Context is the dependency bag every reconciler function accepts.
// Populated by cmd/reconciler/main.go from env + Postgres/AWS clients.
// Pools stay in main.go — jobs talk to repositories, TxRunner, and
// RealmProvisioner. Seat-overage events go through TxRunner's ctx publisher.
type Context struct {
	TxRunner         port.TxRunner               // for atomic state + event emit
	Tenants          port.TenantRepository       // SEAT-5 occupancy lock + overage_since (app pool)
	Invitations      port.InvitationRepository   // invitation-expiry / kc-cleanup (sysPool)
	Reconciler       port.ReconcilerStore        // cross-tenant sweeps (sysPool)
	RealmProvisioner port.RealmProvisionerClient // PI-9 DeleteUser, T-15 PatchRealmConfig
	// Logger is the shared gincommon-backed Logger, wrapped for slog-style
	// call sites (Warn/Info(msg, "key", val, ...)) — every job file calls it
	// exactly as it called *slog.Logger before this switch.
	Logger port.SlogStyleLogger
	// Metrics is nil-checked at every call site — see Metrics doc comment.
	Metrics Metrics

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
