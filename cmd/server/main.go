// Package main is the composition root for the Org & Membership service.
// It wires pgcommon.NewPool with the transaction-local RLS GUC provider
// (RLS-6), runs domain migrations, applies the outbox schema, wires the
// two-topic RoutingPublisher, and serves the HTTP surface.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/gin-gonic/gin"

	_ "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/docs/swagger"
	consumeradapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/consumer"
	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/http"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/sqspipeline"
	auditlogclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/auditlog"
	catalogadminclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/catalogadmin"
	delegationcheckclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/delegationcheck"
	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/eventbus"
	groupmappingclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/groupmappingclient"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	realmprovisionerclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/realmprovisioner"
	tokenserviceclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/tokenservice"
	valkeyadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/valkey"
	workflowclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/workflow"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"

	"go.opentelemetry.io/otel/trace"

	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/logger"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgmetrics"
)

// buildVersion is injected by -ldflags at build time (see Dockerfile / Makefile).
var buildVersion = "dev"

// requestTimeout is the hard per-request deadline (gincommon.Config.RequestTimeout).
const requestTimeout = 30 * time.Second

// initLibraryMetrics registers platform-events' and platform-pgcommon's
// platform_* metrics on gincommon's registerer with gincommon's identity
// (call after ObservabilityMiddlewares and metrics.Register, before any
// pgcommon.NewPool). A metric that cannot be registered is disabled by the
// library and reported as a warning; an invalid identity aborts startup.
func initLibraryMetrics(log port.Logger) {
	identity := gincommon.MetricsConstLabels()
	evWarnings, err := events.InitMetrics(events.MetricsIdentityFromLabels(identity), gincommon.MetricsRegisterer())
	for _, w := range evWarnings {
		log.Warn("platform-events metric disabled", map[string]interface{}{"metric": w.Metric, "error": w.Error()})
	}
	if err != nil {
		panic(fmt.Sprintf("init platform-events metrics: %v", err))
	}
	pgWarnings, err := pgmetrics.InitWithIdentity(pgmetrics.IdentityFromLabels(identity), gincommon.MetricsRegisterer())
	for _, w := range pgWarnings {
		log.Warn("platform-pgcommon metric disabled", map[string]interface{}{"metric": w.Metric, "error": w.Error()})
	}
	if err != nil {
		panic(fmt.Sprintf("init platform-pgcommon metrics: %v", err))
	}
}

func main() {
	appEnv := resolveAppEnv()
	if !isDevLikeAppEnv(appEnv) {
		gin.SetMode(gin.ReleaseMode)
	}

	// Fail fast on missing required env before touching any dependencies —
	// operators get a clear startup error instead of a runtime failure deep
	// in the first request path.
	validateRequiredEnv(appEnv)

	// ── 1. Logger — Zap via platform-gincommon (same sink as iam-user-profile)
	log, err := logger.NewLogger(loggerEnv(appEnv))
	if err != nil {
		panic("init logger: " + err.Error())
	}

	cfg := gincommon.Config{
		Logger:       log,
		ServiceName:  envOr("APP_NAME", "iam-org-membership"),
		BuildVersion: envOr("BUILD_VERSION", buildVersion),
		// Observability Standard identity: domain is fixed for this service
		// (OBSERVABILITY_DOMAIN is also set in Helm/compose). Environment is
		// passed explicitly (resolveAppEnv: local|dev|test|staging|prod —
		// startup panics on anything else, e.g. "production") so metrics,
		// traces and logs all carry the same value.
		Domain:      "iam",
		Environment: appEnv,
		// RequestTimeout appends gincommon's TimeoutMiddleware as the
		// innermost observability handler, so metrics, the access log and
		// the server span record the same 503 the client receives. As an
		// outer r.Use it ran before observability, which recorded 200.
		RequestTimeout: requestTimeout,
	}

	// ── 2. Tracing — always install gincommon's TracerProvider so in-process
	// spans get valid trace IDs even when OTEL_EXPORTER_OTLP_ENDPOINT is unset
	// (dev), with the SAME identity as metrics (service, version, domain,
	// environment). InitTracingFromEnv read domain/environment from the
	// environment on its own, so traces could disagree with metrics.
	// ObservabilityMiddlewares' EnsureInitTelemetry is then a no-op.
	shutdownTracing, err := gincommon.InitTracingWithConfig(gincommon.TracingConfig{
		ServiceName:  cfg.ServiceName,
		BuildVersion: cfg.BuildVersion,
		Domain:       cfg.Domain,
		Environment:  cfg.Environment,
		Logger:       log,
	})
	if err != nil {
		panic("init tracing: " + err.Error())
	}
	// Install the trace-ID extractor used by port.SlogStyleLogger's *Context
	// variants. This is the only place in the binary that touches the OTel
	// trace API directly; every other trace interaction goes through gincommon
	// (InitTracingWithConfig above, NewTracer in sqspipeline, SpanTracer in
	// pgcommon). The extractor is set here, after the provider is installed,
	// so span context is always valid when log4 calls it.
	port.SetTraceExtractor(func(ctx context.Context) string {
		sc := trace.SpanFromContext(ctx).SpanContext()
		if !sc.IsValid() {
			return ""
		}
		return sc.TraceID().String()
	})
	// ObservabilityMiddlewares is gincommon's public metrics-init API. Call
	// it here (before any collector registration or exporter goroutine) so
	// business / events / pgcommon metrics land on gincommon.MetricsRegisterer
	// with matching {domain, service, environment} const labels. NewRouter
	// applies the same middleware slice to the Gin engine; metrics.Init is
	// sync.Once.
	_ = gincommon.ObservabilityMiddlewares(cfg)

	// Business metrics BEFORE events.InitMetrics: the registry-shared
	// platform_* collectors (DLQ, duplicates, dependency calls) are created
	// here and reused by platform-events instead of conflicting with it.
	metrics.Register(appEnv)

	// platform-events outbox/publish/consume metrics and platform-pgcommon
	// query/pool metrics share gincommon's registerer and identity so a
	// single /metrics scrape (gincommon.MetricsHandler on METRICS_PORT) serves HTTP +
	// business + outbox + pg collectors together. pgmetrics must be
	// initialised before NewPool so the pool connection gauges register.
	initLibraryMetrics(log)

	// ── 3. Database — pgcommon.ConfigFromEnv reads DATABASE_URL/PG_* directly
	// so pool sizing, PgBouncer mode, and DSN assembly have exactly one
	// implementation instead of a second one hand-rolled here. ────────────
	pgCfg, pgWarnings := pgcommon.ConfigFromEnv()
	for _, w := range pgWarnings {
		log.Warn("postgres config warning", map[string]interface{}{"key": w.Key, "reason": w.Reason})
	}
	// DSNFromEnv (not a bare pgCfg.DSN) so the DATABASE_URL bypass applies
	// here too: PG_STATEMENT_TIMEOUT must be ignored when DATABASE_URL is
	// set verbatim, per DSNFromEnv/ApplyStatementTimeout's own contract.
	dsn := pgadapter.DSNFromEnv()
	// Migrations must bypass PgBouncer because the migration runner acquires
	// a pg_advisory_lock, which is session-scoped and breaks under
	// transaction pooling (CONFIG-2). MIGRATION_DATABASE_URL points directly
	// at Postgres; falls back to dsn when unset.
	migrationDSN := pgadapter.MigrationDSNFromEnv()

	pgCfg.DSN = dsn
	pgCfg.GUCProvider = pgcommon.GUCSetFromContext
	// pgcommon v1.2.0 retyped Config.Logger against a public domain.Logger
	// (previously typed against an internal, externally-unimplementable
	// interface) — slow-query warnings now route through the same
	// Zap-backed sink as everything else, at the SlowQueryThreshold already
	// resolved by ConfigFromEnv above.
	pgCfg.Logger = pgadapter.NewLoggerAdapter(log)
	// db.query spans export through the TracerProvider
	// gincommon.InitTracingWithConfig installed above — same OTLP pipeline as
	// HTTP spans from ObservabilityMiddlewares.
	pgCfg.Tracer = gincommon.NewSpanTracer(cfg.ServiceName)
	pool, err := pgcommon.NewPool(context.Background(), pgCfg)
	if err != nil {
		panic(fmt.Sprintf("connect to postgres: %v", err))
	}
	defer pool.Close()

	// sysPool: BYPASSRLS pool used by reconciler jobs, cross-tenant metric
	// exporters, I-16, and the seat-overage reconciler (LLD §4.4). In
	// production SYSTEM_DATABASE_URL must point to org_membership_migrator
	// (RLS-4). In dev it falls back to DSNFromEnv() so single-role setups
	// keep working, with a warning so the operator knows cross-tenant
	// queries will RLS-filter to zero rows.
	//
	// Built via SystemPoolConfig (same helper as iam-user-profile): no
	// GUCProvider, PGBouncerMode forced true, pool sizing inherited from
	// ConfigFromEnv. Tracer is wired separately so db.query spans export
	// through gincommon's TracerProvider.
	sysDSN := pgadapter.SystemDSNFromEnv()
	sysCfg := pgadapter.SystemPoolConfig(sysDSN, log)
	sysCfg.Tracer = gincommon.NewSpanTracer(cfg.ServiceName)
	sysPool, err := pgcommon.NewPool(context.Background(), sysCfg)
	if err != nil {
		panic(fmt.Sprintf("connect sysPool: %v", err))
	}
	defer sysPool.Close()
	if sysDSN == dsn {
		log.Warn("SYSTEM_DATABASE_URL not set — sysPool reuses app DSN; cross-tenant queries will be RLS-filtered", nil)
	}

	ctx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()

	// Schema: in Kubernetes the migrate Job (cmd/migrate, a Helm
	// pre-install/pre-upgrade hook) migrates once per release and every
	// replica runs with RUN_MIGRATIONS=false. Unset or true (local dev,
	// tests), the server migrates at startup itself.
	if runMigrationsAtStartup() {
		if err := pgadapter.MigrateSchema(ctx, migrationDSN, log); err != nil {
			panic(fmt.Sprintf("migrate: %v", err))
		}
	} else {
		log.Info("RUN_MIGRATIONS=false — schema is migrated by the migrate Job", nil)
	}

	// ── 4. Cache ──────────────────────────────────────────────────────────
	cache := valkeyadapter.New(envOr("VALKEY_URL", "localhost:6379"))
	defer func() {
		if cerr := cache.Close(); cerr != nil {
			log.Error("valkey close error", map[string]interface{}{"error": cerr.Error()})
		}
	}()

	// ── 5. AWS clients ────────────────────────────────────────────────────
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(envOr("AWS_REGION", "ap-south-1")),
	)
	if err != nil {
		panic(fmt.Sprintf("load aws config: %v", err))
	}

	var sqsOpts []func(*sqs.Options)
	var glueOpts []func(*glue.Options)
	if ep := os.Getenv("AWS_ENDPOINT_URL"); ep != "" {
		sqsOpts = append(sqsOpts, func(o *sqs.Options) { o.BaseEndpoint = &ep })
		glueOpts = append(glueOpts, func(o *glue.Options) { o.BaseEndpoint = &ep })
	}
	// sqsClient is shared by both inbound consumers (§7.1b below). glueClient
	// backs the two GlueCodec instances built next. Neither platform-events'
	// NewSNSPublisher (builds its own SNS client from SNSConfig.Region/
	// EndpointURL) nor NewRoutingPublisher needs an SNS client constructed
	// here — there is deliberately no sns.NewFromConfig call in this file.
	sqsClient := sqs.NewFromConfig(awsCfg, sqsOpts...)
	// dlqPublisher is platform-events' DLQ forwarder over the same client. It
	// resolves each queue's DLQ from its own RedrivePolicy (cached, re-read on
	// expiry) and is used both by the consumers' automatic forwarding
	// (WithDLQForwarding) and by routeRejectsToDLQ's explicit rejects.
	dlqPublisher, err := events.NewSQSDLQPublisherWithClient(events.DLQConfig{
		ConsumerName: envOr("APP_NAME", "iam-org-membership"),
		Logger:       log,
	}, sqsClient)
	if err != nil {
		panic(fmt.Sprintf("build DLQ publisher: %v", err))
	}
	glueClient := glue.NewFromConfig(awsCfg, glueOpts...)

	// ── 6. Event codec + outbox publisher ─────────────────────────────────
	// Two-codec architecture:
	//   enqueueCodec — schema validation only (wraps NoopCodec); outbox always
	//                  stores plain JSON regardless of the SNS-side codec below.
	//   snsCodec     — wire encoding (Glue) applied transiently by the SNS
	//                  publisher immediately before publish (events.WithCodec);
	//                  built once per topic below since each topic is backed by
	//                  its own Glue registry (SCHEMA-7).
	enqueueCodec, err := eventbusadapter.NewValidatingCodec(eventbusadapter.NoopCodec{})
	if err != nil {
		panic(fmt.Sprintf("init validating codec: %v", err))
	}
	outboxPublisher := eventbusadapter.New(eventbusadapter.EventSource, enqueueCodec).WithLogger(log)
	// Consumed payloads are validated against the same embedded schemas; an
	// event type with no schema (ErrNoSchema) passes through to the
	// consumer's ackUnknown forward-compat path.
	consumedValidator := sqspipeline.ValidatorFunc(func(eventType string, payload []byte) error {
		if err := enqueueCodec.Validate(eventType, payload); !errors.Is(err, eventbusadapter.ErrNoSchema) {
			return err
		}
		return nil
	})
	// txRunner injects a tx-bound ContextEventPublisher into the ctx so
	// services can call port.EventPublisherFromContext(ctx).EnqueueCtx
	// inside a RunInTx block — state write + event insert commit together
	// (EVT-10, CONS-1..4).
	txRunner := pgadapter.NewTxRunner(pool, outboxPublisher)

	// Two-topic RoutingPublisher — wraps SNS publishers per topic, each
	// carrying its own Glue-backed wire-format codec when that topic's
	// registry env var is set; falls back to NoopCodec (plain JSON, dev/test
	// without a Glue registry) when unset. In dev (no SNS_TOPIC_*_ARN set)
	// both lanes fall back to the noop publisher regardless of codec.
	allSchemaNames, err := eventbusadapter.AllSchemaNames()
	if err != nil {
		panic(fmt.Sprintf("list embedded event schemas: %v", err))
	}
	var membershipSchemas, tenantSchemas []string
	for _, name := range allSchemaNames {
		if domain.TopicForEvent(name) == domain.TopicTenant {
			tenantSchemas = append(tenantSchemas, name)
		} else {
			membershipSchemas = append(membershipSchemas, name)
		}
	}
	membershipCodec, err := buildTopicCodec(ctx, glueClient, os.Getenv("GLUE_REGISTRY_MEMBERSHIP_NAME"), membershipSchemas)
	if err != nil {
		panic(fmt.Sprintf("init membership glue codec: %v", err))
	}
	tenantCodec, err := buildTopicCodec(ctx, glueClient, os.Getenv("GLUE_REGISTRY_TENANT_NAME"), tenantSchemas)
	if err != nil {
		panic(fmt.Sprintf("init tenant glue codec: %v", err))
	}
	membershipPub, err := buildTopicPublisher(os.Getenv("SNS_TOPIC_MEMBERSHIP_ARN"), membershipCodec, log)
	if err != nil {
		panic(fmt.Sprintf("build membership publisher: %v", err))
	}
	tenantPub, err := buildTopicPublisher(os.Getenv("SNS_TOPIC_TENANT_ARN"), tenantCodec, log)
	if err != nil {
		panic(fmt.Sprintf("build tenant publisher: %v", err))
	}
	routingPublisher := eventbusadapter.NewRoutingPublisher(membershipPub, tenantPub)

	// ── 7. Outbox runner ─────────────────────────────────────────────────
	// LoadOutbox + RunnerConfigFromEnv is the platform-events composition
	// contract (same as iam-user-profile). Helm / .env-example keep the
	// historical 500ms / concurrency-4 / 2s jitter / 10m claim-lease values
	// via OUTBOX_* env; library defaults apply only when those are unset.
	outboxEnv := eventcfg.LoadOutbox()
	eventcfg.LogWarningsTo(log, outboxEnv.Warnings)
	outboxRunner, err := outbox.NewRunner(eventcfg.RunnerConfigFromEnv(outboxEnv, pool, routingPublisher, log))
	if err != nil {
		panic(fmt.Sprintf("create outbox runner: %v", err))
	}

	go func() {
		if err := outboxRunner.Start(ctx); err != nil {
			log.Error("outbox runner stopped", map[string]interface{}{"error": err.Error()})
		}
	}()

	// catalogAdminClient/catalogReader: departments/plans read paths go
	// through catalog-admin-config's CAT-I1/CAT-I2 + a local cache.
	// Migration-runbook Phase 4 (LLD §12 step 4) completed the cutover —
	// catalog-admin-config is now the sole writer too; O-1/O-2/O-3/O-5/O-6
	// and the local departments/plans tables have been removed from this
	// service entirely. Built here (before the SQS consumer wiring below)
	// because MembershipEventConsumer's TrialReactivated handling also
	// needs a PlanCatalogReader.
	catalogAdminClient := catalogadminclient.New(log)
	catalogReader := service.NewCatalogService(catalogAdminClient, cache).WithLogger(log)

	// ── 7b. Inbound SQS consumers (§7.1) ─────────────────────────────────
	// One handler covers both tenant-orgm-q + billing-orgm-q; EVT-14/15/16
	// guards live inside the handler. Both queues share the same consumer
	// identity in processed_events (§16 A33 / PE-1).
	skew := envDuration("MAX_LIFECYCLE_EVENT_SKEW_SECONDS", 300*time.Second)
	// processed_events dedup through platform-events' inbox: the claim, the
	// projection and the relay events commit in one transaction (IDEMP-1..4).
	inbox := pgadapter.NewInboxRepository(pool, outboxPublisher)
	// The TenantIdentityProvisioned signup backstop runs I-1. The consumers
	// start before the HTTP services below are built, so this is its own
	// ProvisioningService over the same pool (TrialSignup needs no Realm
	// Provisioner client, hence nil).
	backstopProvisioning := service.NewProvisioningService(
		pgadapter.NewTenantRepository(pool), pgadapter.NewMembershipRepository(pool),
		pgadapter.NewTenantRoleRepository(pool), pgadapter.NewDeptMembershipRepository(pool),
		pgadapter.NewDeptRoleLabelRepository(pool), pgadapter.NewTenantDepartmentRepository(pool),
		catalogReader, catalogReader, txRunner, cache, nil,
	).WithLogger(log)
	membershipConsumer := consumeradapter.NewMembershipEventConsumer(inbox, pgadapter.NewTenantRepository(pool), catalogReader, cache, skew, log).
		WithTenantProvisioner(backstopProvisioning)

	// LoadSQS supplies region / endpoint / long-poll / visibility / max-receive
	// from the library env contract. Per-queue URL + concurrency overlay
	// SQS_QUEUE_URL / SQS_CONCURRENCY, which this service cannot use as-is
	// (three inbound queues).
	sqsEnv := eventcfg.LoadSQS()
	eventcfg.LogWarningsTo(log, sqsEnv.Warnings)

	// OBS-1: each wired consumer gets a heartbeat pinger so /readyz can
	// detect a dead goroutine and pull the pod from rotation immediately.
	type wiredConsumer struct {
		cons      events.Consumer
		heartbeat *ConsumerHeartbeat
	}
	var sqsConsumers []wiredConsumer
	if url := os.Getenv("SQS_TENANT_ORGM_QUEUE_URL"); url != "" {
		if err := sqspipeline.RequireDLQ(ctx, dlqPublisher, url, log); err != nil {
			panic(fmt.Sprintf("DLQ preflight: %v", err))
		}
		cons, err := sqspipeline.NewConsumer(
			sqspipeline.EnvForQueue(sqsEnv, url, envInt("SQS_TENANT_ORGM_CONCURRENCY", 4)),
			sqsClient,
			sqspipeline.Handler("tenant-orgm-q", membershipConsumer.Handle, consumedValidator, dlqPublisher, log),
			dlqPublisher,
			log,
		)
		if err != nil {
			panic(fmt.Sprintf("build tenant-orgm-q consumer: %v", err))
		}
		sqsConsumers = append(sqsConsumers, wiredConsumer{cons: cons, heartbeat: newConsumerHeartbeat("tenant-orgm-q")})
		log.Info("tenant-orgm-q consumer wired", map[string]interface{}{"queue_url": url})
	} else {
		log.Warn("SQS_TENANT_ORGM_QUEUE_URL unset — tenant lifecycle consumer disabled", nil)
	}
	if url := os.Getenv("SQS_BILLING_ORGM_QUEUE_URL"); url != "" {
		if err := sqspipeline.RequireDLQ(ctx, dlqPublisher, url, log); err != nil {
			panic(fmt.Sprintf("DLQ preflight: %v", err))
		}
		cons, err := sqspipeline.NewConsumer(
			sqspipeline.EnvForQueue(sqsEnv, url, envInt("SQS_BILLING_ORGM_CONCURRENCY", 2)),
			sqsClient,
			sqspipeline.Handler("billing-orgm-q", membershipConsumer.Handle, consumedValidator, dlqPublisher, log),
			dlqPublisher,
			log,
		)
		if err != nil {
			panic(fmt.Sprintf("build billing-orgm-q consumer: %v", err))
		}
		sqsConsumers = append(sqsConsumers, wiredConsumer{cons: cons, heartbeat: newConsumerHeartbeat("billing-orgm-q")})
		log.Info("billing-orgm-q consumer wired", map[string]interface{}{"queue_url": url})
	} else {
		log.Warn("SQS_BILLING_ORGM_QUEUE_URL unset — billing consumer disabled", nil)
	}

	// Gap-12 fix: catalog-orgm-q consumer — clears om:departments /
	// om:departments:stale cache on every DepartmentCatalogChanged event
	// published by iam-catalog-admin, eliminating the 11-min TTL delay.
	// Requires Catalog Service to publish DepartmentCatalogChanged events and
	// infra to provision the catalog-orgm-q SQS queue + SNS subscription.
	catalogConsumer := consumeradapter.NewCatalogConsumer(cache, inbox, log)
	if url := os.Getenv("SQS_CATALOG_ORGM_QUEUE_URL"); url != "" {
		if err := sqspipeline.RequireDLQ(ctx, dlqPublisher, url, log); err != nil {
			panic(fmt.Sprintf("DLQ preflight: %v", err))
		}
		cons, err := sqspipeline.NewConsumer(
			sqspipeline.EnvForQueue(sqsEnv, url, envInt("SQS_CATALOG_ORGM_CONCURRENCY", 2)),
			sqsClient,
			sqspipeline.Handler("catalog-orgm-q", catalogConsumer.Handle, consumedValidator, dlqPublisher, log),
			dlqPublisher,
			log,
		)
		if err != nil {
			panic(fmt.Sprintf("build catalog-orgm-q consumer: %v", err))
		}
		sqsConsumers = append(sqsConsumers, wiredConsumer{cons: cons, heartbeat: newConsumerHeartbeat("catalog-orgm-q")})
		log.Info("catalog-orgm-q consumer wired", map[string]interface{}{"queue_url": url})
	} else {
		log.Warn("SQS_CATALOG_ORGM_QUEUE_URL unset — catalog cache invalidation disabled (departments and plans caches TTL-only)", nil)
	}

	// catalog-orgm-plans-q — dedicated handler for PlanCatalogChanged events.
	// Merges the plan's new feature_set into tenants.feature_flags for all active
	// tenants on the affected plan (sysPool BYPASSRLS) and evicts om:tenant caches.
	// Separate from catalog-orgm-q because it requires a cross-tenant DB write
	// and benefits from independent scaling + DLQ policy.
	// sysPool-backed repo so the bulk UPDATE crosses RLS (BYPASSRLS role).
	planCatalogConsumer := consumeradapter.NewPlanCatalogPlansConsumer(pgadapter.NewTenantRepository(sysPool), cache, inbox, log)
	if url := os.Getenv("SQS_CATALOG_ORGM_PLANS_QUEUE_URL"); url != "" {
		if err := sqspipeline.RequireDLQ(ctx, dlqPublisher, url, log); err != nil {
			panic(fmt.Sprintf("DLQ preflight: %v", err))
		}
		cons, err := sqspipeline.NewConsumer(
			sqspipeline.EnvForQueue(sqsEnv, url, envInt("SQS_CATALOG_ORGM_PLANS_CONCURRENCY", 1)),
			sqsClient,
			sqspipeline.Handler("catalog-orgm-plans-q", planCatalogConsumer.Handle, consumedValidator, dlqPublisher, log),
			dlqPublisher,
			log,
		)
		if err != nil {
			panic(fmt.Sprintf("build catalog-orgm-plans-q consumer: %v", err))
		}
		sqsConsumers = append(sqsConsumers, wiredConsumer{cons: cons, heartbeat: newConsumerHeartbeat("catalog-orgm-plans-q")})
		log.Info("catalog-orgm-plans-q consumer wired", map[string]interface{}{"queue_url": url})
	} else {
		log.Warn("SQS_CATALOG_ORGM_PLANS_QUEUE_URL unset — plan feature_set propagation to tenants disabled (feature flags TTL-only)", nil)
	}

	for _, wc := range sqsConsumers {
		wc := wc
		go func() {
			// RELIABILITY-3: recover() so a consumer panic does not crash the
			// entire pod. Log the panic and let the pod continue serving HTTP;
			// the consumer will need a pod restart to resume — this is observable
			// via the iam_org_membership_* consumer-dead alert (OBS-2).
			// OBS-1: MarkDead() runs on every exit path (panic or normal
			// return) so /readyz detects the dead goroutine within one probe
			// interval and removes the pod from rotation.
			defer wc.heartbeat.MarkDead()
			defer func() {
				if r := recover(); r != nil {
					log.Error("SQS consumer panic — consumer stopped, pod restart required",
						map[string]interface{}{"panic": fmt.Sprintf("%v", r)})
				}
			}()
			if err := wc.cons.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("SQS consumer stopped", map[string]interface{}{"error": err.Error()})
			}
		}()
	}

	// ── 7c. Business-observability exporter goroutines (§11.2) ──────────
	// Populate iam_org_membership_tenant_ownerless /
	// iam_org_membership_realm_sync_pending /
	// iam_org_membership_seat_overage_active /
	// iam_org_membership_pending_invitations_stale gauges every
	// 5 minutes from the sysPool (BYPASSRLS). Follows the sibling
	// iam-user-profile2 pattern — exporters live as goroutines, not
	// CronJobs, so the running server pod is the source of truth.
	runBusinessExporters(ctx, pgadapter.NewGaugeRepository(sysPool), log)

	// ── 8b. Repositories, services, handlers ─────────────────────────────
	tenantRepo := pgadapter.NewTenantRepository(pool)
	tenantDeptRepo := pgadapter.NewTenantDepartmentRepository(pool)
	membershipRepo := pgadapter.NewMembershipRepository(pool)
	tenantRoleRepo := pgadapter.NewTenantRoleRepository(pool)
	deptMemRepo := pgadapter.NewDeptMembershipRepository(pool)
	deptRoleLabelRepo := pgadapter.NewDeptRoleLabelRepository(pool)
	invitationRepo := pgadapter.NewInvitationRepository(pool)
	authzRepo := pgadapter.NewAuthZRepository(pool)

	// Outbound clients — wired to real HTTP adapters behind port interfaces.
	// log is threaded through so their transport-error warnings flow through
	// the same gincommon-backed sink as HTTP/consumer logs instead of
	// slog.Default().
	rpClient := realmprovisionerclient.New(log)
	wfClient := workflowclient.New(log)
	// delegationCheckClient: DeptMembershipService's WFI-11 §8.8.4 dept-scope
	// removal precision goes through the standalone Delegation Service's
	// DLG-I3 (ADR-0008 v2 §6.4) now that `delegations` no longer lives in
	// this service's database.
	delegationCheckClient := delegationcheckclient.New(log)
	// groupMappingClient: I-10's mapping-resolution step goes through Group
	// Mapping Service's GM-I1 behind the om:grm/gdm/gtrm cache (ADR-0007
	// Wave 2). P-14..P-29 admin CRUD and the local group-mapping tables
	// have been fully removed from this service — Group Mapping Service is
	// now the sole owner of that config surface.
	groupMappingClient := groupmappingclient.New(log)
	// tokenServiceClient: AUTH-9's defense-in-depth service-account-not-
	// grantable check on the membership-create (P-6/I-3) and role-grant
	// (P-10/P-28) paths goes through Token Service's TS-5 lookup — the only
	// way this service can resolve a subject's Keycloak sub against the
	// tenant's automation principal without ever touching Keycloak itself
	// (RP-INV-1). Fails open: a Token Service outage degrades to allowing
	// the operation, since the primary guarantee is structural (composite
	// FK bar, TR-8/DM-4).
	tokenServiceClient := tokenserviceclient.New(log)
	auditLogClient := auditlogclient.New(log)

	seatOverageDays := envInt("SEAT_OVERAGE_GRACE_DAYS", 30)
	// I-16 (§16 RP-C3): bound against sysPool (BYPASSRLS), NOT pool — RP's
	// subscription-lapse sweep needs every tenant past grace, cross-tenant,
	// the same reason the reconciler jobs and business-metric exporters
	// above already use sysPool instead of the RLS-scoped app pool.
	subscriptionGraceDays := envInt("SUBSCRIPTION_GRACE_DAYS", 30)
	sysTenantRepo := pgadapter.NewTenantRepository(sysPool)
	invitationExpiryDays := envInt("INVITATION_EXPIRY_DAYS", 7)
	reinviteCooldownMin := envInt("INVITE_REINVITE_COOLDOWN_MINUTES", 60)   // PI-11
	inviteMaxPerHour := envInt("INVITE_MAX_PER_TENANT_PER_HOUR", 200)       // PI-12
	inviteMaxPerActorPerHour := envInt("INVITE_MAX_PER_ACTOR_PER_HOUR", 10) // SECURITY-5

	authzSvc := service.NewAuthZService(authzRepo, catalogReader, catalogReader, cache)
	provisioningSvc := service.NewProvisioningService(tenantRepo, membershipRepo, tenantRoleRepo, deptMemRepo, deptRoleLabelRepo, tenantDeptRepo, catalogReader, catalogReader, txRunner, cache, rpClient).
		WithLogger(log).
		WithTrialLedger(pgadapter.NewTrialSignupLedgerRepository(pool))
	tenantSvc := service.NewTenantService(tenantRepo, cache, rpClient).
		WithAuditLog(auditLogClient).
		WithPlanCatalog(catalogReader).
		WithIDPReader(rpClient).
		WithTxRunner(txRunner)
	deptSvc := service.NewDepartmentService(catalogReader, tenantDeptRepo, cache)
	membershipSvc := service.NewMembershipService(membershipRepo, tenantRoleRepo, deptMemRepo, tenantRepo, invitationRepo, cache, rpClient, wfClient, txRunner, log, seatOverageDays).
		WithTokenServiceClient(tokenServiceClient)
	deptMemSvc := service.NewDeptMembershipService(deptMemRepo, membershipRepo, tenantDeptRepo, catalogReader, delegationCheckClient, wfClient, cache, txRunner).
		WithLogger(log).
		WithTokenServiceClient(tokenServiceClient)
	roleLabelSvc := service.NewRoleLabelService(deptRoleLabelRepo, cache)
	groupMappingSvc := service.NewGroupMappingService(membershipRepo, tenantRoleRepo, deptMemRepo, txRunner, cache, groupMappingClient).WithLogger(log)
	invitationSvc := service.NewInvitationService(invitationRepo, membershipRepo, tenantRoleRepo, deptMemRepo, tenantRepo, rpClient, cache, txRunner, log, invitationExpiryDays).
		WithReinviteCooldown(time.Duration(reinviteCooldownMin) * time.Minute).
		WithMaxInvitesPerHour(inviteMaxPerHour).
		WithMaxInvitesPerActorPerHour(inviteMaxPerActorPerHour).
		WithTokenServiceClient(tokenServiceClient).
		WithAuditLog(auditLogClient)
	operatorSvc := service.NewOperatorService(tenantRepo, tenantRoleRepo, membershipRepo, cache, txRunner)
	subscriptionLapseSvc := service.NewSubscriptionLapseService(sysTenantRepo, subscriptionGraceDays)
	// PUB-1 (LLD §5.4/§8.11): sysTenantRepo, NOT tenantRepo — the route is
	// anonymous and never binds an app.tenant_id GUC, so the RLS-scoped app
	// pool would fail closed and return zero rows for every lookup.
	tenantResolutionSvc := service.NewTenantResolutionService(sysTenantRepo)

	tenantH := httpadapter.NewTenantHandler(tenantSvc)
	deptH := httpadapter.NewDepartmentHandler(deptSvc)
	membershipH := httpadapter.NewMembershipHandler(membershipSvc)
	deptMemH := httpadapter.NewDeptMembershipHandler(deptMemSvc)
	roleLabelH := httpadapter.NewRoleLabelHandler(roleLabelSvc)
	// DelegationHandler and DelegationService removed entirely (ADR-0008
	// v2) — P-18/19/20/32/33 moved to the standalone Delegation Service's
	// DLG-1/2/3/4/5; IDs never reused.
	// ACLHandler and TenderACLService removed entirely (ADR-0007 Wave 3
	// Phase 6) — P-21/22/23/I-12 moved to iam-tender-acl's TAC-1/2/3/4;
	// IDs never reused.
	invitationH := httpadapter.NewInvitationHandler(invitationSvc)
	operatorH := httpadapter.NewOperatorHandler(operatorSvc)
	publicH := httpadapter.NewPublicHandler(tenantResolutionSvc)
	internalH := httpadapter.NewInternalHandler(provisioningSvc, authzSvc, membershipSvc, invitationSvc, groupMappingSvc, tenantSvc, subscriptionLapseSvc, deptMemSvc)

	// ── Router — all routing/middleware wiring lives in the inbound HTTP
	// adapter (internal/adapter/inbound/http/router.go), not here. main.go's
	// job is to construct dependencies and hand them to NewRouter.
	docsEnabled := envOr("DOCS_ENABLED", "false") == "true"
	docsAuthToken := os.Getenv("DOCS_AUTH_TOKEN")
	// SECURITY-10: DOCS_ENABLED=true without DOCS_AUTH_TOKEN in production/
	// staging exposes the Swagger UI to unauthenticated callers. Hard-fail at
	// startup so the misconfiguration is caught before any traffic is served.
	if docsEnabled && docsAuthToken == "" && (appEnv == "prod" || appEnv == "staging") {
		panic("startup aborted — DOCS_ENABLED requires DOCS_AUTH_TOKEN in production/staging (SECURITY-10)")
	}

	router := httpadapter.NewRouter(httpadapter.RouterConfig{
		GinConfig: cfg,
		Docs: httpadapter.DocsConfig{
			Environment: appEnv,
			Enabled:     docsEnabled,
			AuthToken:   docsAuthToken,
		},

		TenantRepo:     tenantRepo,
		MembershipRepo: membershipRepo,

		TenantHandler:         tenantH,
		DepartmentHandler:     deptH,
		MembershipHandler:     membershipH,
		DeptMembershipHandler: deptMemH,
		RoleLabelHandler:      roleLabelH,
		InvitationHandler:     invitationH,
		OperatorHandler:       operatorH,
		InternalHandler:       internalH,
		PublicHandler:         publicH,

		// Helm iam.strictTenantPathValidation. Default false: a mismatched
		// x-tenant-id on /internal/tenants/:id/* is overridden by the path
		// tenant, logged and counted; true rejects it with 400.
		StrictTenantPathValidation: envBool("STRICT_TENANT_PATH_VALIDATION", false),

		Postgres: pingerFunc(func(ctx context.Context) error {
			if hs := pool.Health(ctx); !hs.Healthy {
				return fmt.Errorf("database not healthy")
			}
			return nil
		}),
		// sysPool is a separate physical connection (BYPASSRLS role) from
		// the app pool above — ping it directly so /readyz notices a
		// credential/network problem specific to that role, rather than
		// waiting for the next cross-tenant sweep/reconciler run to fail.
		SysPostgres: pingerFunc(func(ctx context.Context) error {
			return sysPool.Ping(ctx)
		}),
		Cache: cache,
		Outbox: pingerFunc(func(context.Context) error {
			select {
			case <-outboxRunner.Ready():
				return nil
			default:
				return fmt.Errorf("outbox initialising")
			}
		}),
		// OBS-1: per-queue consumer liveness checks. Built from sqsConsumers
		// after the consumer goroutines are launched above — each heartbeat
		// starts alive and MarkDead() is deferred inside the goroutine.
		Consumers: func() []httpadapter.Pinger {
			out := make([]httpadapter.Pinger, len(sqsConsumers))
			for i, wc := range sqsConsumers {
				out[i] = wc.heartbeat
			}
			return out
		}(),
	})

	// ── 9. Graceful shutdown ─────────────────────────────────────────────
	srv := &http.Server{
		Addr:              ":" + envOr("APP_PORT", "8080"),
		Handler:           router.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      35 * time.Second, // 30s app timeout + 5s buffer
		IdleTimeout:       60 * time.Second,
	}

	// Metrics on a dedicated port/listener, separate from the API server
	// above — so a NetworkPolicy can grant the monitoring namespace scrape
	// access without also granting it access to the tenant-facing/gateway
	// API surface. Mirrors iam-tender-acl's identical split.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", gincommon.MetricsHandler())
	metricsServer := &http.Server{
		Addr:              ":" + envOr("METRICS_PORT", "9090"),
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	log.Info("service starting", map[string]interface{}{
		"service": cfg.ServiceName,
		"version": cfg.BuildVersion,
		"env":     appEnv,
		"addr":    srv.Addr,
	})
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", map[string]interface{}{"error": err.Error()})
		}
	}()
	go func() {
		log.Info("metrics server starting", map[string]interface{}{"addr": metricsServer.Addr})
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics server error", map[string]interface{}{"error": err.Error()})
		}
	}()

	<-quit
	log.Info("shutdown signal received — draining", nil)

	// Order, and the budget it must fit in terminationGracePeriodSeconds (75):
	//   1. in parallel: HTTP Shutdown (≤ 30s, the request timeout) and every
	//      SQS consumer's Stop (each ≤ SQS_DRAIN_TIMEOUT, 15s in Helm) — both
	//      stop new work and drain what is in flight;
	//   2. outbox.Stop (≤ OUTBOX_DRAIN_TIMEOUT, 30s) — publishes committed
	//      events, after the consumers so none can enqueue past the drain;
	//   3. cancelBackground — exporters stop;
	//   4. pool drains on their own budget (poolDrainTimeout, 10s).
	// max(30, 15) + 30 + 10 = 70s, leaving a 5s buffer.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), httpDrainTimeout)
	defer cancel()
	var drain sync.WaitGroup
	drain.Add(1)
	go func() {
		defer drain.Done()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Error("HTTP server shutdown error", map[string]interface{}{"error": err.Error()})
		}
		if err := metricsServer.Shutdown(shutdownCtx); err != nil {
			log.Error("metrics server shutdown error", map[string]interface{}{"error": err.Error()})
		}
	}()
	for _, wc := range sqsConsumers {
		drain.Add(1)
		go func() {
			defer drain.Done()
			if err := wc.cons.Stop(); err != nil {
				log.Error("SQS consumer stop error", map[string]interface{}{"error": err.Error()})
			}
		}()
	}
	drain.Wait()
	if err := outboxRunner.Stop(); err != nil {
		log.Error("outbox runner drain error", map[string]interface{}{"error": err.Error()})
	}
	// RELIABILITY-6: cancelBackground AFTER outboxRunner.Stop() so the exporter
	// goroutines that tick on the background context complete their in-flight
	// Postgres queries cleanly before we cancel. Cancelling first caused those
	// goroutines to log context.Canceled at ERROR level during normal shutdown.
	cancelBackground()
	// DrainAndClose waits for in-flight WithConn/RunInTx callbacks then
	// force-closes — pgcommon's documented graceful path. It gets a fresh
	// budget: the HTTP drain context is usually spent by now. defer Close()
	// above remains a safety net for panic/early-return and is idempotent
	// after DrainAndClose returns (must not run concurrently).
	// Both pools drain in parallel on the same budget, so a slow app pool
	// cannot leave the system pool no time.
	poolCtx, cancelPool := context.WithTimeout(context.Background(), poolDrainTimeout)
	defer cancelPool()
	var pools sync.WaitGroup
	for name, p := range map[string]*pgcommon.Pool{"app": pool, "system": sysPool} {
		pools.Add(1)
		go func() {
			defer pools.Done()
			if err := p.DrainAndClose(poolCtx); err != nil {
				log.Error("pool drain error", map[string]interface{}{"pool": name, "error": err.Error()})
			}
		}()
	}
	pools.Wait()
	shutdownTracing()
	if err := gincommon.Shutdown(log); err != nil {
		log.Error("logger/tracer flush error", map[string]interface{}{"error": err.Error()})
	}
}

// ── helpers ────────────────────────────────────────────────────────────

// runMigrationsAtStartup reports whether this process migrates the schema
// itself: RUN_MIGRATIONS unset or true. The Helm chart sets it to false and
// runs cmd/migrate as a hook Job instead.
func runMigrationsAtStartup() bool {
	v := strings.TrimSpace(os.Getenv("RUN_MIGRATIONS"))
	if v == "" {
		return true
	}
	run, err := strconv.ParseBool(v)
	return err != nil || run
}

// Shutdown budgets (see the ordering comment in main).
const (
	httpDrainTimeout = 30 * time.Second
	poolDrainTimeout = 10 * time.Second
)

// pingerFunc adapts a plain func to httpadapter.Pinger so /readyz's three
// dependencies (Postgres, cache, outbox runner) — each with a different
// native health-check shape — can be passed into the router uniformly
// without the http adapter importing any concrete outbound type.
type pingerFunc func(context.Context) error

func (f pingerFunc) Health(ctx context.Context) error { return f(ctx) }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool parses key with strconv.ParseBool; unset or unparsable keeps def.
func envBool(key string, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
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

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// validateRequiredEnv panics with a descriptive message if any environment
// variable required for correct operation is absent. Dev mode relaxes
// checks so local setups without SNS/Valkey still start (with warnings).
func validateRequiredEnv(appEnv string) {
	type req struct {
		key         string
		devOK       bool // skip silently in dev/local/test
		stagingWarn bool // warn to stderr in staging instead of panicking (CONFIG-2 Option C)
		reason      string
	}
	isDevLike := appEnv == "dev" || appEnv == "local" || appEnv == "test"
	isStaging := appEnv == "staging"

	reqs := []req{
		{"VALKEY_URL", false, false, "Valkey address is required for caching"},
		{"SNS_TOPIC_MEMBERSHIP_ARN", true, false, "iam.membership.events topic ARN; events queue in outbox but never publish without it"},
		{"SNS_TOPIC_TENANT_ARN", true, false, "iam.tenant.events topic ARN; TenantCreated/TrialStarted queue but never publish without it"},
		{"GLUE_REGISTRY_MEMBERSHIP_NAME", true, false, "iam-membership-events Glue registry name; NoopCodec (plain JSON) used on that topic without it"},
		{"GLUE_REGISTRY_TENANT_NAME", true, false, "iam-tenant-events Glue registry name; NoopCodec (plain JSON) used on that topic without it"},
		// CONFIG-2 Option C: stagingWarn=true — panic in production only, warn to
		// stderr in staging, skip in dev/local/test. Unset base URL makes these
		// two clients fail OPEN (not closed): RP fabricates a random Keycloak user
		// ID on CreateInvitedUser and silently no-ops all other RP calls; Workflow
		// silently reports zero active workflows — the opposite of the documented
		// fail-CLOSED contract (§20.7, P-6/P-8). Safe in local dev (intentional
		// fallback); dangerous in staging (silent wrong behaviour); fatal in prod.
		// CD-20 fix: GetIDPCount (plan-change-check, I-18) falls back to
		// (0, nil) when the base URL is empty — silently allowing any downgrade.
		// This is intentional only in local dev. In staging and production it is
		// a misconfiguration that must abort startup (stagingWarn changed false).
		{"REALM_PROVISIONER_BASE_URL", true, false, "Realm Provisioner base URL; unset makes RP calls fail OPEN (fabricated KC UUIDs, silent no-ops for GetIDPCount) — wrong in staging, fatal in production (CD-20)"},
		{"WORKFLOW_SERVICE_BASE_URL", true, true, "Workflow Service base URL; unset makes delegate-impact report zero workflows (fail-open, opposite of WFI-13 intent) — wrong in staging, fatal in production"},
		// CatalogAdminClient is NOT fail-open (§20.7): an absent base URL
		// causes catalog_unavailable 503 on every write path that touches
		// department or plan resolution (ProvisioningService.TrialSignup,
		// MembershipService.Invite, DeptMembershipService.Assign, etc.).
		// devOK: false — there is no safe local fallback; developers must run
		// iam-catalog-admin or set CATALOG_ADMIN_BASE_URL to a stub.
		{"CATALOG_ADMIN_BASE_URL", false, false, "CatalogAdminClient is NOT fail-open — absent value causes catalog_unavailable 503 on every write path that touches department/plan resolution (§20.7)"},
		// Unset falls back to the RLS-scoped app DSN (see sysDSN below) —
		// every cross-tenant query (I-16, gauge exporters, reconciler
		// sweeps) then RLS-filters to zero rows instead of erroring, so no
		// alert fires. devOK because local/dev commonly runs single-role
		// Postgres with no separate migrator role.
		{"SYSTEM_DATABASE_URL", true, false, "BYPASSRLS org_membership_migrator DSN; without it cross-tenant queries silently RLS-filter to zero rows with no alert"},
		// AUDIT_LOG_BASE_URL is fail-open (the client logs a WARN and
		// skips all AL-5 writes when unset), but unset in staging means
		// audit records never reach iam-audit-log, which is wrong for
		// compliance testing. devOK=true: local dev rarely runs
		// iam-audit-log. stagingWarn=true so a staging misconfiguration
		// surfaces in stderr without blocking the deploy.
		{"AUDIT_LOG_BASE_URL", true, true, "iam-audit-log AL-5 base URL; unset skips audit writes silently — compliance records missing in staging/prod"},
	}
	var missing []string
	for _, r := range reqs {
		if os.Getenv(r.key) != "" {
			continue
		}
		if r.devOK && isDevLike {
			continue // intentional local-dev convenience — skip silently
		}
		if r.stagingWarn && isStaging {
			// CONFIG-2 Option C: warn to stderr in staging rather than panicking.
			// Logger is not yet initialised at this point, so fmt.Fprintf is the
			// only available sink. This makes the misconfiguration observable in
			// staging logs without blocking a deployment that can still function
			// partially.
			fmt.Fprintf(os.Stderr, "WARNING: %s is not set in staging — %s\n", r.key, r.reason)
			continue
		}
		missing = append(missing, r.key+": "+r.reason)
	}
	// DATABASE_URL optional when PG_HOST + PG_USER + PG_PASSWORD are set.
	if os.Getenv("DATABASE_URL") == "" &&
		(os.Getenv("PG_HOST") == "" || os.Getenv("PG_USER") == "" || os.Getenv("PG_PASSWORD") == "") {
		missing = append(missing, "DATABASE_URL (or PG_HOST + PG_USER + PG_PASSWORD): PostgreSQL connection required")
	}
	// MIGRATION_DATABASE_URL is required with PG_BOUNCER_MODE=true, in every
	// environment: migrations take a session-scoped advisory lock, which
	// transaction pooling silently breaks, and the fallback would be the
	// PgBouncer DSN. DATABASE_URL being set is no exemption — it is the app
	// DSN and points at PgBouncer too (platform-pgcommon's own migrate CLI
	// refuses the same fallback).
	if bouncer, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv("PG_BOUNCER_MODE"))); bouncer && os.Getenv("MIGRATION_DATABASE_URL") == "" {
		missing = append(missing, "MIGRATION_DATABASE_URL: required when PG_BOUNCER_MODE=true — migrations must bypass PgBouncer")
	}
	// TLS Valkey in production.
	isProd := appEnv == "prod" || appEnv == "staging"
	if isProd {
		if v := os.Getenv("VALKEY_URL"); v != "" && !strings.HasPrefix(v, "rediss://") {
			missing = append(missing, "VALKEY_URL: must use rediss:// in production/staging")
		}
	}
	if len(missing) > 0 {
		msg := "startup aborted — required env vars missing or misconfigured:\n"
		for _, m := range missing {
			msg += "  • " + m + "\n"
		}
		panic(msg)
	}
	// No GLUE_REGISTRY_*+PG_BOUNCER_MODE mutual-exclusion check here — a
	// prior version of this guard assumed the Glue codec's 18-byte binary
	// header gets written into outbox_events.payload, which would indeed
	// corrupt that JSONB column under transaction pooling. That model is
	// incorrect: WithCodec's Encode step runs transiently in the SNS
	// publisher immediately before publish, and platform-events
	// base64-wraps the encoded bytes into a JSON string before ever
	// touching the envelope (see port.Codec's doc comment in
	// platform-events) — outbox_events always stores plain, validated JSON
	// regardless of which codec is configured. PG_BOUNCER_MODE and Glue
	// registries are configured together in production (see
	// deploy/helm/values.yaml) and that combination is safe.
}

// noopPublisher usage below keeps encoding/json + noopPublisher usage
// referenced in main so imports don't drift under -tags=integration builds.
var _ = json.RawMessage(nil)
