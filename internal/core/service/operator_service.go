package service

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
)

// OperatorService owns O-4/O-7 — the two operator write paths not
// extracted to the Catalog / Admin Config Service. O-1/O-2/O-3
// (departments) and O-5/O-6 (plans) moved there per migration-runbook
// Phase 4 (LLD §12 step 4); this service, its repositories, and the
// local departments/plans tables have all been removed. Every remaining
// method assumes handler-layer AUTH-6 gate (platform_operator role). RLS
// still applies unless the caller uses the org_membership_migrator
// connection. O-4 (feature-flags on any tenant) and O-7 (reassign
// ownership) mutate a specific tenant — the handler sets that tenant's id
// in the GUCSet before calling in.
type OperatorService struct {
	tenants  port.TenantRepository
	tenRoles port.TenantRoleRepository
	memBs    port.MembershipRepository
	cache    port.Cache
	txRunner port.TxRunner
}

func NewOperatorService(
	tenants port.TenantRepository, roles port.TenantRoleRepository,
	memberships port.MembershipRepository, cache port.Cache,
	txRunner port.TxRunner,
) *OperatorService {
	return &OperatorService{
		tenants:  tenants,
		tenRoles: roles, memBs: memberships, cache: cache, txRunner: txRunner,
	}
}

// ── O-4 Feature flags on a tenant ──────────────────────────────────────

// featureFlagAllowlist is the closed set of tenant-level feature-flag keys
// operators may override via O-4. LLD §16 A18 requires an allow-list so a
// typo like "sso_enable" is rejected up front rather than stored as a dead
// override that looks configured but is never read. Kept in sync with the
// service-layer plan-defaults map (LLD O-4 spec, line 2029).
var featureFlagAllowlist = map[string]struct{}{
	"sso_enabled":           {},
	"custom_branding":       {},
	"require_mfa_all_users": {},
}

// recordFeatureFlagChange enqueues the config.tenant_setting.changed audit
// entry for O-4 on its transaction. It re-reads the row inside the tx for the
// record_version and updated_at the update produced: the key is
// <type>:<tenant>:<record_version>, shared with P-2 so the two paths can
// never collide, and OccurredAt is the row's updated_at. A replace that
// changes nothing does not bump record_version (TRG-3) and is not audited.
// Metadata carries only the flags that were added, removed or changed, with
// their old and new values (scalars from the allow-list; no personal data).
// No recorder in ctx (tests without one) skips it.
func (s *OperatorService) recordFeatureFlagChange(ctx context.Context, tenantID uuid.UUID, oldFlags, newFlags map[string]any, expectedVersion int64) error {
	rec, ok := port.AuditRecorderFromContext(ctx)
	if !ok {
		return nil
	}
	changes := featureFlagChanges(oldFlags, newFlags)
	if len(changes) == 0 {
		return nil
	}
	row, err := s.tenants.FindByID(ctx, tenantID)
	if err != nil {
		return err
	}
	if row.RecordVersion == expectedVersion {
		return nil
	}
	actorID := uuid.Nil
	var ipAddr, ua string
	if rc, ok := requestctx.FromContext(ctx); ok && rc != nil {
		actorID, ipAddr, ua = rc.UserID, rc.ClientIP, rc.UserAgent
	}
	occurredAt := row.UpdatedAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	return rec.Record(ctx, port.AuditEntry{
		IdempotencyKey:  fmt.Sprintf("%s:%s:%d", port.AuditEntryTenantSettingChanged, tenantID, row.RecordVersion),
		TenantID:        tenantID,
		EntryType:       port.AuditEntryTenantSettingChanged,
		Action:          "update",
		Actor:           port.AuditActorFor(actorID),
		OccurredAt:      occurredAt,
		SourceEventType: "TenantSettingChanged",
		IPAddress:       ipAddr,
		UserAgent:       ua,
		Metadata:        map[string]any{"feature_flags": changes},
	})
}

// featureFlagChanges returns, per flag that was added, removed or changed,
// {"change": created|removed|enabled|disabled|modified, "old": …, "new": …}.
// enabled/disabled apply to boolean flags turned on or off.
func featureFlagChanges(oldFlags, newFlags map[string]any) map[string]any {
	out := map[string]any{}
	for k, nv := range newFlags {
		ov, had := oldFlags[k]
		switch {
		case !had:
			out[k] = map[string]any{"change": "created", "new": nv}
		case !reflect.DeepEqual(normalizeFlag(ov), normalizeFlag(nv)):
			change := "modified"
			if nb, ok := nv.(bool); ok {
				if _, wasBool := ov.(bool); wasBool {
					change = map[bool]string{true: "enabled", false: "disabled"}[nb]
				}
			}
			out[k] = map[string]any{"change": change, "old": ov, "new": nv}
		}
	}
	for k, ov := range oldFlags {
		if _, ok := newFlags[k]; !ok {
			out[k] = map[string]any{"change": "removed", "old": ov}
		}
	}
	return out
}

// normalizeFlag maps integer kinds to float64 so a stored JSON number and a
// request int compare equal.
func normalizeFlag(v any) any {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return v
}

func (s *OperatorService) SetFeatureFlags(ctx context.Context, tenantID uuid.UUID, flags map[string]any, expectedVersion int64) (*domain.Tenant, error) {
	if flags == nil {
		flags = map[string]any{}
	}
	// LLD O-4: keys validated against a fixed allow-list (400 unknown_feature_flag)
	// before the scalar-value check so a typo lands on the more specific error.
	for k := range flags {
		if _, ok := featureFlagAllowlist[k]; !ok {
			return nil, domain.NewError(domain.ErrValidation, "unknown feature flag key").
				WithDetails(map[string]any{"code": "unknown_feature_flag", "key": k})
		}
	}
	// PLAN-6(d): scalars only (defense — no nested objects/arrays).
	for k, v := range flags {
		switch v.(type) {
		case string, bool, float64, int, int64, nil:
		default:
			return nil, domain.NewError(domain.ErrValidation, "feature flag values must be scalars").
				WithDetails(map[string]any{"code": "invalid_feature_value", "key": k})
		}
	}
	flagsJSON, _ := json.Marshal(flags)

	// Read current flags before the transaction so the old value is available
	// for the audit event without extending the transaction's lock hold time.
	// (The optimistic-lock version check in SetFeatureFlags prevents a lost
	// update: if another writer races between this read and the tx, the tx
	// fails with ErrOptimisticLockConflict and the caller retries.)
	current, err := s.tenants.FindByID(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	oldFlagsJSON, _ := json.Marshal(current.FeatureFlags)

	// Resolve the authenticated operator — O-4 is a human operator action and
	// the event must carry the real actor ID, not the system sentinel. Falls
	// back to SystemActorID only when no authenticated user is present (e.g.
	// internal tooling running as the system principal).
	actorID := domain.SystemActorID
	var ipAddr, ua string
	if rc, ok := requestctx.FromContext(ctx); ok && rc.UserID != uuid.Nil {
		actorID = rc.UserID
		ipAddr = rc.ClientIP
		ua = rc.UserAgent
	}

	// LLD O-4 optimistic-lock contract (CONC-1) lives in
	// TenantRepository.SetFeatureFlags so the service never touches SQL.
	// SetFeatureFlags + TenantSettingChanged enqueue are inside one RunInTx
	// so the event is atomic with the write (OP-6/EVT-10). The lock is held
	// for one statement only (the UPDATE) — the old-value read is pre-tx.
	var updated *domain.Tenant
	err = s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		if err := s.tenants.SetFeatureFlags(txCtx, tenantID, flagsJSON, expectedVersion); err != nil {
			return err
		}
		// O-4 is audited (AL-5 config.tenant_setting.changed): feature flags
		// are tenant configuration that changes application behaviour.
		if err := s.recordFeatureFlagChange(txCtx, tenantID, current.FeatureFlags, flags, expectedVersion); err != nil {
			return err
		}
		pub, ok := port.EventPublisherFromContext(txCtx)
		if !ok {
			return nil
		}
		return pub.Enqueue(txCtx, &domain.DomainEvent{
			Type:      domain.EventTenantSettingChanged,
			TenantID:  tenantID,
			Subject:   tenantID.String(),
			Actor:     actorID.String(),
			IPAddress: ipAddr,
			UserAgent: ua,
			Data: domain.TenantSettingChangedPayload{
				TenantID: tenantID,
				Setting:  "feature_flags",
				OldValue: string(oldFlagsJSON),
				NewValue: string(flagsJSON),
				ActorID:  actorID,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	updated, err = s.tenants.FindByID(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if s.cache != nil {
		_ = s.cache.Delete(ctx, cacheKeyTenant(tenantID))
		// Evict I-8 membership projections for all active members — feature_flags
		// feeds effective_feature_flags in every MembershipProjection (CACHE-3).
		if s.memBs != nil {
			if uids, err := s.memBs.ListActiveUserIDs(ctx, tenantID); err == nil && len(uids) > 0 {
				keys := make([]string, len(uids))
				for i, uid := range uids {
					keys[i] = "om:memberships:" + tenantID.String() + ":" + uid.String()
				}
				_ = s.cache.Delete(ctx, keys...)
			}
		}
	}
	return updated, nil
}

// ── O-7 Reassign owner ─────────────────────────────────────────────────

func (s *OperatorService) ReassignOwner(ctx context.Context, tenantID, newOwnerUserID uuid.UUID, actorID uuid.UUID) (*domain.TenantRole, error) {
	t, err := s.tenants.FindByID(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if t.Status == domain.StatusOffboarded {
		return nil, domain.NewError(domain.ErrTenantOffboarded, "tenant is offboarded")
	}
	// New owner must be an active member.
	mem, err := s.memBs.FindByUserID(ctx, tenantID, newOwnerUserID)
	if err != nil || mem.Status != domain.MembershipActive {
		return nil, domain.NewError(domain.ErrInvalidOwnerCandidate, "new owner is not an active member")
	}
	// Grant tenant_owner, clear ownerless_since, and emit TenantRoleGranted
	// in one tx via the TxRunner (which injects an EventPublisher into ctx
	// so outbox events land atomically per CONS-1). LLD §5.4 O-7 step 5
	// requires the event so AuthZ/Audit/Notification see the reassignment.
	var tr *domain.TenantRole
	err = s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		granted, gerr := s.tenRoles.Grant(txCtx, &domain.TenantRole{
			TenantID: tenantID, UserID: newOwnerUserID,
			TenantMembershipID: mem.ID, RoleCode: domain.RoleTenantOwner, GrantedBy: actorID,
		})
		if gerr != nil {
			return gerr
		}
		tr = granted
		if uerr := s.tenants.ClearOwnerlessSince(txCtx, tenantID); uerr != nil {
			return uerr
		}
		pub, _ := port.EventPublisherFromContext(txCtx)
		if pub != nil {
			if err := pub.Enqueue(txCtx, &domain.DomainEvent{
				Type: domain.EventTenantRoleGranted, TenantID: tenantID,
				Subject: newOwnerUserID.String(), Actor: actorID.String(),
				Data: domain.TenantRoleGrantedPayload{
					UserID: newOwnerUserID, TenantID: tenantID,
					RoleCode: domain.RoleTenantOwner, ActorID: actorID,
				},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Evict the affected user's membership cache so AuthZ Enrichment's next
	// I-8 lookup sees the new owner grant (LLD §5.4 O-7 step 6).
	if s.cache != nil {
		_ = s.cache.Delete(ctx, cacheKeyTenant(tenantID))
		_ = s.cache.Delete(ctx, cacheKeyMemberships(tenantID, newOwnerUserID))
	}
	return tr, nil
}

// ActiveRoleCodes returns a user's current active elevated tenant_roles
// (LLD §5.4 O-7 response: `roles: ["tenant_owner", …]` — the full set, not
// just the role this call granted, since the promoted user may already
// have held other elevated roles).
func (s *OperatorService) ActiveRoleCodes(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenantRoleCode, error) {
	roles, err := s.tenRoles.ListByUser(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	codes := make([]domain.TenantRoleCode, len(roles))
	for i, r := range roles {
		codes[i] = r.RoleCode
	}
	return codes, nil
}

// Suppress unused reference to time (present for future policy checks).
var _ = time.Now
