package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
)

// patchLocaleRe is the same structural BCP-47 check used by provisioning
// (provisioning_service.go isValidLocale). Kept here so P-2 and I-1 apply
// identical rules — a stored locale must always be a well-formed tag.
var patchLocaleRe = regexp.MustCompile(`^[a-zA-Z]{1,8}(-[a-zA-Z0-9]{1,8})*$`)

// TenantService owns the P-1/P-2 flows. AUTH-1 (read as active member)
// and AUTH-1 write (tenant_owner only for P-2) are checked at the handler
// layer via requestctx; this service enforces T-10 range, optimistic
// locking round-trip, and the T-15 realm-config sync on
// local_accounts_enabled changes (Option A local-first + reconcile).
type TenantService struct {
	tenants   port.TenantRepository
	cache     port.Cache
	rp        port.RealmProvisionerClient
	audit     port.AuditLogClient    // optional; nil disables AL-5 direct-write
	plans     port.PlanCatalogReader // optional; used by plan-change-check (I-18)
	idpReader port.RealmIDPReader    // optional; used by plan-change-check (I-18)
	txRunner  port.TxRunner          // optional; when set, residency_mode changes publish TenantSettingChanged
}

func NewTenantService(tenants port.TenantRepository, cache port.Cache, rp port.RealmProvisionerClient) *TenantService {
	return &TenantService{tenants: tenants, cache: cache, rp: rp}
}

// WithAuditLog attaches the AL-5 client used for config.tenant_setting.changed
// entries (D-6). Nil is safe — missing client silently skips the write.
func (s *TenantService) WithAuditLog(audit port.AuditLogClient) *TenantService {
	s.audit = audit
	return s
}

// WithPlanCatalog attaches the plan catalog reader used by plan-change-check
// (I-18, CD-20). Nil is safe — the SSO check is skipped and the result is
// allowed (fail-open on catalog unavailability).
func (s *TenantService) WithPlanCatalog(plans port.PlanCatalogReader) *TenantService {
	s.plans = plans
	return s
}

// WithIDPReader attaches the Realm Provisioner IDP-count client used by
// plan-change-check (I-18, CD-20).
//
// Fail-closed: when the target plan drops SSO, a nil reader returns
// 503 realm_provisioner_unavailable — the count is required. In production
// this is always wired via main.go. Tests must provide a mock.
func (s *TenantService) WithIDPReader(idpReader port.RealmIDPReader) *TenantService {
	s.idpReader = idpReader
	return s
}

// WithTxRunner attaches the transaction runner used to publish
// TenantSettingChanged onto iam.tenant.events atomically with the DB write
// when residency_mode changes (CD-37). Nil is safe — the event is simply
// not published if no runner is wired (graceful degradation in tests/dev
// environments where the outbox is not active).
func (s *TenantService) WithTxRunner(txRunner port.TxRunner) *TenantService {
	s.txRunner = txRunner
	return s
}

// BlockerCodeSSOIdpConfigured is the wire code emitted in I-18 blockers[]
// when the tenant has active SAML/OIDC IdPs and the target plan drops SSO.
const BlockerCodeSSOIdpConfigured = "sso_idp_configured"

// PlanChangeBlocker is a single reason a plan change cannot proceed.
type PlanChangeBlocker struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// PlanChangeCheckResult is the response payload for I-18.
type PlanChangeCheckResult struct {
	Allowed  bool                `json:"allowed"`
	Blockers []PlanChangeBlocker `json:"blockers"`
}

// PlanChangeCheck is I-18 (CD-20). It evaluates whether changing to
// targetPlan is safe and returns {allowed, blockers[]}.
//
// Validation order:
//  1. target_plan empty or unknown code → 400 invalid_target_plan
//  2. Unknown tenant → 404 tenant_not_found
//  3. Target plan has sso_enabled=true in catalog → allowed (no RP call)
//  4. RP IDP count unavailable → 503 realm_provisioner_unavailable (fail-closed)
//  5. idp_count > 0 → blocked sso_idp_configured
//  6. idp_count == 0 → allowed
//
// The catalog lookup is fail-open (assumes SSO=true on miss).
// The RP call is fail-closed: any error surfaces as 503.
func (s *TenantService) PlanChangeCheck(ctx context.Context, tenantID uuid.UUID, targetPlan domain.TenantPlan) (*PlanChangeCheckResult, error) {
	// 1. Validate target_plan enum — both missing and unknown → 400.
	if targetPlan == "" {
		metrics.IncPlanChangeCheck("error")
		return nil, domain.NewError(domain.ErrInvalidTargetPlan, "target_plan is required")
	}
	switch targetPlan {
	case domain.PlanStarter, domain.PlanPro, domain.PlanEnterprise:
	default:
		metrics.IncPlanChangeCheck("error")
		return nil, domain.NewError(domain.ErrInvalidTargetPlan,
			"invalid target_plan: must be starter, pro, or enterprise").
			WithDetails(map[string]any{"received": string(targetPlan)})
	}

	// 2. Confirm tenant exists (→ 404 on miss).
	if _, err := s.Get(ctx, tenantID); err != nil {
		return nil, err
	}

	// 3. Catalog lookup — determines SSO grant for the target plan.
	//    Fail-open ONLY when catalog positively confirms sso_enabled=true: an
	//    upgrade to a plan with SSO can never cause an IdP conflict. On catalog
	//    miss, error, or nil reader, we cannot confirm the plan has SSO, so we
	//    fall through to step 4 and let RP decide (CD-20 review fix).
	if s.plans != nil {
		if p, err := s.plans.PlanByCode(ctx, targetPlan); err == nil && p != nil && p.SSOEnabled {
			// Catalog confirmed: target plan has SSO. No IdP conflict possible.
			metrics.IncPlanChangeCheck("allowed")
			return &PlanChangeCheckResult{Allowed: true, Blockers: []PlanChangeBlocker{}}, nil
		}
	}

	// 4. Target plan drops SSO — must verify no active IdPs (fail-closed).
	if s.idpReader == nil {
		metrics.IncPlanChangeCheck("error")
		return nil, domain.NewError(domain.ErrRealmProvisionerUnavailable,
			"realm provisioner client not configured; cannot verify IdP count")
	}
	idpCount, err := s.idpReader.GetIDPCount(ctx, tenantID)
	if err != nil {
		metrics.IncPlanChangeCheck("error")
		return nil, domain.NewError(domain.ErrRealmProvisionerUnavailable,
			"realm provisioner unavailable; cannot verify IdP count for plan change")
	}

	// 5–6. Decide on the blocker.
	if idpCount > 0 {
		metrics.IncPlanChangeCheck("blocked")
		return &PlanChangeCheckResult{
			Allowed: false,
			Blockers: []PlanChangeBlocker{{
				Code: BlockerCodeSSOIdpConfigured,
				Detail: fmt.Sprintf(
					"tenant has %d active IdP(s); target plan %s has no SSO",
					idpCount, targetPlan),
			}},
		}, nil
	}
	metrics.IncPlanChangeCheck("allowed")
	return &PlanChangeCheckResult{Allowed: true, Blockers: []PlanChangeBlocker{}}, nil
}

// Get returns the tenant row for the caller's tenant (P-1). Cache-through
// pattern: read om:tenant:{tenant} first, fall through to Postgres on miss,
// populate cache with 600 s TTL (CACHE-9 advisory).
func (s *TenantService) Get(ctx context.Context, tenantID uuid.UUID) (*domain.Tenant, error) {
	// AUTH-1 read: any active member of the target tenant. Handler already
	// verified rc.TenantID == tenantID (API-2). We could re-verify here
	// for defense-in-depth but RLS enforces at the DB layer regardless.

	if cached := s.getCached(ctx, tenantID); cached != nil {
		return cached, nil
	}
	t, err := s.tenants.FindByID(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	s.setCached(ctx, t)
	return t, nil
}

// GetIncludingOffboarded is the iam-system-only read path for offboarded
// (soft-deleted) tenants. Used by internal endpoints (I-2 RP cleanup,
// I-9/I-14 locale/mfa reads) where the caller is iam-system and the tenant
// may have deleted_at IS NOT NULL. Bypasses cache (offboarded rows are
// excluded from cache population) and calls FindByIDIncludingDeleted.
func (s *TenantService) GetIncludingOffboarded(ctx context.Context, tenantID uuid.UUID) (*domain.Tenant, error) {
	return s.tenants.FindByIDIncludingDeleted(ctx, tenantID)
}

// Patch applies P-2. Enforces T-10 range on MFA freshness. On
// local_accounts_enabled change, calls RP PatchRealmConfig — on non-nil
// error, sets realm_sync_pending=true (T-15 Option A) and the handler
// returns HTTP 202 to the client.
func (s *TenantService) Patch(ctx context.Context, tenantID uuid.UUID, patch *domain.TenantPatch) (*domain.Tenant, bool, error) {
	if patch == nil {
		return nil, false, domain.NewError(domain.ErrValidation, "patch is required")
	}
	// Empty body — no fields to update; return current tenant as a no-op.
	// P-2 LLD has no detailed spec for this case; treat as idempotent read.
	if patch.Name == nil && patch.DefaultLocale == nil &&
		patch.LocalAccountsEnabled == nil && patch.MFAFreshnessSeconds == nil &&
		patch.ResidencyMode == nil {
		t, err := s.tenants.FindByID(ctx, tenantID)
		return t, false, err
	}
	// T-10: mfa_freshness_seconds must be in [60, 900].
	if patch.MFAFreshnessSeconds != nil {
		v := *patch.MFAFreshnessSeconds
		if v < 60 || v > 900 {
			return nil, false, domain.NewError(domain.ErrValidation, "mfa_freshness_seconds must be between 60 and 900").
				WithDetails(map[string]any{"code": "invalid_mfa_freshness_seconds"})
		}
	}
	// Name length cap — same as provisioning (GAP-I1-4 / provisioning_service.go).
	// The DB has CHECK(name <> '') but no upper-bound constraint, so the service
	// layer is the only guard.
	if patch.Name != nil && len(*patch.Name) > 255 {
		return nil, false, domain.NewError(domain.ErrValidation, "name must not exceed 255 characters").
			WithDetails(map[string]any{"code": "invalid_request"})
	}

	// BCP-47 structural check — same regex as provisioning so stored locales
	// are always well-formed (e.g. "en-US", "fr", "zh-Hant").
	if patch.DefaultLocale != nil {
		if *patch.DefaultLocale == "" || !patchLocaleRe.MatchString(*patch.DefaultLocale) {
			return nil, false, domain.NewError(domain.ErrValidation, "default_locale must be a valid BCP-47 language tag (e.g. en-US, fr, zh-Hant)").
				WithDetails(map[string]any{"code": "invalid_locale"})
		}
	}

	// CD-37: residency_mode must be 'standard' or 'strict'.
	if patch.ResidencyMode != nil {
		switch *patch.ResidencyMode {
		case domain.ResidencyModeStandard, domain.ResidencyModeStrict:
		default:
			return nil, false, domain.NewError(domain.ErrValidation, "residency_mode must be 'standard' or 'strict'").
				WithDetails(map[string]any{"code": "invalid_residency_mode", "received": string(*patch.ResidencyMode)})
		}
	}

	// Pre-read: capture state needed for T-15 realm sync detection and
	// residency_mode old-value audit tracking (CD-37).
	needRealmSync := false
	var preReadTenant *domain.Tenant
	if patch.LocalAccountsEnabled != nil || patch.ResidencyMode != nil {
		var preErr error
		preReadTenant, preErr = s.tenants.FindByID(ctx, tenantID)
		if preErr != nil {
			return nil, false, preErr
		}
		if patch.LocalAccountsEnabled != nil &&
			preReadTenant.LocalAccountsEnabled != *patch.LocalAccountsEnabled {
			needRealmSync = true
		}
	}

	// When residency_mode is changing and a TxRunner is wired, wrap the Update
	// and the TenantSettingChanged event enqueue in a single transaction so the
	// DB write and the outbox insert commit atomically (EVT-10, CD-37).
	var updated *domain.Tenant
	if patch.ResidencyMode != nil && s.txRunner != nil {
		oldMode := domain.ResidencyModeStandard
		if preReadTenant != nil {
			oldMode = preReadTenant.ResidencyMode
		}
		var txUpdated *domain.Tenant
		if txErr := s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
			var updateErr error
			txUpdated, updateErr = s.tenants.Update(txCtx, tenantID, patch)
			if updateErr != nil {
				return updateErr
			}
			return s.enqueueSettingChanged(txCtx, tenantID, "residency_mode",
				string(oldMode), string(*patch.ResidencyMode))
		}); txErr != nil {
			return nil, false, txErr
		}
		updated = txUpdated
	} else {
		var updateErr error
		updated, updateErr = s.tenants.Update(ctx, tenantID, patch)
		if updateErr != nil {
			return nil, false, updateErr
		}
	}

	// Post-commit cache invalidation (CACHE-6).
	s.invalidateCache(ctx, tenantID)

	// T-15 Option A local-first + reconcile.
	deferredSync := false
	if needRealmSync {
		if rpErr := s.rp.PatchRealmConfig(ctx, tenantID, port.RealmConfigPatch{
			LocalAccountsEnabled: patch.LocalAccountsEnabled,
		}); rpErr != nil {
			// BUG-P2-1 fix: persist realm_sync_pending=true so the
			// realm-config-sync reconciler has a durable signal (T-15/§16 A58).
			// Fail-open: if this write also fails, log and continue — the 202
			// response still tells the caller to retry, and RP will be polled.
			if syncErr := s.tenants.SetRealmSyncPending(ctx, tenantID); syncErr != nil {
				_ = syncErr // best-effort — caller still gets 202 + retries
			}
			deferredSync = true
		}
	}

	// AL-5 best-effort audit (config.tenant_setting.changed, D-6).
	// Runs in a detached goroutine so an Audit Log outage never blocks P-2.
	if s.audit != nil {
		s.auditSettingChange(ctx, tenantID, patch, preReadTenant)
	}

	return updated, deferredSync, nil
}

// auditSettingChange posts a config.tenant_setting.changed entry to AL-5
// in a detached goroutine (fail-open, 5s hard cap).
// before is the pre-update tenant snapshot (may be nil if not pre-read).
func (s *TenantService) auditSettingChange(parent context.Context, tenantID uuid.UUID, patch *domain.TenantPatch, before *domain.Tenant) {
	// Capture actor from request context before spawning — parent ctx may
	// be cancelled by the time the goroutine runs.
	rc, _ := requestctx.FromContext(parent)
	actor := port.AuditActor{Type: "iam_system"}
	if rc != nil && rc.UserID != uuid.Nil {
		actor = port.AuditActor{Type: "user", ID: rc.UserID.String()}
	}
	ipAddress, userAgent := "", ""
	if rc != nil {
		ipAddress = rc.ClientIP
		userAgent = rc.UserAgent
	}

	changed := map[string]any{}
	if patch.MFAFreshnessSeconds != nil {
		changed["mfa_freshness_seconds"] = *patch.MFAFreshnessSeconds
	}
	if patch.DefaultLocale != nil {
		changed["default_locale"] = *patch.DefaultLocale
	}
	if patch.LocalAccountsEnabled != nil {
		changed["local_accounts_enabled"] = *patch.LocalAccountsEnabled
	}
	if patch.Name != nil {
		changed["name"] = *patch.Name
	}
	// CD-37: include old+new residency_mode so audit entries are self-contained.
	if patch.ResidencyMode != nil {
		entry := map[string]any{"new": string(*patch.ResidencyMode)}
		if before != nil {
			entry["old"] = string(before.ResidencyMode)
		}
		changed["residency_mode"] = entry
	}

	// Stable idempotency key: scoped to tenant + actor + changed field set
	// (millisecond timestamp keeps it unique per real patch but not per retry
	// within the same millisecond — acceptable for a best-effort audit call).
	idemKey := fmt.Sprintf("tenant-setting:%s:%s:%d", tenantID, actor.ID, time.Now().UnixMilli())

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.audit.WriteEntry(ctx, port.AuditWriteRequest{
			IdempotencyKey:  idemKey,
			TenantID:        tenantID,
			EntryType:       "config.tenant_setting.changed",
			Action:          "update",
			Actor:           actor,
			OccurredAt:      time.Now().UTC(),
			SourceService:   "iam-org-membership",
			SourceEventType: "TenantSettingChanged",
			IPAddress:       ipAddress,
			UserAgent:       userAgent,
			Metadata:        changed,
		}); err != nil {
			// Fail-open: audit outage must not degrade P-2; count for alerting.
			metrics.IncAuditDeliveryFailed("config.tenant_setting.changed")
		}
	}()
}

// enqueueSettingChanged enqueues a TenantSettingChanged event into the
// outbox via port.EventPublisherFromContext. Must be called inside a RunInTx
// so the outbox row and the business write share one transaction (EVT-10).
// Skips silently if no EventPublisher is present in ctx (tests that don't
// wire a publisher still pass without panicking).
func (s *TenantService) enqueueSettingChanged(ctx context.Context, tenantID uuid.UUID, setting, oldVal, newVal string) error {
	pub, ok := port.EventPublisherFromContext(ctx)
	if !ok {
		return nil
	}
	rc, _ := requestctx.FromContext(ctx)
	actorID := domain.SystemActorID
	if rc != nil && rc.UserID != uuid.Nil {
		actorID = rc.UserID
	}
	var ipAddr, ua string
	if rc != nil {
		ipAddr = rc.ClientIP
		ua = rc.UserAgent
	}
	return pub.Enqueue(ctx, &domain.DomainEvent{
		Type:       domain.EventTenantSettingChanged,
		TenantID:   tenantID,
		Subject:    tenantID.String(),
		Actor:      actorID.String(),
		OccurredAt: time.Now().UTC(),
		IPAddress:  ipAddr,
		UserAgent:  ua,
		Data: domain.TenantSettingChangedPayload{
			TenantID: tenantID,
			Setting:  setting,
			OldValue: oldVal,
			NewValue: newVal,
			ActorID:  actorID,
		},
	})
}

// ── cache helpers ────────────────────────────────────────────────────────

const tenantCacheTTL = 600 * time.Second

func (s *TenantService) getCached(ctx context.Context, tenantID uuid.UUID) *domain.Tenant {
	if s.cache == nil {
		return nil
	}
	raw, err := s.cache.Get(ctx, cacheKeyTenant(tenantID))
	if err != nil || raw == nil {
		return nil
	}
	var t domain.Tenant
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil
	}
	return &t
}

func (s *TenantService) setCached(ctx context.Context, t *domain.Tenant) {
	if s.cache == nil || t == nil {
		return
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return
	}
	_ = s.cache.Set(ctx, cacheKeyTenant(t.ID), raw, tenantCacheTTL)
}

func (s *TenantService) invalidateCache(ctx context.Context, tenantID uuid.UUID) {
	if s.cache == nil {
		return
	}
	_ = s.cache.Delete(ctx,
		cacheKeyTenant(tenantID),
		cacheKeyLocale(tenantID),
	)
}

// requireActiveMember is a defense-in-depth helper — handlers already
// check AUTH-1 via requestctx, but services can gate too. Referenced by
// future methods; harmless unused right now.
var _ = requireActiveMember

func requireActiveMember(rc *requestctx.RequestContext, tenantID uuid.UUID) error {
	if rc == nil {
		return domain.NewError(domain.ErrMissingIdentity, "missing identity")
	}
	if rc.TenantID != tenantID {
		return domain.NewError(domain.ErrInsufficientRole, "cross-tenant access")
	}
	return nil
}
