// Package main is the composition root for the Org & Membership service.
// It wires pgcommon.NewPool with the transaction-local RLS GUC provider
// (RLS-6), runs domain migrations, applies the outbox schema, wires the
// two-topic RoutingPublisher, and serves the HTTP surface.
//
// Phase 0 wires infrastructure only — health/readyz/metrics, outbox runner,
// and empty router group. Business services and handlers arrive in Phase 2.
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
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"

	_ "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/docs/swagger"
	consumeradapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/consumer"
	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/http"
	catalogadminclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/catalogadmin"
	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/eventbus"
	groupmappingclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/groupmappingclient"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	realmprovisionerclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/realmprovisioner"
	userprofileclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/userprofile"
	valkeyadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/valkey"
	workflowclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/workflow"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/logger"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/migrate"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/jackc/pgx/v5/pgxpool"
)

// buildVersion is injected by -ldflags at build time (see Dockerfile / Makefile).
var buildVersion = "dev"

func main() {
	appEnv := envOr("APP_ENV", "dev")
	if appEnv != "dev" {
		gin.SetMode(gin.ReleaseMode)
	}

	// Fail fast on missing required env before touching any dependencies —
	// operators get a clear startup error instead of a runtime failure deep
	// in the first request path.
	validateRequiredEnv(appEnv)

	// ── 1. Logger ─────────────────────────────────────────────────────────
	log, err := logger.NewLogger(appEnv)
	if err != nil {
		panic("init logger: " + err.Error())
	}

	metrics.Register()

	// ── 2. Tracing (opt-in) ───────────────────────────────────────────────
	var shutdownTracing func()
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		shutdownTracing = gincommon.InitTracingFromEnv()
	} else {
		shutdownTracing = func() {}
	}

	cfg := gincommon.Config{
		Logger:       log,
		ServiceName:  envOr("APP_NAME", "iam-org-membership"),
		BuildVersion: envOr("BUILD_VERSION", buildVersion),
	}

	// ── 3. Database ───────────────────────────────────────────────────────
	dsn := pgadapter.DSNFromEnv()
	migrationDSN := pgadapter.MigrationDSNFromEnv()

	maxConns, _ := strconv.Atoi(envOr("PG_MAX_CONNS", "20"))
	minConns, _ := strconv.Atoi(envOr("PG_MIN_CONNS", "0"))
	slowQueryThreshold := 200 * time.Millisecond
	if s := os.Getenv("PG_SLOW_QUERY_THRESHOLD"); s != "" {
		if d, perr := time.ParseDuration(s); perr == nil && d > 0 {
			slowQueryThreshold = d
		}
	}

	pgBouncerMode := os.Getenv("PG_BOUNCER_MODE") == "true"
	pool, err := pgcommon.NewPool(context.Background(), pgcommon.Config{
		DSN:                dsn,
		MaxConns:           int32(maxConns),
		MinConns:           int32(minConns),
		PGBouncerMode:      pgBouncerMode,
		GUCProvider:        pgcommon.GUCSetFromContext,
		SlowQueryThreshold: slowQueryThreshold,
	})
	if err != nil {
		panic(fmt.Sprintf("connect to postgres: %v", err))
	}
	defer pool.Close()

	// sysPool: BYPASSRLS pool used by reconciler jobs, cross-tenant metric
	// exporters, and the seat-overage reconciler (LLD §4.4). In production
	// SYSTEM_DATABASE_URL must point to org_membership_migrator (RLS-4). In
	// dev it falls back to DSNFromEnv() so single-role setups keep working,
	// with a warning so the operator knows cross-tenant queries will
	// RLS-filter to zero rows.
	sysDSN := pgadapter.SystemDSNFromEnv()
	sysPool, err := pgxpool.New(context.Background(), sysDSN)
	if err != nil {
		panic(fmt.Sprintf("connect sysPool: %v", err))
	}
	defer sysPool.Close()
	if sysDSN == dsn {
		log.Warn("SYSTEM_DATABASE_URL not set — sysPool reuses app DSN; cross-tenant queries will be RLS-filtered", nil)
	}

	ctx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()

	// outbox.ApplySchema must run first so platform-events creates outbox_events
	// (with JSONB payload) before domain migration 10 converts it to TEXT.
	if err := outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: migrationDSN}); err != nil {
		panic(fmt.Sprintf("outbox schema: %v", err))
	}
	if err := pgadapter.RunMigrations(ctx, migrationDSN); err != nil {
		panic(fmt.Sprintf("domain migrations: %v", err))
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

	var snsOpts []func(*sns.Options)
	var sqsOpts []func(*sqs.Options)
	var glueOpts []func(*glue.Options)
	if ep := os.Getenv("AWS_ENDPOINT_URL"); ep != "" {
		snsOpts = append(snsOpts, func(o *sns.Options) { o.BaseEndpoint = &ep })
		sqsOpts = append(sqsOpts, func(o *sqs.Options) { o.BaseEndpoint = &ep })
		glueOpts = append(glueOpts, func(o *glue.Options) { o.BaseEndpoint = &ep })
	}
	// Retained for Phase 3 (event consumer wiring). Reference to keep the
	// imports live under go vet's unused-check.
	_ = sns.NewFromConfig(awsCfg, snsOpts...)
	_ = sqs.NewFromConfig(awsCfg, sqsOpts...)
	_ = glue.NewFromConfig(awsCfg, glueOpts...)

	// ── 6. Event codec + outbox publisher ─────────────────────────────────
	// Two-codec architecture (new flow):
	//   enqueueCodec — schema validation only (wraps NoopCodec); outbox stores plain JSON.
	//   snsCodec     — wire encoding (Glue) at SNS publish time via WithCodec.
	//
	// Phase 0: both are NoopCodec — dev works end-to-end without AWS/Glue.
	// Phase 3: replace snsCodec with a real GlueCodec; enqueueCodec stays NoopCodec.
	enqueueCodec, err := eventbusadapter.NewValidatingCodec(eventbusadapter.NoopCodec{})
	if err != nil {
		panic(fmt.Sprintf("init validating codec: %v", err))
	}
	outboxPublisher := eventbusadapter.New(cfg.ServiceName, enqueueCodec)
	// txRunner injects a tx-bound ContextEventPublisher into the ctx so
	// services can call port.EventPublisherFromContext(ctx).EnqueueCtx
	// inside a RunInTx block — state write + event insert commit together
	// (EVT-10, CONS-1..4).
	txRunner := pgadapter.NewTxRunner(pool, outboxPublisher)

	// Two-topic RoutingPublisher — wraps SNS publishers per topic. In dev
	// (no SNS_TOPIC_*_ARN set) both lanes fall back to the noop publisher.
	// Phase 3: wire GlueCodec via events.WithCodec once platform-events exposes it.
	membershipPub, err := buildTopicPublisher(os.Getenv("SNS_TOPIC_MEMBERSHIP_ARN"))
	if err != nil {
		panic(fmt.Sprintf("build membership publisher: %v", err))
	}
	tenantPub, err := buildTopicPublisher(os.Getenv("SNS_TOPIC_TENANT_ARN"))
	if err != nil {
		panic(fmt.Sprintf("build tenant publisher: %v", err))
	}
	routingPublisher := eventbusadapter.NewRoutingPublisher(membershipPub, tenantPub)

	// ── 7. Outbox runner ─────────────────────────────────────────────────
	outboxRunner, err := outbox.NewRunner(outbox.Config{
		Pool:               pool,
		Publisher:          routingPublisher,
		PollInterval:       envDuration("OUTBOX_POLL_INTERVAL", 500*time.Millisecond),
		BatchSize:          envInt("OUTBOX_BATCH_SIZE", 50),
		MaxAttempts:        envInt("OUTBOX_MAX_ATTEMPTS", 5),
		DrainTimeout:       envDuration("OUTBOX_DRAIN_TIMEOUT", 30*time.Second),
		PublishConcurrency: envInt("OUTBOX_PUBLISH_CONCURRENCY", 4),
		PublishTimeout:     envDuration("OUTBOX_PUBLISH_TIMEOUT", 10*time.Second),
		StartupJitter:      envDuration("OUTBOX_STARTUP_JITTER", 2*time.Second),
		ClaimLeaseDuration: envDuration("OUTBOX_CLAIM_LEASE_DURATION", 10*time.Minute),
	})
	if err != nil {
		panic(fmt.Sprintf("create outbox runner: %v", err))
	}

	go func() {
		if err := outboxRunner.Start(ctx); err != nil {
			log.Error("outbox runner stopped", map[string]interface{}{"error": err.Error()})
		}
	}()

	// ── 7b. Inbound SQS consumers (§7.1) ─────────────────────────────────
	// One handler covers both tenant-orgm-q + billing-orgm-q; EVT-14/15/16
	// guards live inside the handler. Both queues share the same consumer
	// identity in processed_events (§16 A33 / PE-1).
	skew := envDuration("MAX_LIFECYCLE_EVENT_SKEW_SECONDS", 300*time.Second)
	membershipConsumer := consumeradapter.NewMembershipEventConsumer(pool, outboxPublisher, skew, nil)

	var sqsConsumers []events.Consumer
	if url := os.Getenv("SQS_TENANT_ORGM_QUEUE_URL"); url != "" {
		cons, err := events.NewSQSConsumerWithClient(
			events.SQSConfig{QueueURL: url, Region: envOr("AWS_REGION", "ap-south-1")},
			sqs.NewFromConfig(awsCfg, sqsOpts...),
			membershipConsumer.Handle,
			events.WithConcurrency(envInt("SQS_TENANT_ORGM_CONCURRENCY", 4)),
		)
		if err != nil {
			panic(fmt.Sprintf("build tenant-orgm-q consumer: %v", err))
		}
		sqsConsumers = append(sqsConsumers, cons)
		log.Info("tenant-orgm-q consumer wired", map[string]interface{}{"queue_url": url})
	} else {
		log.Warn("SQS_TENANT_ORGM_QUEUE_URL unset — tenant lifecycle consumer disabled", nil)
	}
	if url := os.Getenv("SQS_BILLING_ORGM_QUEUE_URL"); url != "" {
		cons, err := events.NewSQSConsumerWithClient(
			events.SQSConfig{QueueURL: url, Region: envOr("AWS_REGION", "ap-south-1")},
			sqs.NewFromConfig(awsCfg, sqsOpts...),
			membershipConsumer.Handle,
			events.WithConcurrency(envInt("SQS_BILLING_ORGM_CONCURRENCY", 2)),
		)
		if err != nil {
			panic(fmt.Sprintf("build billing-orgm-q consumer: %v", err))
		}
		sqsConsumers = append(sqsConsumers, cons)
		log.Info("billing-orgm-q consumer wired", map[string]interface{}{"queue_url": url})
	} else {
		log.Warn("SQS_BILLING_ORGM_QUEUE_URL unset — billing consumer disabled", nil)
	}

	for _, cons := range sqsConsumers {
		cons := cons
		go func() {
			if err := cons.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("SQS consumer stopped", map[string]interface{}{"error": err.Error()})
			}
		}()
	}

	// ── 8. Router ─────────────────────────────────────────────────────────
	r := gin.New()
	// Return 405 Method Not Allowed (with Allow header) when a path exists
	// but the HTTP method is not registered, instead of the default 404.
	// Clients get a precise signal ("wrong method") rather than "not found".
	r.HandleMethodNotAllowed = true

	// 1 MB body cap to prevent memory exhaustion via oversized JSON payloads.
	r.Use(func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
		c.Next()
	})
	// 30 s hard deadline on every request.
	r.Use(gincommon.TimeoutMiddleware(30 * time.Second))
	// Panic recovery, request-ID, tracing, correlation, metrics, logging.
	r.Use(gincommon.ObservabilityMiddlewares(cfg)...)
	// G-13: normalize platform-gincommon 401 responses to include code field.
	r.Use(httpadapter.NormalizeAuthErrors())

	// Public infra endpoints (registered before RequireAuth so LB probes
	// with no headers still get 200).
	r.GET("/healthz", healthzHandler())
	r.GET("/readyz", readyzHandler(pool, cache, outboxRunner))
	r.GET("/metrics", metricsHandler())

	// ── API + Event docs surface ────────────────────────────────────────
	// Mirrors sibling iam-user-profile2 exactly:
	//   /swagger/*any    Swagger UI (REST APIs, custom BCBP theme, Try-it-out)
	//     /swagger/index.css              → custom purple-gradient theme
	//     /swagger/swagger-initializer.js → KeepModelTogglePlugin bootstrap
	//     /swagger/doc.json               → Swagger 2.0 spec (regenerated by `make swag` from handler annotations)
	//   /asyncapi        Custom event catalog page (13 events, 2 topics,
	//                    dark theme, sidebar search, deep-linkable schemas)
	//   /asyncapi.yaml   Raw AsyncAPI 3.0 spec (embedded YAML)
	if appEnv != "production" || envOr("DOCS_ENABLED", "false") == "true" {
		docsSecHeaders := func(c *gin.Context) {
			c.Header("X-Frame-Options", "DENY")
			c.Header("X-Content-Type-Options", "nosniff")
			c.Header("Content-Security-Policy",
				"default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; "+
					"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; "+
					"img-src 'self' data:; font-src 'self' data: https://fonts.gstatic.com; "+
					"connect-src 'self'")
			c.Next()
		}

		// In production, require DOCS_AUTH_TOKEN as a Bearer token; else pass through.
		var docsAuthMiddleware gin.HandlerFunc = func(c *gin.Context) { c.Next() }
		if appEnv == "production" {
			if docsToken := os.Getenv("DOCS_AUTH_TOKEN"); docsToken != "" {
				docsAuthMiddleware = func(c *gin.Context) {
					if c.GetHeader("Authorization") != "Bearer "+docsToken {
						c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
							"code":    "unauthorized",
							"message": "docs require Authorization: Bearer <DOCS_AUTH_TOKEN>",
						})
						return
					}
					c.Next()
				}
			} else {
				log.Warn("DOCS_ENABLED in production without DOCS_AUTH_TOKEN — docs surface is unauthenticated", nil)
			}
		}

		stdSwagger := ginSwagger.WrapHandler(swaggerFiles.Handler)
		r.GET("/swagger/*any", docsSecHeaders, docsAuthMiddleware, func(c *gin.Context) {
			switch {
			case strings.HasSuffix(c.Request.URL.Path, "/index.css"):
				httpadapter.SwaggerThemeHandler(c)
			case strings.HasSuffix(c.Request.URL.Path, "/swagger-initializer.js"):
				httpadapter.SwaggerInitializerHandler(c)
			default:
				stdSwagger(c)
			}
		})
		r.GET("/asyncapi", docsSecHeaders, docsAuthMiddleware, httpadapter.AsyncAPIHandler)
		// Raw AsyncAPI spec (unauthenticated within the docs-enabled block).
		// The OpenAPI spec is served by ginSwagger at /swagger/doc.json.
		r.GET("/asyncapi.yaml", docsSecHeaders, docsAuthMiddleware, httpadapter.AsyncAPIYAMLHandler)
	}

	// ── 7c. Business-observability exporter goroutines (§11.2) ──────────
	// Populate iam_tenant_ownerless / iam_realm_sync_pending /
	// iam_seat_overage_active / iam_pending_invitations_stale gauges every
	// 5 minutes from the sysPool (BYPASSRLS). Follows the sibling
	// iam-user-profile2 pattern — exporters live as goroutines, not
	// CronJobs, so the running server pod is the source of truth.
	runBusinessExporters(ctx, sysPool, log)

	// ── 8b. Repositories, services, handlers ─────────────────────────────
	tenantRepo := pgadapter.NewTenantRepository(pool)
	tenantDeptRepo := pgadapter.NewTenantDepartmentRepository(pool)
	membershipRepo := pgadapter.NewMembershipRepository(pool)
	tenantRoleRepo := pgadapter.NewTenantRoleRepository(pool)
	deptMemRepo := pgadapter.NewDeptMembershipRepository(pool)
	deptRoleLabelRepo := pgadapter.NewDeptRoleLabelRepository(pool)
	delegationRepo := pgadapter.NewDelegationRepository(pool)
	aclRepo := pgadapter.NewTenderACLRepository(pool)
	invitationRepo := pgadapter.NewInvitationRepository(pool)

	// Outbound clients — Phase 2 fail-open stubs; Phase 4 wires real HTTP.
	rpClient := realmprovisionerclient.New()
	upClient := userprofileclient.New()
	wfClient := workflowclient.New()
	// catalogAdminClient/catalogReader: departments/plans read paths go
	// through catalog-admin-config's CAT-I1/CAT-I2 + a local cache.
	// Migration-runbook Phase 4 (LLD §12 step 4) completed the cutover —
	// catalog-admin-config is now the sole writer too; O-1/O-2/O-3/O-5/O-6
	// and the local departments/plans tables have been removed from this
	// service entirely.
	catalogAdminClient := catalogadminclient.New()
	catalogReader := service.NewCatalogService(catalogAdminClient, cache)
	// groupMappingClient: I-10's mapping-resolution step goes through Group
	// Mapping Service's GM-I1 behind the om:grm/gdm/gtrm cache (ADR-0007
	// Wave 2). P-14..P-29 admin CRUD and the local group-mapping tables
	// have been fully removed from this service — Group Mapping Service is
	// now the sole owner of that config surface.
	groupMappingClient := groupmappingclient.New()

	seatOverageDays := envInt("SEAT_OVERAGE_GRACE_DAYS", 30)
	invitationExpiryDays := envInt("INVITATION_EXPIRY_DAYS", 7)
	reinviteCooldownMin := envInt("INVITE_REINVITE_COOLDOWN_MINUTES", 60) // PI-11
	inviteMaxPerHour := envInt("INVITE_MAX_PER_TENANT_PER_HOUR", 200)     // PI-12

	authzSvc := service.NewAuthZService(pool, catalogReader, cache)
	provisioningSvc := service.NewProvisioningService(pool, tenantRepo, membershipRepo, tenantRoleRepo, deptMemRepo, deptRoleLabelRepo, tenantDeptRepo, catalogReader, delegationRepo, aclRepo, catalogReader, txRunner, cache, rpClient)
	tenantSvc := service.NewTenantService(tenantRepo, cache, rpClient)
	deptSvc := service.NewDepartmentService(catalogReader, tenantDeptRepo, cache)
	membershipSvc := service.NewMembershipService(membershipRepo, tenantRoleRepo, deptMemRepo, delegationRepo, aclRepo, tenantRepo, invitationRepo, cache, rpClient, wfClient, txRunner, nil, seatOverageDays)
	deptMemSvc := service.NewDeptMembershipService(deptMemRepo, membershipRepo, tenantDeptRepo, catalogReader, delegationRepo, wfClient, cache, txRunner)
	roleLabelSvc := service.NewRoleLabelService(deptRoleLabelRepo, cache)
	groupMappingSvc := service.NewGroupMappingService(membershipRepo, tenantRoleRepo, deptMemRepo, txRunner, cache, groupMappingClient)
	reviewWindowDays := envInt("DELEGATION_REVIEW_WINDOW_DAYS", 90) // superseded by tenants.delegation_review_window_days (§16 A71); kept as fallback
	delegationSvc := service.NewDelegationService(delegationRepo, membershipRepo, upClient, tenantRepo, txRunner, reviewWindowDays)
	aclSvc := service.NewTenderACLService(aclRepo, membershipRepo)
	invitationSvc := service.NewInvitationService(invitationRepo, membershipRepo, tenantRoleRepo, deptMemRepo, tenantRepo, rpClient, cache, txRunner, nil, invitationExpiryDays).
		WithReinviteCooldown(time.Duration(reinviteCooldownMin) * time.Minute).
		WithMaxInvitesPerHour(inviteMaxPerHour)
	operatorSvc := service.NewOperatorService(pool, tenantRepo, tenantRoleRepo, membershipRepo, cache, txRunner)

	tenantH := httpadapter.NewTenantHandler(tenantSvc)
	deptH := httpadapter.NewDepartmentHandler(deptSvc)
	membershipH := httpadapter.NewMembershipHandler(membershipSvc)
	deptMemH := httpadapter.NewDeptMembershipHandler(deptMemSvc)
	roleLabelH := httpadapter.NewRoleLabelHandler(roleLabelSvc)
	delegationH := httpadapter.NewDelegationHandler(delegationSvc)
	aclH := httpadapter.NewACLHandler(aclSvc)
	invitationH := httpadapter.NewInvitationHandler(invitationSvc)
	operatorH := httpadapter.NewOperatorHandler(operatorSvc)
	internalH := httpadapter.NewInternalHandler(provisioningSvc, authzSvc, membershipSvc, invitationSvc, groupMappingSvc, aclSvc, tenantSvc)

	// Protected API group — GUCBridge writes the tx-local RLS GUC on every
	// checkout (RLS-6). Order: ProtectedMiddlewares (auth + context) →
	// GUCBridge → RequireJSONContentType → handlers.
	protected := append(
		gincommon.ProtectedMiddlewares(cfg),
		httpadapter.GUCBridgeMiddleware(),
		httpadapter.RequireJSONContentType(),
	)
	v1 := r.Group("/api/v1", protected...)
	{
		// TRIAL-4 / §16 A53 defense-in-depth: block API access to tenants in
		// terminal-ish lifecycle states (trial_expired/suspended/offboarded)
		// and enforce read-only on cancelled. Applied ONLY to public routes;
		// operator + internal groups below bypass this by design so O-7
		// reassign, TrialReactivated consumer, etc. can restore a tenant.
		activeTenantGate := httpadapter.RequireActiveTenant(tenantRepo)
		activeMemberGate := httpadapter.RequireActiveMembership(membershipRepo)
		tenants := v1.Group("/tenants", activeTenantGate, activeMemberGate)
		// Tenant — P-1, P-2
		tenants.GET("/:id", tenantH.Get)
		tenants.PATCH("/:id", tenantH.Patch)

		// Departments (tenant-scoped) — P-3, P-24, P-25
		tenants.GET("/:id/departments", deptH.List)
		tenants.POST("/:id/departments", deptH.Activate)
		tenants.PATCH("/:id/departments/:dept_id", deptH.Patch)

		// Members — P-4, P-5, P-6, P-7, P-8, P-26, P-27, P-28
		tenants.GET("/:id/members", membershipH.List)
		tenants.GET("/:id/members/:user_id", membershipH.Get)
		tenants.POST("/:id/members", invitationH.Invite)                                      // P-6 (invite)
		tenants.PATCH("/:id/members/:user_id", membershipH.Patch)                             // P-7
		tenants.DELETE("/:id/members/:user_id", membershipH.Remove)                           // P-8 (§8.8)
		tenants.POST("/:id/users/:user_id/removal-resolution", membershipH.RemovalResolution) // P-26 (§8.8.3)
		tenants.PUT("/:id/members/:user_id/roles", membershipH.ReconcileRoles)
		tenants.GET("/:id/seat-usage", membershipH.SeatUsage)

		// Dept memberships — P-9, P-10, P-11
		tenants.GET("/:id/departments/:dept_id/members", deptMemH.List)
		tenants.PUT("/:id/departments/:dept_id/members/:user_id", deptMemH.Assign)
		tenants.DELETE("/:id/departments/:dept_id/members/:user_id", deptMemH.Remove)

		// Role labels — P-12, P-13
		tenants.GET("/:id/roles", roleLabelH.List)
		tenants.PATCH("/:id/roles/:role_code", roleLabelH.Patch)

		// Group mappings (P-14, P-15, P-16, P-17, P-29) — retired, moved
		// to Group Mapping Service (GM-1..GM-6). IDs never reused.

		// Tender ACL — P-21, P-22, P-23
		tenants.GET("/:id/tenders/:tender_id/acl", aclH.List)
		tenants.POST("/:id/tenders/:tender_id/acl", aclH.Grant)
		tenants.DELETE("/:id/tenders/:tender_id/acl/:user_id", aclH.Revoke)

		// Invitations — P-30, P-31
		tenants.GET("/:id/invitations", invitationH.List)
		tenants.DELETE("/:id/invitations/:invitation_id", invitationH.Revoke)

		// Delegations — P-18, P-19, P-20, P-32, P-33 (tenant-scoped via requestctx).
		// Same TRIAL-4 gate as /tenants — a trial_expired tenant cannot
		// create or list delegations.
		v1.GET("/delegations", activeTenantGate, activeMemberGate, delegationH.List)
		v1.POST("/delegations", activeTenantGate, activeMemberGate, delegationH.Create)
		v1.DELETE("/delegations/:id", activeTenantGate, activeMemberGate, delegationH.Cancel)
		v1.POST("/delegations/:id/extend", activeTenantGate, activeMemberGate, delegationH.Extend)
		v1.POST("/delegations/:id/reassign", activeTenantGate, activeMemberGate, delegationH.Reassign)

		// Operator routes — AUTH-6 defense-in-depth (RequireOperatorRole
		// middleware + handler re-check inside each operator handler).
		// O-1/O-2/O-3 (departments) and O-5/O-6 (plans) moved to the
		// Catalog / Admin Config Service (migration-runbook Phase 4, LLD
		// §12 step 4).
		op := v1.Group("/operator", httpadapter.RequireOperatorRole())
		op.PATCH("/tenants/:id/feature-flags", operatorH.SetFeatureFlags) // O-4
		op.POST("/tenants/:id/reassign-owner", operatorH.ReassignOwner)   // O-7

		// Internal /api/v1/internal/* routes — mTLS + iam-system role
		// (RLS-5, IAPI-2, AUTH-5). NetworkPolicy is the primary defence;
		// RequireSystemRole is defense-in-depth.
		internal := v1.Group("/internal", httpadapter.RequireSystemRole())
		internal.POST("/tenants", internalH.ProvisionTenant)                                           // I-1
		internal.PATCH("/tenants/:id", internalH.PatchTenantRealm)                                     // I-2
		internal.POST("/tenants/:id/members", internalH.AddMember)                                     // I-3 (§8.10 accept + JIT add)
		internal.POST("/tenants/:id/dept-memberships", internalH.AssignFromGroups)                     // I-10 (§8.5 SAML JIT)
		internal.PATCH("/tenants/:id/members/:user_id", internalH.PatchMemberLifecycle)                // I-4
		internal.DELETE("/tenants/:id/members/:user_id", internalH.DeleteMember)                       // I-5
		internal.GET("/users/:id/memberships", internalH.GetMemberships)                               // I-8 HOT PATH
		internal.GET("/tenants/:id/locale", internalH.GetLocale)                                       // I-9
		internal.GET("/tenants/:id/mfa-freshness", internalH.GetMFAFreshness)                          // I-14 (§16 A72)
		internal.GET("/tenants/:id/seat-usage", internalH.GetSeatUsage)                                // I-11
		internal.GET("/tenants/:id/tenders/:tender_id/acl/:user_id", internalH.CheckTenderAccess)      // I-12
		internal.POST("/tenants/:id/tenders/:tender_id/assignee-override", internalH.AssigneeOverride) // I-13
	}
	httpadapter.RegisterValidators()

	// ── 9. Graceful shutdown ─────────────────────────────────────────────
	srv := &http.Server{
		Addr:         ":" + envOr("APP_PORT", "8080"),
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 35 * time.Second, // 30s app timeout + 5s buffer
		IdleTimeout:  60 * time.Second,
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

	<-quit
	log.Info("shutdown signal received — draining", nil)

	// Order matters (sibling convention):
	//   1. HTTP Shutdown  — stop accepting new requests, drain in-flight
	//   2. outbox.Stop    — its DrainTimeout publishes committed events
	//   3. cancelBg       — exporters + consumers stop
	//   4. cache.Close    — flush connection pool
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("HTTP server shutdown error", map[string]interface{}{"error": err.Error()})
	}
	if err := outboxRunner.Stop(); err != nil {
		log.Error("outbox runner drain error", map[string]interface{}{"error": err.Error()})
	}
	for _, cons := range sqsConsumers {
		if err := cons.Stop(); err != nil {
			log.Error("SQS consumer stop error", map[string]interface{}{"error": err.Error()})
		}
	}
	cancelBackground()
	shutdownTracing()
	if err := gincommon.Shutdown(log); err != nil {
		log.Error("logger/tracer flush error", map[string]interface{}{"error": err.Error()})
	}
}

// ── helpers ────────────────────────────────────────────────────────────

// buildTopicPublisher returns an SNS publisher for topicARN, or a noop
// publisher when the ARN is empty (dev/test). Phase 3: add GlueCodec option.
func buildTopicPublisher(topicARN string) (events.Publisher, error) {
	if topicARN == "" {
		return eventbusadapter.NoopPublisher{}, nil
	}
	return events.NewSNSPublisher(events.SNSConfig{
		TopicARN:    topicARN,
		Region:      envOr("AWS_REGION", "ap-south-1"),
		EndpointURL: os.Getenv("AWS_ENDPOINT_URL"),
	})
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
		key    string
		devOK  bool
		reason string
	}
	reqs := []req{
		{"VALKEY_URL", false, "Valkey address is required for caching"},
		{"SNS_TOPIC_MEMBERSHIP_ARN", true, "iam.membership.events topic ARN; events queue in outbox but never publish without it"},
		{"SNS_TOPIC_TENANT_ARN", true, "iam.tenant.events topic ARN; TenantCreated/TrialStarted queue but never publish without it"},
		{"GLUE_REGISTRY_NAME", true, "Glue registry name; NoopCodec used without it"},
	}
	var missing []string
	for _, r := range reqs {
		if os.Getenv(r.key) != "" {
			continue
		}
		if r.devOK && (appEnv == "dev" || appEnv == "development" || appEnv == "local" || appEnv == "test") {
			continue
		}
		missing = append(missing, r.key+": "+r.reason)
	}
	// DATABASE_URL optional when PG_HOST + PG_USER + PG_PASSWORD are set.
	if os.Getenv("DATABASE_URL") == "" &&
		(os.Getenv("PG_HOST") == "" || os.Getenv("PG_USER") == "" || os.Getenv("PG_PASSWORD") == "") {
		missing = append(missing, "DATABASE_URL (or PG_HOST + PG_USER + PG_PASSWORD): PostgreSQL connection required")
	}
	// MIGRATION_DATABASE_URL required with PG_BOUNCER_MODE=true (advisory locks are session-scoped).
	if os.Getenv("MIGRATION_DATABASE_URL") == "" && os.Getenv("PG_BOUNCER_MODE") == "true" &&
		os.Getenv("DATABASE_URL") == "" {
		missing = append(missing, "MIGRATION_DATABASE_URL: required when PG_BOUNCER_MODE=true — migrations must bypass PgBouncer")
	}
	// TLS Valkey in production.
	isProd := appEnv == "production" || appEnv == "staging"
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
	// Glue codec writes a 18-byte binary header to outbox_events.payload —
	// not valid JSONB and corrupts the row under PgBouncer simple-protocol.
	if os.Getenv("GLUE_REGISTRY_NAME") != "" && os.Getenv("PG_BOUNCER_MODE") == "true" {
		panic("startup aborted — GLUE_REGISTRY_NAME and PG_BOUNCER_MODE=true are mutually exclusive: " +
			"the Glue 18-byte header is not valid JSONB and corrupts outbox_events.payload under transaction pooling. " +
			"Leave GLUE_REGISTRY_NAME empty (NoopCodec) when PG_BOUNCER_MODE=true.")
	}
}

// noopPublisher usage below keeps encoding/json + noopPublisher usage
// referenced in main so imports don't drift under -tags=integration builds.
var _ = json.RawMessage(nil)
