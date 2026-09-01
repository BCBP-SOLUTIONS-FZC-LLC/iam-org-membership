// Package main is the single-binary reconciler entry point (LLD §13.1).
// Selects a job by --job flag or RECONCILER_JOB env var and dispatches to
// the matching handler. Each of the 7 CronJobs in
// deploy/helm/templates/cronjobs.yaml invokes this binary with a
// different --job value.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/cmd/reconciler/jobs"
	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/eventbus"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	realmprovisionerclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/realmprovisioner"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/logger"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

var registry = map[string]jobs.Func{
	"invitation-expiry":      jobs.InvitationExpiry,
	"invitation-kc-cleanup":  jobs.InvitationKCCleanup,
	"realm-config-sync":      jobs.RealmConfigSync,
	"seat-overage-reconcile": jobs.SeatOverageReconcile,
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

	// Same Zap-backed Logger as cmd/server/main.go — every reconciler job's
	// logs flow through the identical gincommon sink instead of slog.Default().
	rawLog, err := logger.NewLogger(envOr("APP_ENV", "dev"))
	if err != nil {
		panic("init logger: " + err.Error())
	}
	log := port.NewSlogStyleLogger(rawLog)

	log.Info("reconciler starting", "job", jobName)

	timeout := 5 * time.Minute
	if s := os.Getenv("RECONCILER_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			timeout = d
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// pgcommon.ConfigFromEnv reads DATABASE_URL/PG_* directly — same source
	// of truth cmd/server/main.go uses, so pool sizing/DSN assembly never
	// drifts between the two binaries.
	pgCfg, pgWarnings := pgcommon.ConfigFromEnv()
	for _, w := range pgWarnings {
		log.Warn("postgres config warning", "key", w.Key, "reason", w.Reason)
	}
	pgCfg.DSN = pgadapter.DSNFromEnv()
	pgCfg.GUCProvider = pgcommon.GUCSetFromContext
	pgCfg.Logger = pgadapter.NewLoggerAdapter(rawLog)
	pool, err := pgcommon.NewPool(ctx, pgCfg)
	if err != nil {
		die("connect to postgres: %v", err)
	}
	defer pool.Close()

	// *pgcommon.Pool (not a raw pgxpool.Pool), deliberately with no
	// GUCProvider — same rationale as cmd/server/main.go's sysPool — wired
	// with the same Logger so slow cross-tenant queries are traced through
	// the same structured sink instead of nowhere.
	sysPool, err := pgcommon.NewPool(ctx, pgcommon.Config{
		DSN:    pgadapter.SystemDSNFromEnv(),
		Logger: pgadapter.NewLoggerAdapter(rawLog),
	})
	if err != nil {
		die("connect sysPool: %v", err)
	}
	defer sysPool.Close()

	rawCodec := eventbusadapter.Codec(eventbusadapter.NoopCodec{})
	codec, err := eventbusadapter.NewValidatingCodec(rawCodec)
	if err != nil {
		die("init validating codec: %v", err)
	}
	outboxPublisher := eventbusadapter.New("iam-org-membership-reconciler", codec).WithLogger(rawLog)

	jctx := &jobs.Context{
		Pool:                   pool,
		SysPool:                sysPool,
		OutboxPublisher:        outboxPublisher,
		TxRunner:               pgadapter.NewTxRunner(pool, outboxPublisher),
		RealmProvisioner:       realmprovisionerclient.New(rawLog),
		Logger:                 log,
		BatchLimit:             envInt("RECONCILER_BATCH_LIMIT", 500),
		SeatOverageGraceDays:   envInt("SEAT_OVERAGE_GRACE_DAYS", 30),
		TrialGraceDays:         envInt("TRIAL_GRACE_DAYS", 15),
		OutboxRetentionDays:    envInt("OUTBOX_RETENTION_DAYS", 8),
		ProcessedEventsTTLDays: envInt("PROCESSED_EVENTS_TTL_DAYS", 8),
	}

	res, err := fn(ctx, jctx)
	if err != nil {
		log.Error("reconciler job failed", "job", jobName, "error", err.Error())
		_ = gincommon.Shutdown(rawLog)
		os.Exit(1)
	}
	log.Info("reconciler complete", "job", jobName,
		"attempted", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed, "skipped", res.Skipped)
	_ = gincommon.Shutdown(rawLog)
}

func registryNames() []string {
	names := make([]string, 0, len(registry))
	for k := range registry {
		names = append(names, k)
	}
	return names
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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
