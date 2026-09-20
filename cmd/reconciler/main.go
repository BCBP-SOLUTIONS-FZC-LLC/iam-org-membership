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
	"sync"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/cmd/reconciler/jobs"
	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	realmprovisionerclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/realmprovisioner"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/logger"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgmetrics"
	"go.opentelemetry.io/otel"
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
	os.Exit(run())
}

// run holds every deferred cleanup (pool drain, tracer/logger flush, span
// end) so every exit path — success or failure — runs it via a normal
// return, rather than an os.Exit call that would skip it. main() only ever
// calls os.Exit once, on run()'s own return value, after every defer here
// has already fired.
func run() int {
	var jobName string
	flag.StringVar(&jobName, "job", os.Getenv("RECONCILER_JOB"), "reconciler job code")
	flag.Parse()

	if jobName == "" {
		fmt.Fprintln(os.Stderr, "no job specified — pass --job=<name> or set RECONCILER_JOB")
		return 1
	}
	fn, ok := registry[jobName]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown job %q — valid: %v\n", jobName, registryNames())
		return 1
	}

	appEnv := envOr("APP_ENV", "dev")

	// Same Zap-backed Logger as cmd/server/main.go — every reconciler job's
	// logs flow through the identical gincommon sink instead of slog.Default().
	rawLog, err := logger.NewLogger(appEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "init logger: "+err.Error())
		return 1
	}
	log := port.NewSlogStyleLogger(rawLog)

	// SYSTEM_DATABASE_URL unset means the sysPool built below silently
	// reuses the RLS-scoped app DSN, so every cross-tenant sweep this
	// binary runs (ListRealmSyncPending, seat-overage/trial-cleanup scans,
	// I-16-adjacent reads) RLS-filters to zero rows instead of erroring —
	// no alert fires. Fail fast outside dev rather than degrade silently;
	// mirrors cmd/server/main.go's validateRequiredEnv SYSTEM_DATABASE_URL
	// entry so both binaries enforce the same invariant.
	if os.Getenv("SYSTEM_DATABASE_URL") == "" && !isDevLikeEnv(appEnv) {
		log.Error("SYSTEM_DATABASE_URL is required outside dev — must be the BYPASSRLS org_membership_migrator role, or cross-tenant reconciler sweeps silently RLS-filter to zero rows with no alert",
			"app_env", appEnv, "job", jobName)
		return 1
	}

	// Distinct from cmd/server's "iam-org-membership" default so reconciler
	// spans/metrics/logs are attributable to this binary, not the HTTP
	// server, on a shared {service} dashboard.
	serviceName := envOr("APP_NAME", "iam-org-membership-reconciler")
	buildVersion := envOr("BUILD_VERSION", "dev")

	// Same TracerProvider as cmd/server so db.query spans from this job
	// export through gincommon's OTLP pipeline when the collector is set.
	shutdownTracing := gincommon.InitTracingFromEnv()
	// Registered in reverse of execution order (defers run LIFO): this
	// gincommon.Shutdown defer, registered FIRST, fires LAST — so the
	// TracerProvider shuts down (below) before the Zap flush, matching
	// cmd/server/main.go's explicit shutdownTracing() → gincommon.Shutdown()
	// sequence (same order iam-user-profile uses).
	defer func() {
		if err := gincommon.Shutdown(rawLog); err != nil {
			log.Error("logger/tracer flush error", "error", err.Error())
		}
	}()
	defer shutdownTracing()

	// ObservabilityMiddlewares is gincommon's public metrics-init API —
	// call it here (mirrors cmd/server/main.go) so business / events /
	// pgcommon metrics land on gincommon.MetricsRegisterer with matching
	// {service, version} const labels. This CronJob never serves /metrics;
	// collectors still have to exist because jobs increment them.
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
		Logger:       rawLog,
		ServiceName:  serviceName,
		BuildVersion: buildVersion,
	})
	metrics.Register(appEnv)
	events.InitWithRegisterer(serviceName, buildVersion, gincommon.MetricsRegisterer())
	pgmetrics.InitWithRegisterer(serviceName, buildVersion, gincommon.MetricsRegisterer())

	log.Info("reconciler starting", "job", jobName)

	timeout := 5 * time.Minute
	if s := os.Getenv("RECONCILER_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			timeout = d
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ctx, jobSpan := otel.Tracer(serviceName).Start(ctx, "reconciler."+jobName)
	defer jobSpan.End()

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
	pgCfg.Tracer = pgadapter.NewOTelTracer(serviceName)
	pool, err := pgcommon.NewPool(ctx, pgCfg)
	if err != nil {
		log.Error("connect to postgres", "error", err.Error())
		return 1
	}

	// DrainAndClose is pgcommon's graceful path (wait for in-flight
	// WithConn/RunInTx, then close). sync.Once keeps DrainAndClose and a
	// later defer from running concurrently (pgcommon forbids that). This
	// defer is the ONLY place drainPools runs — every exit path below
	// (success or job failure) is a plain `return`, not os.Exit, so the
	// defer chain always fires.
	var sysPool *pgcommon.Pool
	var drainOnce sync.Once
	drainPools := func() {
		drainOnce.Do(func() {
			drainCtx, cancelDrain := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancelDrain()
			if err := pool.DrainAndClose(drainCtx); err != nil {
				log.Error("app pool drain error", "error", err.Error())
			}
			if sysPool != nil {
				if err := sysPool.DrainAndClose(drainCtx); err != nil {
					log.Error("sysPool drain error", "error", err.Error())
				}
			}
		})
	}
	defer drainPools()

	// *pgcommon.Pool via SystemPoolConfig — same helper as cmd/server
	// (no GUCProvider, PGBouncerMode forced true, ConfigFromEnv pool
	// sizing). Tracer is wired separately so db.query spans export
	// through gincommon's TracerProvider.
	sysCfg := pgadapter.SystemPoolConfig(pgadapter.SystemDSNFromEnv(), rawLog)
	sysCfg.Tracer = pgadapter.NewOTelTracer(serviceName)
	sysPool, err = pgcommon.NewPool(ctx, sysCfg)
	if err != nil {
		log.Error("connect sysPool", "error", err.Error())
		return 1
	}

	rawCodec := eventbusadapter.Codec(eventbusadapter.NoopCodec{})
	codec, err := eventbusadapter.NewValidatingCodec(rawCodec)
	if err != nil {
		log.Error("init validating codec", "error", err.Error())
		return 1
	}
	outboxPublisher := eventbusadapter.New("iam-org-membership-reconciler", codec).WithLogger(rawLog)

	// Prune-only runner: LoadOutbox + RunnerConfigFromEnv is the
	// platform-events composition contract. Publisher is NoopPublisher
	// because this binary never Start()s the poll loop — OutboxPrune only
	// calls PrunePublished against the outbox_events schema ApplySchema
	// created at server startup.
	outboxEnv := eventcfg.LoadOutbox()
	eventcfg.LogWarningsTo(rawLog, outboxEnv.Warnings)
	outboxRunner, err := outbox.NewRunner(eventcfg.RunnerConfigFromEnv(outboxEnv, sysPool, eventbusadapter.NoopPublisher{}, rawLog))
	if err != nil {
		log.Error("create outbox runner", "error", err.Error())
		return 1
	}

	jctx := &jobs.Context{
		TxRunner:               pgadapter.NewTxRunner(pool, outboxPublisher),
		Tenants:                pgadapter.NewTenantRepository(pool),
		Invitations:            pgadapter.NewInvitationRepository(sysPool),
		Reconciler:             pgadapter.NewReconcilerStore(sysPool),
		OutboxRunner:           outboxRunner,
		RealmProvisioner:       realmprovisionerclient.New(rawLog),
		Logger:                 log,
		Metrics:                metrics.Recorder{},
		BatchLimit:             envInt("RECONCILER_BATCH_LIMIT", 500),
		SeatOverageGraceDays:   envInt("SEAT_OVERAGE_GRACE_DAYS", 30),
		TrialGraceDays:         envInt("TRIAL_GRACE_DAYS", 15),
		OutboxRetentionDays:    envInt("OUTBOX_RETENTION_DAYS", 8),
		ProcessedEventsTTLDays: envInt("PROCESSED_EVENTS_TTL_DAYS", 8),
	}

	res, err := fn(ctx, jctx)
	if err != nil {
		log.Error("reconciler job failed", "job", jobName, "error", err.Error())
		return 1
	}
	log.Info("reconciler complete", "job", jobName,
		"attempted", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed, "skipped", res.Skipped)
	return 0
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

// isDevLikeEnv reports whether appEnv is one of this service's recognized
// local/dev aliases — matches cmd/server/main.go's validateRequiredEnv
// devOK bypass exactly, so the SYSTEM_DATABASE_URL fail-fast above behaves
// identically in both binaries.
func isDevLikeEnv(appEnv string) bool {
	switch appEnv {
	case "dev", "development", "local", "test":
		return true
	default:
		return false
	}
}
