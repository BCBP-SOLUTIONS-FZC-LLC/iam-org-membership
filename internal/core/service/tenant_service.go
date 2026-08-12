package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
)

// TenantService owns the P-1/P-2 flows. AUTH-1 (read as active member)
// and AUTH-1 write (tenant_owner only for P-2) are checked at the handler
// layer via requestctx; this service enforces T-10 range, optimistic
// locking round-trip, and the T-15 realm-config sync on
// local_accounts_enabled changes (Option A local-first + reconcile).
type TenantService struct {
	tenants port.TenantRepository
	cache   port.Cache
	rp      port.RealmProvisionerClient
}

func NewTenantService(tenants port.TenantRepository, cache port.Cache, rp port.RealmProvisionerClient) *TenantService {
	return &TenantService{tenants: tenants, cache: cache, rp: rp}
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
// error, sets realm_sync_pending=true and returns 202 semantics (handler
// translates to HTTP 202 in Phase 4). Phase 2 stub RP returns nil so this
// path exercises the happy branch.
func (s *TenantService) Patch(ctx context.Context, tenantID uuid.UUID, patch *domain.TenantPatch) (*domain.Tenant, bool, error) {
	if patch == nil {
		return nil, false, domain.NewError(domain.ErrValidation, "patch is required")
	}
	// Empty body — no fields to update; return current tenant as a no-op.
	// P-2 LLD has no detailed spec for this case; treat as idempotent read.
	if patch.Name == nil && patch.DefaultLocale == nil &&
		patch.LocalAccountsEnabled == nil && patch.MFAFreshnessSeconds == nil {
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
	// BCP-47 sanity: for Phase 2 we accept anything non-empty and defer
	// full BCP-47 validation to a shared validator lib in Phase 6.
	if patch.DefaultLocale != nil && *patch.DefaultLocale == "" {
		return nil, false, domain.NewError(domain.ErrValidation, "default_locale must not be empty").
			WithDetails(map[string]any{"code": "invalid_locale"})
	}

	// Detect a local_accounts_enabled change so we know whether to invoke
	// PatchRealmConfig after the local write commits.
	needRealmSync := false
	if patch.LocalAccountsEnabled != nil {
		before, err := s.tenants.FindByID(ctx, tenantID)
		if err != nil {
			return nil, false, err
		}
		if before.LocalAccountsEnabled != *patch.LocalAccountsEnabled {
			needRealmSync = true
		}
	}

	updated, err := s.tenants.Update(ctx, tenantID, patch)
	if err != nil {
		return nil, false, err
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

	return updated, deferredSync, nil
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
