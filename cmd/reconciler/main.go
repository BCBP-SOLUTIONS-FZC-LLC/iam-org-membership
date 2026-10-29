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
	consumeradapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/consumer"
	auditlogclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/auditlog"
	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	realmprovisionerclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/realmprovisioner"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"

	"go.opentelemetry.io/otel/trace"

	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/logger"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgmetrics"
	"github.com/prometheus/client_golang/prometheus/push"
)

// buildVersion is set at build time (-ldflags "-X main.buildVersion=...");
// BUILD_VERSION in the environment overrides it.
var buildVersion = "dev"

var registry = map[string]jobs.Func{
	"invitation-expiry":      jobs.InvitationExpiry,
	"invitation-kc-cleanup":  jobs.InvitationKCCleanup,
	"realm-config-sync":      jobs.RealmConfigSync,
	"seat-overage-reconcile": jobs.SeatOverageReconcile,
	"trial-cleanup":          jobs.TrialCleanup,
	"outbox-prune":           jobs.OutboxPrune,
	"processed-events-prune": jobs.ProcessedEventsPrune,
	"parked-events-replay":   jobs.ParkedEventsReplay,
	"trial-ledger-prune":     jobs.TrialLedgerPrune,
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

	appEnv := resolveAppEnv()

	// Build the gincommon-backed Zap logger first so ALL log output —
	// including pre-dispatch usage errors — routes through platform-gincommon,
	// never through fmt.Fprintf(os.Stderr). The one remaining fmt.Fprintln
	// below is the bootstrapping exception: if the logger itself fails to
	// initialize there is literally no platform sink to write to.
	rawLog, err := logger.NewLogger(loggerEnv(appEnv))
	if err != nil {
		// Bootstrapping: logger unavailable — no alternative to stderr here.
		fmt.Fprintln(os.Stderr, "init logger: "+err.Error())
		return 1
	}
	log := port.NewSlogStyleLogger(rawLog)

	if jobName == "" {
		log.Error("no job specified — pass --job=<name> or set RECONCILER_JOB", "valid_jobs", registryNames())
		return 1
	}
	fn, ok := registry[jobName]
	if !ok {
		log.Error("unknown reconciler job", "job", jobName, "valid_jobs", registryNames())
		return 1
	}

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

	// The same service identity as cmd/server: the observability standard
	// requires service-specific metrics to be named <domain>_<service>_*, so
	// iam_org_membership_* series must carry service="iam-org-membership".
	// Reconciler runs stay distinguishable by the cronjob grouping label on
	// pushed metrics, the reconciler.<job> span name, and the job log field.
	serviceName := envOr("APP_NAME", "iam-org-membership")
	buildVersion := envOr("BUILD_VERSION", buildVersion)

	// Same TracerProvider as cmd/server so db.query spans from this job
	// export through gincommon's OTLP pipeline when the collector is set.
	// Same identity as the metrics below (service, version, domain,
	// environment), not read from the environment separately.
	shutdownTracing, err := gincommon.InitTracingWithConfig(gincommon.TracingConfig{
		ServiceName:  serviceName,
		BuildVersion: buildVersion,
		Domain:       "iam",
		Environment:  appEnv,
		Logger:       rawLog,
	})
	if err != nil {
		panic("init tracing: " + err.Error())
	}
	// Install trace-ID extractor for port.SlogStyleLogger's *Context variants
	// (same pattern as cmd/server/main.go — only place in this binary that
	// touches the OTel trace API directly; all other trace work goes through
	// gincommon). Must run after InitTracingWithConfig so the provider exists.
	port.SetTraceExtractor(func(ctx context.Context) string {
		sc := trace.SpanFromContext(ctx).SpanContext()
		if !sc.IsValid() {
			return ""
		}
		return sc.TraceID().String()
	})
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
	// {domain, service, environment} const labels. This CronJob never
	// serves /metrics; collectors still have to exist because jobs
	// increment them. Domain is required (startup panics without it); the
	// environment is resolveAppEnv's (local|dev|test|staging|prod), passed
	// explicitly so it matches the tracing identity above.
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
		Environment:  appEnv,
		Logger:       rawLog,
		ServiceName:  serviceName,
		BuildVersion: buildVersion,
		Domain:       "iam",
	})
	// Business metrics before events.InitMetrics so platform-events reuses
	// the registry-shared platform_* collectors (see metrics.Register).
	metrics.Register(appEnv)
	identity := gincommon.MetricsConstLabels()
	evWarnings, err := events.InitMetrics(events.MetricsIdentityFromLabels(identity), gincommon.MetricsRegisterer())
	for _, w := range evWarnings {
		log.Warn("platform-events metric disabled", "metric", w.Metric, "error", w.Error())
	}
	if err != nil {
		log.Error("init platform-events metrics", "error", err.Error())
		return 1
	}
	// Before NewPool below, so the pool connection gauges register.
	pgMetricWarnings, err := pgmetrics.InitWithIdentity(pgmetrics.IdentityFromLabels(identity), gincommon.MetricsRegisterer())
	for _, w := range pgMetricWarnings {
		log.Warn("platform-pgcommon metric disabled", "metric", w.Metric, "error", w.Error())
	}
	if err != nil {
		log.Error("init platform-pgcommon metrics", "error", err.Error())
		return 1
	}

	log.Info("reconciler starting", "job", jobName)

	timeout := 5 * time.Minute
	if s := os.Getenv("RECONCILER_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			timeout = d
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ctx, jobSpan := gincommon.NewTracer(serviceName).Start(ctx, "reconciler."+jobName)
	defer jobSpan.End()

	// pgcommon.ConfigFromEnv reads DATABASE_URL/PG_* directly — same source
	// of truth cmd/server/main.go uses, so pool sizing/DSN assembly never
	// drifts between the two binaries.
	pgCfg, pgWarnings := pgcommon.ConfigFromEnv()
	for _, w := range pgWarnings {
		log.Warn("postgres config warning", "key", w.Key, "reason", w.Reason)
	}
	pgCfg.DSN = pgadapter.DSNFromEnv()
	// Batch sweeps (trial-cleanup's cascading DELETE of up to
	// RECONCILER_BATCH_LIMIT tenants, the prune loops) legitimately run far
	// longer than the API's PG_STATEMENT_TIMEOUT, which the CronJobs inherit
	// from the shared Helm env. The job's own deadline is RECONCILER_TIMEOUT.
	stmtTimeout := envDuration("RECONCILER_STATEMENT_TIMEOUT", defaultReconcilerStatementTimeout)
	pgCfg.StatementTimeout = stmtTimeout
	pgCfg.GUCProvider = pgcommon.GUCSetFromContext
	pgCfg.Logger = pgadapter.NewLoggerAdapter(rawLog)
	pgCfg.Tracer = gincommon.NewSpanTracer(serviceName)
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
	sysCfg.StatementTimeout = stmtTimeout
	sysCfg.Tracer = gincommon.NewSpanTracer(serviceName)
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
	outboxPublisher := eventbusadapter.New(eventbusadapter.EventSource, codec).WithLogger(rawLog)

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

	// EVT-18 replay runs the consumer's own projection under each tenant's
	// GUC on the RLS-scoped app pool. No inbox (the events were already
	// claimed when parked) and no Catalog client: a parked TrialReactivated
	// fails here and stays parked for I-1's replay or a later fix — the
	// stale gauge surfaces it.
	parkedReplayer := consumeradapter.NewMembershipEventConsumer(nil, pgadapter.NewTenantRepository(pool), nil, nil, 0, rawLog).
		WithParkedEvents(pgadapter.NewParkedTenantEventRepository(pool), pgadapter.NewTxRunner(pool, outboxPublisher))
	jctx := &jobs.Context{
		TxRunner:                 pgadapter.NewTxRunner(pool, outboxPublisher),
		Tenants:                  pgadapter.NewTenantRepository(pool),
		Invitations:              pgadapter.NewInvitationRepository(sysPool),
		Reconciler:               pgadapter.NewReconcilerStore(sysPool),
		OutboxRunner:             outboxRunner,
		Inbox:                    pgadapter.NewInboxRepository(sysPool, nil),
		InboxConsumers:           consumeradapter.ProcessedEventsConsumers(),
		RealmProvisioner:         realmprovisionerclient.New(rawLog),
		ParkedReplayer:           parkedReplayer,
		TrialLedger:              pgadapter.NewTrialSignupLedgerRepository(pool),
		Logger:                   log,
		Metrics:                  metrics.Recorder{},
		AuditLog:                 auditlogclient.New(rawLog),
		BatchLimit:               envInt("RECONCILER_BATCH_LIMIT", 500),
		SeatOverageGraceDays:     envInt("SEAT_OVERAGE_GRACE_DAYS", 30),
		TrialGraceDays:           envInt("TRIAL_GRACE_DAYS", 15),
		OutboxRetentionDays:      envInt("OUTBOX_RETENTION_DAYS", 8),
		ProcessedEventsTTLDays:   envInt("PROCESSED_EVENTS_TTL_DAYS", 8),
		TrialLedgerRetentionDays: envInt("TRIAL_LEDGER_RETENTION_DAYS", 730),
	}

	res, err := fn(ctx, jctx)
	// A CronJob is never scraped: push what the run recorded (e.g.
	// iam_org_membership_realm_sync_failed_total), success or failure.
	pushMetrics(ctx, jobName, log)
	if err != nil {
		log.Error("reconciler job failed", "job", jobName, "error", err.Error())
		return 1
	}
	log.Info("reconciler complete", "job", jobName,
		"attempted", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed, "skipped", res.Skipped)
	return 0
}

// pushMetrics pushes this run's metrics to the Prometheus Pushgateway at
// PROMETHEUS_PUSHGATEWAY_URL, grouped by cronjob, replacing the previous
// run's group. Unset URL skips the push (local runs). A failed push is
// logged, never fails the job.
func pushMetrics(ctx context.Context, jobName string, log port.SlogStyleLogger) {
	url := os.Getenv("PROMETHEUS_PUSHGATEWAY_URL")
	if url == "" {
		return
	}
	pushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := push.New(url, "iam-org-membership-reconciler").
		Grouping("cronjob", jobName).
		Gatherer(gincommon.MetricsGatherer()).
		PushContext(pushCtx)
	if err != nil {
		log.Warn("pushgateway push failed — this run's metrics are lost", "job", jobName, "error", err.Error())
	}
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

// defaultReconcilerStatementTimeout bounds one statement in a reconciler
// job (RECONCILER_STATEMENT_TIMEOUT overrides it).
const defaultReconcilerStatementTimeout = 2 * time.Minute

// envDuration parses key as a Go duration, or returns def when unset or
// invalid.
func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
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
	case "dev", "local", "test":
		return true
	default:
		return false
	}
}
