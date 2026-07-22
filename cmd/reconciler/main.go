// Package main is the single-binary reconciler entry point (LLD §13.1).
// Selects a job by --job flag or RECONCILER_JOB env var and dispatches to
// the matching handler. Each of the 8 CronJobs in
// deploy/helm/templates/cronjobs.yaml invokes this binary with a
// different --job value.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/cmd/reconciler/jobs"
	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/eventbus"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	realmprovisionerclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/realmprovisioner"
	userprofileclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/userprofile"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/jackc/pgx/v5/pgxpool"
)

var registry = map[string]jobs.Func{
	"invitation-expiry":      jobs.InvitationExpiry,
	"invitation-kc-cleanup":  jobs.InvitationKCCleanup,
	"realm-config-sync":      jobs.RealmConfigSync,
	"seat-overage-reconcile": jobs.SeatOverageReconcile,
	"delegation-expiry":      jobs.DelegationExpiry,
	"trial-cleanup":          jobs.TrialCleanup,
	"outbox-prune":           jobs.OutboxPrune,
	"processed-events-prune": jobs.ProcessedEventsPrune,
}

func main() {
	var jobName string
	flag.StringVar(&jobName, "job", os.Getenv("RECONCILER_JOB"), "reconciler job code")
	flag.Parse()

	if jobName == "" {
		die("no job specified — pass --job=<name> or set RECONCILER_JOB")
	}
	fn, ok := registry[jobName]
	if !ok {
		die("unknown job %q — valid: %v", jobName, registryNames())
	}

	slog.Info("reconciler starting", "job", jobName)

	timeout := 5 * time.Minute
	if s := os.Getenv("RECONCILER_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			timeout = d
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	pool, err := pgcommon.NewPool(ctx, pgcommon.Config{
		DSN:           pgadapter.DSNFromEnv(),
		MaxConns:      4,
		MinConns:      0,
		PGBouncerMode: os.Getenv("PG_BOUNCER_MODE") == "true",
		GUCProvider:   pgcommon.GUCSetFromContext,
	})
	if err != nil {
		die("connect to postgres: %v", err)
	}
	defer pool.Close()

	sysPool, err := pgxpool.New(ctx, pgadapter.SystemDSNFromEnv())
	if err != nil {
		die("connect sysPool: %v", err)
	}
	defer sysPool.Close()

	rawCodec := eventbusadapter.Codec(eventbusadapter.NoopCodec{})
	codec, err := eventbusadapter.NewValidatingCodec(rawCodec)
	if err != nil {
		die("init validating codec: %v", err)
	}
	outboxPublisher := eventbusadapter.New("iam-org-membership-reconciler", codec)

	jctx := &jobs.Context{
		Pool:                   pool,
		SysPool:                sysPool,
		OutboxPublisher:        outboxPublisher,
		TxRunner:               pgadapter.NewTxRunner(pool, outboxPublisher),
		RealmProvisioner:       realmprovisionerclient.New(),
		UserProfile:            userprofileclient.New(),
		Logger:                 slog.Default(),
		BatchLimit:             envInt("RECONCILER_BATCH_LIMIT", 500),
		SeatOverageGraceDays:   envInt("SEAT_OVERAGE_GRACE_DAYS", 30),
		TrialGraceDays:         envInt("TRIAL_GRACE_DAYS", 15),
		OutboxRetentionDays:    envInt("OUTBOX_RETENTION_DAYS", 8),
		ProcessedEventsTTLDays: envInt("PROCESSED_EVENTS_TTL_DAYS", 8),
	}

	res, err := fn(ctx, jctx)
	if err != nil {
		slog.Error("reconciler job failed", "job", jobName, "error", err.Error())
		os.Exit(1)
	}
	slog.Info("reconciler complete", "job", jobName,
		"attempted", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed, "skipped", res.Skipped)
}

func registryNames() []string {
	names := make([]string, 0, len(registry))
	for k := range registry {
		names = append(names, k)
	}
	return names
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
