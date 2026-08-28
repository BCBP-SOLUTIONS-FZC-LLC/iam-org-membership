package service

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ProvisioningService owns I-1 (tenant creation), I-2 (RP realm patch),
// I-4 (Keycloak lifecycle status), I-5 (delete cascade). Called by Realm
// Provisioner + Signup BFF + Event Consumer over the /internal/* mesh.
type ProvisioningService struct {
	pool        *pgcommon.Pool
	tenants     port.TenantRepository
	memberships port.MembershipRepository
	roles       port.TenantRoleRepository
	deptMems    port.DeptMembershipRepository
	labels      port.DeptRoleLabelRepository
	tenantDepts port.TenantDepartmentRepository
	depts       port.DepartmentCatalogReader
	plans       port.PlanCatalogReader
	txRunner    port.TxRunner
	cache       port.Cache
	rp          port.RealmProvisionerClient
	log         port.SlogStyleLogger // optional — see WithLogger
}

// WithLogger injects the shared gincommon-backed Logger so this service's
// tenant_ownerless_escalation alert flows through the same sink as HTTP/
// consumer/outbound-client logs instead of slog.Default(). Optional — the
// zero value falls back to the top-level slog functions.
func (s *ProvisioningService) WithLogger(log port.Logger) *ProvisioningService {
	s.log = port.NewSlogStyleLogger(log)
	return s
}

func NewProvisioningService(
	pool *pgcommon.Pool,
	tenants port.TenantRepository, memberships port.MembershipRepository,
	roles port.TenantRoleRepository, deptMems port.DeptMembershipRepository,
	labels port.DeptRoleLabelRepository, tenantDepts port.TenantDepartmentRepository,
	depts port.DepartmentCatalogReader,
	plans port.PlanCatalogReader,
	txRunner port.TxRunner, cache port.Cache, rp port.RealmProvisionerClient,
) *ProvisioningService {
	return &ProvisioningService{
		pool: pool, tenants: tenants, memberships: memberships,
		roles: roles, deptMems: deptMems, labels: labels,
		tenantDepts: tenantDepts, depts: depts,
		plans:    plans,
		txRunner: txRunner, cache: cache, rp: rp,
	}
}

// TrialSignupInput carries the I-1 payload from Realm Provisioner /
// Signup BFF after the shared trial-realm user has been created (§8.1).
type TrialSignupInput struct {
	TenantID      uuid.UUID
	Slug          string
	Name          string
	Plan          domain.TenantPlan
	OwnerUserID   uuid.UUID
	DefaultLocale string
	LicensedSeats int
}

// TrialSignup is I-1: creates the tenant row + 5 system dept activations
// + 3 role labels + owner membership + tenant_owner role grant, and emits
// TenantCreated + TrialStarted in one RunInTx (§8.1). Runs under GUCSet
// (target tenant + iam-system principal, RLS-5) so the RLS WITH CHECK
// passes on inserts into tenant-scoped tables.
//
// Returns (tenant, wasCreated, err). wasCreated=false means the tenant row
// already existed (LLD I-1 idempotent replay via ON CONFLICT (id)); we skip
// all seeding + event emission and return the existing row so the handler
// serves 200 rather than 201.
func (s *ProvisioningService) TrialSignup(ctx context.Context, req TrialSignupInput) (*domain.Tenant, bool, error) {
	// LLD line 2460: plan is required. The handler enforces this + the
	// {starter, pro, enterprise} whitelist, so an empty/unknown plan should
	// never reach here — but the belt-and-suspenders check protects direct
	// service callers (tests, future BFF) from producing a raw pgconn.PgError
	// on the DB-enum cast.
	switch req.Plan {
	case domain.PlanStarter, domain.PlanPro, domain.PlanEnterprise:
	default:
		return nil, false, domain.NewError(domain.ErrInvalidPlan,
			"plan must be one of starter, pro, enterprise").
			WithDetails(map[string]any{"received": string(req.Plan)})
	}
	// GAP-I1-3: slug format — lowercase alphanumeric and hyphens only,
	// no leading/trailing hyphens, 3–63 chars (DNS label convention).
	if !isValidSlug(req.Slug) {
		return nil, false, domain.NewError(domain.ErrValidation,
			"slug must be 3–63 lowercase alphanumeric characters or hyphens, "+
				"and must not start or end with a hyphen")
	}
	// GAP-I1-4: name max length 255 chars.
	if len(req.Name) > 255 {
		return nil, false, domain.NewError(domain.ErrValidation, "name must not exceed 255 characters")
	}
	// GAP-I1-5: default_locale basic BCP-47 format (e.g. "en-US", "fr", "zh-Hant").
	if req.DefaultLocale != "" && !isValidLocale(req.DefaultLocale) {
		return nil, false, domain.NewError(domain.ErrValidation,
			"default_locale must be a valid BCP-47 language tag (e.g. en-US, fr, zh-Hant)")
	}
	// Look up plan for trial_duration_days.
	plan, err := s.plans.PlanByCode(ctx, req.Plan)
	if err != nil {
		return nil, false, err
	}
	trialEnds := time.Now().UTC().Add(time.Duration(plan.TrialDurationDays) * 24 * time.Hour)

	// Fetch the system-department seeding set BEFORE the tx starts (outer
	// ctx, not gucCtx/txCtx) — catalog-admin-config's client+cache call has
	// no business running while a Postgres tx is open (LLD §7: CAT-I1 is a
	// mesh HTTP call, ≤30ms p99 but still a new failure mode inside a tx
	// boundary that didn't exist when this was a local repo.List call).
	// §8.1 fixes the trial-activation set to exactly these 5 system-dept
	// codes; activeOnly=true's old semantic (only activate depts that are
	// still globally active) is preserved here via the explicit IsActive
	// filter below.
	trialCodes := map[string]struct{}{
		"engineering": {},
		"design":      {},
		"procurement": {},
		"finance":     {},
		"legal":       {},
	}
	allDepts, err := s.depts.Departments(ctx)
	if err != nil {
		return nil, false, err
	}
	var trialDeptIDs []uuid.UUID
	for _, d := range allDepts {
		if !d.IsSystem || !d.IsActive {
			continue
		}
		if _, ok := trialCodes[strings.ToLower(d.Code)]; !ok {
			continue
		}
		trialDeptIDs = append(trialDeptIDs, d.ID)
	}

	// Set GUC to the new tenant + iam-system so RLS WITH CHECK passes on
	// inserts. This is the RLS-5 internal-provisioning path.
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.UserID = "iam-system"
	g.TenantID = req.TenantID.String()
	gucCtx := pgcommon.WithGUCSet(ctx, g)

	locale := req.DefaultLocale
	if locale == "" {
		locale = "en-US"
	}
	licensedSeats := req.LicensedSeats
	if licensedSeats <= 0 {
		licensedSeats = 10 // DB column default (SEAT-1, T-8)
	}

	var created *domain.Tenant
	var wasCreated bool
	err = s.txRunner.RunInTx(gucCtx, func(txCtx context.Context) error {
		// 1) Create the tenant row. LLD I-1 ON CONFLICT (id) DO NOTHING —
		// if the row already exists (idempotent replay), skip the whole
		// seeding + event storm and return the existing row so the handler
		// serves 200 instead of 201.
		t, freshInsert, err := s.tenants.Insert(txCtx, &domain.Tenant{
			ID:                   req.TenantID,
			Slug:                 req.Slug,
			Name:                 req.Name,
			Plan:                 req.Plan,
			Status:               domain.StatusTrial,
			TrialEndsAt:          &trialEnds,
			RealmID:              "trial",
			RealmType:            domain.RealmShared,
			KeycloakShard:        "shard-0",
			MFAFreshnessSeconds:  300,
			LocalAccountsEnabled: true,
			DefaultLocale:        locale,
			LicensedSeats:        licensedSeats,
		})
		if err != nil {
			return err
		}
		created = t
		wasCreated = freshInsert
		if !freshInsert {
			// Idempotent replay: the tenant already exists. Per LLD I-1
			// "no re-seed", so short-circuit the tx here.
			return nil
		}

		// 2) Activate the pre-fetched system departments (§8.1) — the
		// catalog read itself already happened outside this tx (see above).
		// If the global catalog ever grows a 6th is_system dept, it's opt-in
		// per-tenant via P-24; trial signup never auto-activates it.
		for _, deptID := range trialDeptIDs {
			if _, err := s.tenantDepts.Activate(txCtx, req.TenantID, deptID); err != nil {
				return err
			}
		}

		// 3) Seed 3 dept-role labels (DRL-1).
		if _, err := s.labels.Seed(txCtx, req.TenantID); err != nil {
			return err
		}

		// 4) Owner membership.
		mem, err := s.memberships.Insert(txCtx, &domain.TenantMembership{
			TenantID: req.TenantID,
			UserID:   req.OwnerUserID,
			Status:   domain.MembershipActive,
		})
		if err != nil {
			return err
		}

		// 5) Grant tenant_owner role.
		if _, err := s.roles.Grant(txCtx, &domain.TenantRole{
			TenantID:           req.TenantID,
			UserID:             req.OwnerUserID,
			TenantMembershipID: mem.ID,
			RoleCode:           domain.RoleTenantOwner,
			GrantedBy:          req.OwnerUserID,
		}); err != nil {
			return err
		}

		// 6) Emit TenantCreated + TrialStarted (both on iam.tenant.events
		// via RoutingPublisher).
		pub, _ := port.EventPublisherFromContext(txCtx)
		if pub != nil {
			_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
				Type: domain.EventTenantCreated, TenantID: req.TenantID,
				Subject: req.TenantID.String(), Actor: "iam-system",
				Data: domain.TenantCreatedPayload{
					TenantID: req.TenantID, Slug: req.Slug, Plan: req.Plan, Status: domain.StatusTrial,
				},
			})
			_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
				Type: domain.EventTrialStarted, TenantID: req.TenantID,
				Subject: req.TenantID.String(), Actor: "iam-system",
				Data: domain.TrialStartedPayload{
					TenantID: req.TenantID, Plan: req.Plan, TrialEndsAt: trialEnds,
				},
			})
			// Also emit TenantRoleGranted for the owner (§16 A14).
			// LLD I-1: granted_by = owner_user_id itself, since no other
			// admin exists yet — mirror that in the event payload so the
			// ActorID matches tenant_roles.granted_by written above.
			_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
				Type: domain.EventTenantRoleGranted, TenantID: req.TenantID,
				Subject: req.OwnerUserID.String(), Actor: "iam-system",
				Data: domain.TenantRoleGrantedPayload{
					UserID: req.OwnerUserID, TenantID: req.TenantID,
					RoleCode: domain.RoleTenantOwner, ActorID: req.OwnerUserID,
				},
			})
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return created, wasCreated, nil
}

// SetRealmFields is I-2: RP updates realm_id/realm_type/keycloak_shard
// atomically after dedicated-realm provisioning (TenantConverted flow).
// Routed through the shared TxRunner so unit tests can inject a fake tx.
// Returns ErrTenantNotFound (→ 404) when the id has no tenant row at all.
// Intentionally includes offboarded (deleted_at IS NOT NULL) tenants — RP
// must be able to write realm fields during KC realm cleanup even after O&M
// has soft-deleted the tenant row (§15.5 offboarding sequence).
func (s *ProvisioningService) SetRealmFields(ctx context.Context, tenantID uuid.UUID, realmID string, realmType domain.RealmType, shard string, recordVersion int64) (int64, error) {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.UserID = "iam-system"
	g.TenantID = tenantID.String()
	gucCtx := pgcommon.WithGUCSet(ctx, g)
	var newVersion int64
	if err := s.txRunner.RunInTx(gucCtx, func(txCtx context.Context) error {
		tx, ok := pgadapterTxFromContext(txCtx)
		if !ok {
			return domain.NewError(domain.ErrConflict, "tx unavailable")
		}
		// CONC-4: include record_version in WHERE clause so concurrent I-2
		// calls fail with 409 optimistic_lock_conflict (BUG-I2-2).
		// RETURNING record_version captures the post-trigger value so the
		// caller can propagate it to the response without a second round-trip.
		row := tx.QueryRow(txCtx,
			`UPDATE tenants SET realm_id = $2, realm_type = $3, keycloak_shard = $4 WHERE id = $1 AND record_version = $5 RETURNING record_version`,
			tenantID, realmID, string(realmType), shard, recordVersion)
		if err := row.Scan(&newVersion); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return err // propagate SQL errors directly
			}
			// 0 rows: probe to distinguish tenant_not_found vs stale record_version.
			// No deleted_at filter — RP must be able to act on offboarded tenants.
			var current int64
			probe := tx.QueryRow(txCtx, `SELECT record_version FROM tenants WHERE id = $1`, tenantID)
			if perr := probe.Scan(&current); perr != nil {
				return domain.NewError(domain.ErrTenantNotFound, "tenant not found")
			}
			return domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict").
				WithDetails(map[string]any{"record_version": current})
		}
		return nil
	}); err != nil {
		return 0, err
	}
	// BUG-I2-1: evict cached tenant so I-8 hot-path reads updated realm fields (CACHE-6).
	if s.cache != nil {
		_ = s.cache.Delete(gucCtx, cacheKeyTenant(tenantID), cacheKeyLocale(tenantID))
	}
	return newVersion, nil
}

// SetMembershipStatus is I-4: Event Consumer updates lifecycle status.
func (s *ProvisioningService) SetMembershipStatus(ctx context.Context, tenantID, userID uuid.UUID, status domain.MembershipStatus, expectedVersion int64) (*domain.TenantMembership, error) {
	// GAP-AUTH-2: validate status against the allowed set before hitting DB.
	// Without this, an invalid value like "unknown" returns a raw pgconn 500
	// instead of a clean 400 validation_error.
	switch status {
	case domain.MembershipActive, domain.MembershipSuspended, domain.MembershipLeft:
	default:
		return nil, domain.NewError(domain.ErrValidation, "status must be one of active, suspended, left")
	}
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.UserID = "iam-system"
	g.TenantID = tenantID.String()
	gucCtx := pgcommon.WithGUCSet(ctx, g)
	mem, err := s.memberships.SetStatus(gucCtx, tenantID, userID, status, expectedVersion)
	if err != nil {
		return nil, err
	}
	// CACHE-7: evict per-user and list caches so I-8 hot path does not serve
	// stale membership status after a KC lifecycle event (e.g. suspended user
	// still appearing active to AuthZ Enrichment).
	if s.cache != nil {
		_ = s.cache.Delete(gucCtx,
			cacheKeyMemberships(tenantID, userID),
			cacheKeyMembers(tenantID, 50),
		)
	}
	return mem, nil
}

// DeleteMember is I-5: full cascade on Keycloak USER_DELETE. Soft-deletes
// tenant_memberships + cascades tenant_roles, dept_memberships. Sets
// ownerless_since if we just removed the last active tenant_owner
// (TM-12/T-13). Emits MembershipRevoked atomically (ADR-0008 §6.4 — the
// delegation and tender-ACL cascades this used to run inline moved to the
// Delegation Service's and Tender-ACL Service's own async consumers, both
// subscribed to this one shared event, LLD §15.2.2).
func (s *ProvisioningService) DeleteMember(ctx context.Context, tenantID, userID uuid.UUID) error {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.UserID = "iam-system"
	g.TenantID = tenantID.String()
	gucCtx := pgcommon.WithGUCSet(ctx, g)

	return s.txRunner.RunInTx(gucCtx, func(txCtx context.Context) error {
		// Look up membership (need expected version for soft delete).
		mem, err := s.memberships.FindByUserID(txCtx, tenantID, userID)
		if err != nil {
			// TM-12 idempotency: member already deleted (deleted_at IS NOT NULL
			// makes FindByUserID return ErrMemberNotFound). Return success so
			// KC webhook retries are safe no-ops.
			if errors.Is(err, domain.ErrMemberNotFound) {
				return nil
			}
			return err
		}
		// Check whether this user is the last active tenant_owner BEFORE
		// cascading revokes.
		wasOwner := false
		trs, err := s.roles.ListByUser(txCtx, tenantID, userID)
		if err != nil {
			return err
		}
		for _, tr := range trs {
			if tr.RoleCode == domain.RoleTenantOwner {
				wasOwner = true
				break
			}
		}

		pub, _ := port.EventPublisherFromContext(txCtx)

		// Cascade 1: revoke all elevated roles.
		revokedRoles, err := s.roles.SoftDeleteAllForUser(txCtx, tenantID, userID)
		if err != nil {
			return err
		}
		for _, r := range revokedRoles {
			if pub != nil {
				_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
					Type: domain.EventTenantRoleRevoked, TenantID: tenantID,
					Subject: userID.String(), Actor: "iam-system",
					Data: domain.TenantRoleRevokedPayload{
						UserID: userID, TenantID: tenantID, RoleCode: r.RoleCode, ActorID: uuid.Nil,
					},
				})
			}
		}

		// Cascade 2: soft-delete dept memberships.
		revokedDepts, err := s.deptMems.SoftDeleteAllForUser(txCtx, tenantID, userID)
		if err != nil {
			return err
		}
		for _, d := range revokedDepts {
			if pub != nil {
				_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
					Type: domain.EventDepartmentMembershipRevoked, TenantID: tenantID,
					Subject: userID.String(), Actor: "iam-system",
					Data: domain.DepartmentMembershipRevokedPayload{
						UserID: userID, TenantID: tenantID, DepartmentID: d.DepartmentID,
					},
				})
			}
		}

		// Cascade 3 (ADR-0008 §6.4): the delegation and tender-ACL cascades
		// that used to run inline here (s.delegations.SoftDeleteForUser +
		// per-row DelegationEnded{delegate_removed}; s.acls.
		// SoftDeleteForUser) moved to the Delegation Service's and
		// Tender-ACL Service's own async consumers, both of which subscribe
		// to this single shared MembershipRevoked emission (LLD §15.2.2,
		// mirroring MembershipService.RemoveUser's identical emission) and
		// run their own cascades.
		if pub != nil {
			_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
				Type: domain.EventMembershipRevoked, TenantID: tenantID,
				Subject: userID.String(), Actor: "iam-system",
				Data: domain.MembershipRevokedPayload{
					TenantID: tenantID, UserID: userID, ActorID: domain.SystemActorID,
				},
			})
		}

		// Membership itself.
		if err := s.memberships.SoftDelete(txCtx, tenantID, userID, mem.RecordVersion); err != nil {
			return err
		}

		// TM-12: if we just removed the last active owner, set
		// ownerless_since. Uses TxFromContext to hit the running tx.
		// LLD §11.4 / TM-12 line 3807: fire an event-time counter + ERROR
		// log so on-call is paged the moment escalation triggers, without
		// waiting for the periodic ownerless-scanner gauge.
		if wasOwner {
			ownerRemaining, err := s.roles.CountActiveOwners(txCtx, tenantID)
			if err != nil {
				return err
			}
			if ownerRemaining == 0 {
				if tx, ok := pgadapterTxFromContext(txCtx); ok {
					cmd, err := tx.Exec(txCtx, `UPDATE tenants SET ownerless_since = now() WHERE id = $1 AND ownerless_since IS NULL`, tenantID)
					if err != nil {
						return err
					}
					// Only page if this call actually flipped the flag (idempotent
					// re-removals during retries must not double-alert).
					if cmd.RowsAffected() > 0 {
						if metrics.TenantOwnerlessEscalated != nil {
							metrics.TenantOwnerlessEscalated.WithLabelValues("user_removed").Inc()
						}
						s.log.ErrorContext(txCtx, "tenant_ownerless_escalation",
							"tenant_id", tenantID.String(),
							"removed_user_id", userID.String(),
							"reason", "last_active_owner_removed",
						)
					}
				}
			}
		}
		return nil
	})
}

// ── validation helpers (GAP-I1-3/I1-4/I1-5) ──────────────────────────────

var slugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{1,61}[a-z0-9])?$`)

// isValidSlug enforces DNS-label rules: lowercase alphanumeric + hyphens,
// 3–63 chars, no leading/trailing hyphens.
func isValidSlug(s string) bool {
	return len(s) >= 3 && len(s) <= 63 && slugRe.MatchString(s)
}

var localeRe = regexp.MustCompile(`^[a-zA-Z]{1,8}(-[a-zA-Z0-9]{1,8})*$`)

// isValidLocale does a lightweight BCP-47 structural check.
func isValidLocale(s string) bool {
	return localeRe.MatchString(s)
}
