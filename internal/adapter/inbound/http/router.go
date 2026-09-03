// Package http's router.go is the single source of truth for this
// service's HTTP surface: every path, method, middleware, and route group.
// It mirrors group-mapping-jit-config's own router.go (ADR-0007 Wave 2) —
// main.go's job is to construct dependencies and call NewRouter, not to
// encode routing decisions itself. Before this file existed, main.go's
// inline route table had already drifted from test/e2e/harness_test.go's
// hand-copied duplicate (missing the activeTenantGate/activeMemberGate
// gates, missing I-... routes added later) — a single NewRouter used by
// both main.go and the e2e harness makes that drift impossible.
package http

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// Pinger is satisfied by any dependency /readyz must check. pgcommon.Pool,
// valkey.Cache, and the outbox runner are each wrapped to implement it by
// the composition root (cmd/server/main.go) — this package never imports
// those concrete outbound types directly.
type Pinger interface {
	Health(ctx context.Context) error
}

// DocsConfig controls whether — and how — the interactive docs surface
// (Swagger UI, AsyncAPI catalog) is exposed. Outside production it's
// always on; in production it's opt-in via Enabled and, if AuthToken is
// set, gated behind a bearer token so the API surface isn't exposed to
// the open internet by default. Mirrors group-mapping-jit-config's
// DocsConfig exactly.
type DocsConfig struct {
	Environment string
	Enabled     bool
	AuthToken   string
}

func (d DocsConfig) active() bool {
	return d.Environment != "production" || d.Enabled
}

// RouterConfig bundles every dependency NewRouter needs: the already-built
// handlers (composition root's job to construct), the two repositories
// used directly by gate middleware, the three readiness pingers, and the
// shared gincommon/docs configuration.
type RouterConfig struct {
	GinConfig gincommon.Config
	Docs      DocsConfig

	TenantRepo     port.TenantRepository
	MembershipRepo port.MembershipRepository

	TenantHandler         *TenantHandler
	DepartmentHandler     *DepartmentHandler
	MembershipHandler     *MembershipHandler
	DeptMembershipHandler *DeptMembershipHandler
	RoleLabelHandler      *RoleLabelHandler
	InvitationHandler     *InvitationHandler
	OperatorHandler       *OperatorHandler
	InternalHandler       *InternalHandler

	Postgres Pinger
	// SysPostgres is optional — the BYPASSRLS *pgcommon.Pool used by
	// cross-tenant reconciler/metric-exporter paths (LLD §4.4), deliberately
	// with no GUCProvider so it sees across every tenant. Same Pinger wiring
	// as Postgres above (via Ping/Health) — it's a separate physical
	// connection pool from the app pool, so a credential rotation or network
	// partition specific to that role would otherwise go unnoticed here.
	// Left nil, this check is skipped.
	SysPostgres Pinger
	Cache       Pinger
	Outbox      Pinger
}

// Router owns the Gin engine for this service.
type Router struct {
	engine *gin.Engine
}

// Handler returns the http.Handler to serve.
func (r *Router) Handler() http.Handler { return r.engine }

// NewRouter builds and wires every route this service exposes: public
// /api/v1/*, operator /api/v1/operator/*, internal /api/v1/internal/*,
// the unauthenticated infra probes, and (when enabled) the docs surface.
func NewRouter(cfg RouterConfig) *Router {
	// Set once so HandleError's unhandled-500 branch (middleware.go) logs
	// through the same gincommon-backed sink as every other log line,
	// instead of a bare stdlib log.Printf.
	errorLogger = cfg.GinConfig.Logger

	r := gin.New()
	// Return 405 Method Not Allowed (with Allow header) when a path exists
	// but the HTTP method is not registered, instead of the default 404.
	r.HandleMethodNotAllowed = true

	// 1 MB body cap to prevent memory exhaustion via oversized JSON payloads.
	r.Use(func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
		c.Next()
	})
	// 30 s hard deadline on every request.
	r.Use(gincommon.TimeoutMiddleware(30 * time.Second))
	// Panic recovery, request-ID, tracing, correlation, metrics, logging.
	r.Use(gincommon.ObservabilityMiddlewares(cfg.GinConfig)...)
	// G-13: normalize platform-gincommon 401 responses to include code field.
	r.Use(NormalizeAuthErrors())

	registerInfraRoutes(r, cfg)
	registerDocsRoutes(r, cfg)
	registerAPIRoutes(r, cfg)

	RegisterValidators()

	return &Router{engine: r}
}

// ── Infra routes (unauthenticated, registered before auth so LB probes
// with no headers still get 200) ────────────────────────────────────────

type healthHandlers struct {
	postgres    Pinger
	sysPostgres Pinger // optional — see RouterConfig.SysPostgres
	cache       Pinger
	outbox      Pinger
}

func registerInfraRoutes(r *gin.Engine, cfg RouterConfig) {
	h := &healthHandlers{postgres: cfg.Postgres, sysPostgres: cfg.SysPostgres, cache: cfg.Cache, outbox: cfg.Outbox}
	r.GET("/healthz", h.healthz)
	r.GET("/readyz", h.readyz)
	// /metrics is served on its own dedicated port (METRICS_PORT, see
	// cmd/server/main.go's metricsServer) — not on this router — so
	// scraping never shares a listener with tenant-facing/gateway traffic.
	// Mirrors iam-tender-acl's identical split.
}

// healthz is a pure liveness check: if the process can answer HTTP at all,
// it reports ok. It never inspects dependencies — see /readyz for that.
//
// @Summary      Liveness probe
// @Description  Always returns ok if the process can answer HTTP at all. Never inspects dependencies.
// @Tags         infra
// @Produce      json
// @Success      200  {object}  map[string]string
// @Router       /healthz [get]
func (h *healthHandlers) healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// readyz checks Postgres, Valkey, and the outbox runner, and reports 503
// if any of them is not ready.
//
// @Summary      Readiness probe
// @Description  Checks Postgres, Valkey, and the outbox runner. Returns 503 if any dependency is not ready.
// @Tags         infra
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      503  {object}  map[string]interface{}
// @Router       /readyz [get]
func (h *healthHandlers) readyz(c *gin.Context) {
	ctx := c.Request.Context()
	healthy := true
	checks := gin.H{}

	if err := h.postgres.Health(ctx); err != nil {
		checks["database"] = "down"
		healthy = false
	} else {
		checks["database"] = "ok"
	}
	// sysPostgres (BYPASSRLS pool) is a separate physical connection pool
	// from the app pool above — a credential rotation or network partition
	// specific to that role would otherwise go unnoticed by /readyz until
	// the next cross-tenant sweep/reconciler run failed. Optional: skipped
	// when unset (e.g. single-role local dev).
	if h.sysPostgres != nil {
		if err := h.sysPostgres.Health(ctx); err != nil {
			checks["sys_database"] = "down"
			healthy = false
		} else {
			checks["sys_database"] = "ok"
		}
	}
	// Cache is advisory (CACHE-9) everywhere else, but /readyz still fails
	// so the pod is removed from rotation while Valkey is down — otherwise
	// every cache miss silently amplifies DB load.
	if err := h.cache.Health(ctx); err != nil {
		checks["cache"] = "down"
		healthy = false
	} else {
		checks["cache"] = "ok"
	}
	if err := h.outbox.Health(ctx); err != nil {
		checks["outbox"] = "initialising"
		healthy = false
	} else {
		checks["outbox"] = "ok"
	}

	status := http.StatusOK
	overall := "ready"
	if !healthy {
		status = http.StatusServiceUnavailable
		overall = "not ready"
	}
	c.JSON(status, gin.H{"status": overall, "checks": checks})
}

// ── Docs surface ─────────────────────────────────────────────────────────

// registerDocsRoutes mirrors sibling iam-user-profile2 exactly:
//
//	/swagger/*any    Swagger UI (REST APIs, custom BCBP theme, Try-it-out)
//	/asyncapi        Custom event catalog page
//	/asyncapi.yaml   Raw AsyncAPI 3.0 spec (embedded YAML)
func registerDocsRoutes(r *gin.Engine, cfg RouterConfig) {
	if !cfg.Docs.active() {
		return
	}

	secHeaders := func(c *gin.Context) {
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; "+
				"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; "+
				"img-src 'self' data:; font-src 'self' data: https://fonts.gstatic.com; "+
				"connect-src 'self'")
		c.Next()
	}

	var authMiddleware gin.HandlerFunc = func(c *gin.Context) { c.Next() }
	if cfg.Docs.Environment == "production" {
		if cfg.Docs.AuthToken != "" {
			token := cfg.Docs.AuthToken
			authMiddleware = func(c *gin.Context) {
				if c.GetHeader("Authorization") != "Bearer "+token {
					c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
						"code":    "unauthorized",
						"message": "docs require Authorization: Bearer <DOCS_AUTH_TOKEN>",
					})
					return
				}
				c.Next()
			}
		} else if cfg.GinConfig.Logger != nil {
			cfg.GinConfig.Logger.Warn("DOCS_ENABLED in production without DOCS_AUTH_TOKEN — docs surface is unauthenticated", nil)
		}
	}

	stdSwagger := ginSwagger.WrapHandler(swaggerFiles.Handler)
	r.GET("/swagger/*any", secHeaders, authMiddleware, func(c *gin.Context) {
		switch {
		case strings.HasSuffix(c.Request.URL.Path, "/index.css"):
			SwaggerThemeHandler(c)
		case strings.HasSuffix(c.Request.URL.Path, "/swagger-initializer.js"):
			SwaggerInitializerHandler(c)
		default:
			stdSwagger(c)
		}
	})
	r.GET("/asyncapi", secHeaders, authMiddleware, AsyncAPIHandler)
	r.GET("/asyncapi.yaml", secHeaders, authMiddleware, AsyncAPIYAMLHandler)
}

// ── Business API routes ──────────────────────────────────────────────────

func registerAPIRoutes(r *gin.Engine, cfg RouterConfig) {
	tenantH := cfg.TenantHandler
	deptH := cfg.DepartmentHandler
	membershipH := cfg.MembershipHandler
	deptMemH := cfg.DeptMembershipHandler
	roleLabelH := cfg.RoleLabelHandler
	invitationH := cfg.InvitationHandler
	operatorH := cfg.OperatorHandler
	internalH := cfg.InternalHandler

	// Protected API group — GUCBridge writes the tx-local RLS GUC on every
	// checkout (RLS-6). Order: ProtectedMiddlewares (auth + context) →
	// GUCBridge → RequireJSONContentType → handlers.
	protected := append(
		gincommon.ProtectedMiddlewares(cfg.GinConfig),
		GUCBridgeMiddleware(),
		RequireJSONContentType(),
	)
	v1 := r.Group("/api/v1", protected...)

	// TRIAL-4 / §16 A53 defense-in-depth: block API access to tenants in
	// terminal-ish lifecycle states (trial_expired/suspended/offboarded)
	// and enforce read-only on cancelled. Applied ONLY to public routes;
	// operator + internal groups below bypass this by design so O-7
	// reassign, TrialReactivated consumer, etc. can restore a tenant.
	activeTenantGate := RequireActiveTenant(cfg.TenantRepo)
	activeMemberGate := RequireActiveMembership(cfg.MembershipRepo)

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
	tenants.POST("/:id/members/:user_id/reset-mfa", membershipH.ResetMFA) // P-34 (§16 OQ-8/F6)
	tenants.GET("/:id/seat-usage", membershipH.SeatUsage)

	// Dept memberships — P-9, P-10, P-11
	tenants.GET("/:id/departments/:dept_id/members", deptMemH.List)
	tenants.PUT("/:id/departments/:dept_id/members/:user_id", deptMemH.Assign)
	tenants.DELETE("/:id/departments/:dept_id/members/:user_id", deptMemH.Remove)

	// Role labels — P-12, P-13
	tenants.GET("/:id/roles", roleLabelH.List)
	tenants.PATCH("/:id/roles/:role_code", roleLabelH.Patch)

	// Group mappings (P-14, P-15, P-16, P-17, P-29) — retired, moved to
	// Group Mapping Service (GM-1..GM-6, ADR-0007 Wave 2). IDs never reused.

	// Tender ACL (P-21, P-22, P-23) — retired, moved to iam-tender-acl's
	// TAC-1/2/3 (ADR-0007 Wave 3). IDs never reused.

	// Invitations — P-30, P-31
	tenants.GET("/:id/invitations", invitationH.List)
	tenants.DELETE("/:id/invitations/:invitation_id", invitationH.Revoke)

	// Delegations (P-18, P-19, P-20, P-32, P-33) — retired, moved to the
	// standalone Delegation Service's DLG-1..5 (ADR-0008 v2). IDs never
	// reused.

	// Operator routes — AUTH-6 defense-in-depth (RequireOperatorRole
	// middleware + handler re-check inside each operator handler). O-1/O-2/
	// O-3 (departments) and O-5/O-6 (plans) moved to the Catalog / Admin
	// Config Service (migration-runbook Phase 4, LLD §12 step 4).
	op := v1.Group("/operator", RequireOperatorRole())
	op.PATCH("/tenants/:id/feature-flags", operatorH.SetFeatureFlags) // O-4
	op.POST("/tenants/:id/reassign-owner", operatorH.ReassignOwner)   // O-7

	// Internal /api/v1/internal/* routes — mTLS + iam-system role (RLS-5,
	// IAPI-2, AUTH-5). NetworkPolicy is the primary defence; RequireSystemRole
	// is defense-in-depth.
	internal := v1.Group("/internal", RequireSystemRole())
	internal.POST("/tenants", internalH.ProvisionTenant)                            // I-1
	internal.PATCH("/tenants/:id", internalH.PatchTenantRealm)                      // I-2
	internal.POST("/tenants/:id/members", internalH.AddMember)                      // I-3 (§8.10 accept + JIT add)
	internal.POST("/tenants/:id/dept-memberships", internalH.AssignFromGroups)      // I-10 (§8.5 SAML JIT)
	internal.PATCH("/tenants/:id/members/:user_id", internalH.PatchMemberLifecycle) // I-4
	internal.DELETE("/tenants/:id/members/:user_id", internalH.DeleteMember)        // I-5
	internal.GET("/users/:id/memberships", internalH.GetMemberships)                // I-8 HOT PATH
	internal.GET("/tenants/:id/locale", internalH.GetLocale)                        // I-9
	internal.GET("/tenants/:id/mfa-freshness", internalH.GetMFAFreshness)           // I-14 (§16 A72)
	internal.GET("/tenants/:id/seat-usage", internalH.GetSeatUsage)                 // I-11
	// I-12 (tender ACL check) retired, moved to iam-tender-acl's TAC-4
	// (ADR-0007 Wave 3). ID never reused.
	internal.POST("/tenants/:id/tenders/:tender_id/assignee-override", internalH.AssigneeOverride) // I-13
	internal.GET("/tenants/:id/members/:user_id/exists", internalH.CheckMemberExists)              // I-15
	// I-16 (§16 RP-C3) is cross-tenant at the DB layer (BYPASSRLS sysPool)
	// but the caller still authenticates like every other internal route —
	// gincommon's RequireAuth requires x-tenant-id on every /api/v1 request
	// regardless, so RP's client sends a sentinel uuid.Nil value for it
	// (never read by this handler, which queries sysPool unconditionally).
	internal.GET("/subscription-lapses", internalH.ListSubscriptionLapses) // I-16 (§16 RP-C3)
}
