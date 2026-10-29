// Package consumer implements SQS event handlers for the two inbound
// queues Org & Membership subscribes to (§7.1):
//
//	tenant-orgm-q  ← iam.tenant.events (RP-produced tenant lifecycle)
//	billing-orgm-q ← billing.events    (Billing-produced status/plan/seats)
//
// The consumer enforces three cross-cutting guards on every applied event
// (§16 A33, A40, A61):
//
//   - EVT-14 recency guard — under the tenants row lock, compare
//     event.time against tenants.last_event_at. If event.time <=
//     last_event_at, skip the state change but still record
//     processed_events (stale/reordered).
//   - EVT-15 future-time clamp — if event.time > now() + skew, reject to
//     DLQ; do NOT record processed_events (poison-pill guard).
//   - EVT-16 tenant-state relay — whenever a consumed event actually
//     changes tenants.status or tenants.plan (post-EVT-14), enqueue a
//     TenantStateChanged event on iam.membership.events in the same tx
//     as the projection UPDATE.
//
// It additionally relays TenantMembershipsPurged on iam.membership.events
// whenever a consumed event genuinely transitions the tenant into
// 'offboarded' — same real-transition guard as EVT-16, but on its own
// event type so the Delegation, Tender-ACL, and Group-Mapping services'
// consumers can subscribe without matching every other status/plan change
// (LLD §15.5, ADR-0008 §6.4 pattern). Distinct from — and never a re-emit
// of — the Realm-Provisioner-produced TenantOffboarded event this consumer
// reacts to on tenant-orgm-q (LLD §16 OQ-1: one producer per event name).
//
// Idempotency (IDEMP-2/4) is provided by port.IdempotencyStore against
// processed_events, keyed by (event_id, consumer) — same port shape as
// iam-user-profile's IdempotencyStore. MarkProcessed joins the caller's
// TxRunner transaction so the dedup write commits atomically with the
// EVT-14 row lock and projection update below. Beyond-window duplicates
// (SQS max 14d + DLQ dwell) are backstopped by EVT-14 recency.
//
// Unknown event types are silently acknowledged, logged at INFO, and
// counted by iam_org_membership_unknown_event_acknowledged_total (§6 event
// consumer scope — forward-compat, avoids DLQ storm on producer schema
// additions).
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/google/uuid"
)

// consumerName is the (event_id, consumer) key component in processed_events.
// Both queues share one consumer identity so PE-1 dedup covers both.
const consumerName = "iam-org-membership"

// ErrPoisonPill marks an EVT-15 future-time clamp reject: processed_events
// is NOT recorded, and the message must go to the DLQ rather than be
// retried. platform-events treats every handler error as retryable, so the
// immediate DLQ move is done by the composition root's handler wrapper
// (cmd/server/dlq.go routeRejectsToDLQ), not by the SQS runner itself.
var ErrPoisonPill = errors.New("event rejected as poison pill (EVT-15 future-time clamp)")

// ErrProvisioningRejected marks a TenantIdentityProvisioned event whose
// payload I-1 can never accept (validation failure, or the slug is held by a
// different tenant): retrying cannot succeed, so it goes straight to the DLQ
// (reason provisioning_rejected) and processed_events is not recorded.
var ErrProvisioningRejected = errors.New("TenantIdentityProvisioned rejected: I-1 cannot accept the payload")

// errTenantNotYetProvisioned is returned, so the message is redelivered,
// when a lifecycle/billing event names a tenant with no tenants row at all
// and no parked-event store is wired (EVT-17; production parks instead,
// EVT-18). Replay also uses it to roll back and leave an event parked.
// Delivery on iam.tenant.events is unordered, so RP's events for a new tenant
// can arrive before the tenant is created (by the BFF's I-1 call or by the
// TenantIdentityProvisioned backstop). Before the backstop these were
// recorded and dropped, which lost e.g. a direct-paid TenantRealmReady.
var errTenantNotYetProvisioned = errors.New("tenant not provisioned yet — retrying")

// eventTenantIdentityProvisioned is RP's signup event (RP-D16) that drives the
// tenant provisioning backstop.
const eventTenantIdentityProvisioned = "TenantIdentityProvisioned"

// TenantProvisioner runs I-1 (ProvisioningService.TrialSignup) for the
// TenantIdentityProvisioned backstop.
type TenantProvisioner interface {
	TrialSignup(ctx context.Context, req service.TrialSignupInput) (*domain.Tenant, bool, error)
}

type MembershipEventConsumer struct {
	inbox       port.Inbox
	tenants     port.TenantRepository
	catalog     port.PlanCatalogReader
	cache       port.Cache
	provisioner TenantProvisioner
	parked      port.ParkedTenantEventStore
	txRunner    port.TxRunner
	skew        time.Duration
	logger      port.SlogStyleLogger
}

// NewMembershipEventConsumer builds a MembershipEventConsumer. logger may be
// nil — see MembershipService's constructor doc comment for the fallback/
// production-wiring contract, which applies identically here. inbox may be
// nil only in tests that never reach the dedup claim (e.g. constructor-
// validation tests); production wiring always supplies a real port.Inbox
// (postgres.InboxRepository, platform-events' inbox.Store). catalog may be nil
// only in tests that never exercise a TrialReactivated event — it resolves
// the reactivated plan's trial_duration_days from the Catalog Service
// (Plans moved out of this service's own database under ADR-0007). cache may
// be nil — the TenantOffboarded GDPR wipe's cache-invalidation step (§15.5)
// is best-effort and skipped entirely when cache is nil, consistent with
// CACHE-2/9 (advisory-only, never a correctness dependency).
func NewMembershipEventConsumer(inbox port.Inbox, tenants port.TenantRepository, catalog port.PlanCatalogReader, cache port.Cache, skew time.Duration, logger port.Logger) *MembershipEventConsumer {
	if skew <= 0 {
		skew = 300 * time.Second
	}
	return &MembershipEventConsumer{inbox: inbox, tenants: tenants, catalog: catalog, cache: cache, skew: skew, logger: port.NewSlogStyleLogger(logger)}
}

// WithTenantProvisioner wires the I-1 flow the TenantIdentityProvisioned
// backstop runs. Production wiring always sets it; without it such an event
// is retried and lands in the DLQ rather than being silently dropped.
func (c *MembershipEventConsumer) WithTenantProvisioner(p TenantProvisioner) *MembershipEventConsumer {
	c.provisioner = p
	return c
}

// WithParkedEvents switches the no-tenant-row path from retry-until-DLQ
// (EVT-17) to park-and-replay (EVT-18, CD-50): an event for a tenant with no
// row is parked in pending_tenant_events and acknowledged, and ReplayParked
// applies it once the tenant exists. txRunner runs each replay. Production
// wiring always sets both; without them the EVT-17 retry stays in force.
func (c *MembershipEventConsumer) WithParkedEvents(store port.ParkedTenantEventStore, txRunner port.TxRunner) *MembershipEventConsumer {
	c.parked = store
	c.txRunner = txRunner
	return c
}

// Handle is the entry point for platform-events SQS consumer.
func (c *MembershipEventConsumer) Handle(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	// ── EVT-15 future-time clamp ─────────────────────────────────────────
	if !env.Timestamp.IsZero() && env.Timestamp.After(time.Now().UTC().Add(c.skew)) {
		metrics.IncConsumedEventRejected(env.Type, "future_time_clamp")
		c.logger.Warn("EVT-15 future-time clamp — DLQ",
			"event_id", env.ID, "event_type", env.Type, "event_time", env.Timestamp)
		return ErrPoisonPill
	}

	kind := classify(env.Type)
	if kind == kindUnknown {
		return ackUnknown(ctx, c.inbox, c.logger, consumerName, env)
	}

	tenantID, err := uuid.Parse(env.TenantID)
	if err != nil {
		return fmt.Errorf("parse tenant_id from envelope: %w", err)
	}

	gucCtx := tenantGUC(ctx, tenantID)

	if env.Type == eventTenantIdentityProvisioned {
		return c.handleTenantIdentityProvisioned(ctx, gucCtx, tenantID, env)
	}

	trialDurationDays, err := c.trialDurationDays(ctx, gucCtx, tenantID, env)
	if err != nil {
		return err
	}

	var gdprWipeRan bool
	// One transaction: the processed_events claim (first, so a concurrent
	// duplicate waits on it and is then skipped), the EVT-14 row lock, the
	// projection and the relay events (port.Inbox).
	txErr := processOnce(gucCtx, c.inbox, consumerName, env, func(txCtx context.Context) error {
		locked, err := c.tenants.LockForProjection(txCtx, tenantID)
		if err != nil {
			return err
		}
		if locked == nil {
			locked, err = c.tenantAbsent(txCtx, tenantID, env)
			if err != nil || locked == nil {
				return err
			}
			// I-1 created the tenant while this event waited for the
			// provisioning lock. TrialReactivated's trial duration was peeked
			// while the row was missing, so redeliver it to recompute.
			if env.Type == "TrialReactivated" {
				return errTenantNotYetProvisioned
			}
		}
		gdprWipeRan, err = c.project(txCtx, tenantID, env, locked, trialDurationDays)
		return err
	})
	if txErr != nil {
		return txErr
	}

	if gdprWipeRan {
		c.evictAfterWipe(ctx, tenantID)
	}
	return nil
}

// tenantGUC binds the RLS GUC to the target tenant so RLS lets the consumer
// see and update the tenants row. Consumers run as the app pool (no
// BYPASSRLS) per RLS-4.
func tenantGUC(ctx context.Context, tenantID uuid.UUID) context.Context {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.UserID = "iam-system"
	g.TenantID = tenantID.String()
	return pgcommon.WithGUCSet(ctx, g)
}

// trialDurationDays resolves, for a TrialReactivated event, the trial length
// of the tenant's current plan; every other event type needs none.
func (c *MembershipEventConsumer) trialDurationDays(ctx, gucCtx context.Context, tenantID uuid.UUID, env events.Envelope[json.RawMessage]) (int, error) {
	if env.Type != "TrialReactivated" {
		return 0, nil
	}
	// TrialReactivated (§8.10.3/TR2) needs the tenant's current plan's
	// trial_duration_days from the Catalog Service to compute the new
	// trial_ends_at. Catalog is an HTTP call, so — like ProvisioningService.
	// TrialSignup — it must complete before the write transaction opens; an
	// HTTP call has no business running while a Postgres tx is held open.
	// This short read-only peek at the current plan runs in its own
	// transaction, committed and closed before the HTTP call.
	tenant, err := c.tenants.FindByID(gucCtx, tenantID)
	switch {
	case err != nil && errors.Is(err, domain.ErrTenantNotFound), err == nil && tenant == nil:
		// Tenant absent — the write tx hits the missing-row path (park, or
		// drop for a soft-deleted tenant) and needs no duration.
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("peek current plan for TrialReactivated: %w", err)
	case c.catalog == nil:
		return 0, errors.New("TrialReactivated: no PlanCatalogReader configured")
	}
	plan, err := c.catalog.PlanByCode(ctx, tenant.Plan)
	if err != nil {
		return 0, fmt.Errorf("resolve plan %q for TrialReactivated: %w", tenant.Plan, err)
	}
	return plan.TrialDurationDays, nil
}

// evictAfterWipe runs the GDPR wipe's cache invalidation (§15.5 step 3,
// CACHE-8) after the transaction commits — Valkey isn't part of the
// Postgres transaction, and cache is advisory only (CACHE-2/9): a failure
// here never rolls back or fails the wipe that already committed.
// Best-effort, skipped entirely if cache is nil.
func (c *MembershipEventConsumer) evictAfterWipe(ctx context.Context, tenantID uuid.UUID) {
	if c.cache == nil {
		return
	}
	if err := c.cache.Delete(ctx, gdprWipeCacheKeys(tenantID)...); err != nil {
		c.logger.Warn("GDPR wipe: cache invalidation failed (advisory-only, not fatal)",
			"tenant_id", tenantID, "error", err)
	}
}

// project runs the EVT-14 recency guard, the projection and the EVT-16 /
// TenantMembershipsPurged relays for one event against an already-locked
// tenant row. The live path and parked-event replay (EVT-18) share it. It
// reports whether the GDPR wipe ran, so the caller evicts the cache after
// the transaction commits.
func (c *MembershipEventConsumer) project(txCtx context.Context, tenantID uuid.UUID, env events.Envelope[json.RawMessage], locked *port.TenantProjectionLock, trialDurationDays int) (gdprWipeRan bool, err error) {
	// ── EVT-14 recency guard ─────────────────────────────────────────
	if locked.LastEventAt != nil && !env.Timestamp.After(*locked.LastEventAt) {
		if metrics.LifecycleEventSkipped != nil {
			metrics.LifecycleEventSkipped.WithLabelValues(env.Type).Inc()
		}
		// SLO-3: record lag even for skipped events — a late-arriving
		// event that is skipped is exactly the delivery-delay signal
		// iam_lifecycle_event_lag_seconds is designed to surface.
		if !env.Timestamp.IsZero() {
			metrics.ObserveLifecycleEventLag(env.Type, time.Since(env.Timestamp).Seconds())
		}
		c.logger.Info("EVT-14 stale — projection unchanged", "event_id", env.ID, "event_type", env.Type)
		return false, nil
	}

	prevStatus := locked.Status
	prevPlan := locked.Plan

	newStatus, newPlan, err := c.applyProjection(txCtx, tenantID, env, prevStatus, prevPlan, trialDurationDays)
	if err != nil {
		return false, err
	}

	// Bump last_event_at only when projection actually ran.
	if err := c.tenants.SetLastEventAt(txCtx, tenantID, env.Timestamp); err != nil {
		return false, err
	}

	// SLO-3: record the lag between event.time and apply time so
	// iam_lifecycle_event_lag_seconds tracks consumer processing drift.
	if !env.Timestamp.IsZero() {
		metrics.ObserveLifecycleEventLag(env.Type, time.Since(env.Timestamp).Seconds())
	}

	pub, _ := port.EventPublisherFromContext(txCtx)

	// ── EVT-16 tenant-state relay ────────────────────────────────────
	if pub != nil && (newStatus != prevStatus || newPlan != prevPlan) {
		if err := pub.Enqueue(txCtx, &domain.DomainEvent{
			Type:      domain.EventTenantStateChanged,
			TenantID:  tenantID,
			Subject:   tenantID.String(),
			Actor:     "iam-system",
			IPAddress: "system",
			UserAgent: "iam-org-membership/event-consumer",
			Data: domain.TenantStateChangedPayload{
				TenantID:       tenantID,
				Status:         newStatus,
				PreviousStatus: prevStatus,
				Plan:           newPlan,
				PreviousPlan:   prevPlan,
				ChangedAt:      env.Timestamp,
				Cause:          env.Type,
			},
		}); err != nil {
			return gdprWipeRan, err
		}
	}

	// ── ADR-0008 §6.4 (LLD §15.5): Core → Delegation/Tender-ACL/
	// Group-Mapping tenant-purge cascade signal. Whenever this
	// consumed event actually transitions the tenant into 'offboarded'
	// (guarded, like EVT-16 above, by the EVT-14 recency check already
	// having run and by the prevStatus != newStatus diff — never a
	// bare relay of the inbound event), relay TenantMembershipsPurged
	// on iam.membership.events so those services' consumers can
	// soft-delete their own tenant-scoped rows. Distinct from the
	// generic EVT-16 TenantStateChanged relay above (which also fires
	// on this transition) so a downstream consumer can filter on it
	// without matching every other status/plan change, and distinct
	// from the Realm-Provisioner-produced TenantOffboarded event this
	// same consumer reacts to on tenant-orgm-q (§16 OQ-1 — Core never
	// re-emits that event under its own name).
	if prevStatus != domain.StatusOffboarded && newStatus == domain.StatusOffboarded {
		// GDPR tenant wipe (§15.5) — delete Core's own remaining
		// tenant-scoped child rows in the same tx as the tenants
		// projection UPDATE above. The tenants row itself is only
		// soft-deleted (deleted_at set, PAID-1/chk_offboarded_soft_deleted
		// — id retained for audit), so ON DELETE CASCADE never fires
		// from it; these are explicit deletes, not a cascade side
		// effect. Order doesn't matter for FK ordering here — none of
		// these six tables reference each other, only tenants (which
		// is never itself deleted).
		if err := c.tenants.WipeTenantChildren(txCtx, tenantID); err != nil {
			return false, err
		}
		gdprWipeRan = true
	}

	if pub != nil && prevStatus != domain.StatusOffboarded && newStatus == domain.StatusOffboarded {
		if err := pub.Enqueue(txCtx, &domain.DomainEvent{
			Type:      domain.EventTenantMembershipsPurged,
			TenantID:  tenantID,
			Subject:   tenantID.String(),
			Actor:     "iam-system",
			IPAddress: "system",
			UserAgent: "iam-org-membership/event-consumer",
			Data: domain.TenantMembershipsPurgedPayload{
				TenantID: tenantID,
				ActorID:  domain.SystemActorID,
			},
		}); err != nil {
			return gdprWipeRan, err
		}
	}

	return gdprWipeRan, nil
}

// gdprWipeCacheKeys returns the exactly-known, bounded tenant-scoped cache
// keys to evict on offboard (§15.5 step 3, CACHE-8). Two per-secondary-key
// families are deliberately NOT enumerated here — om:memberships:{tenant}:
// {user} (unbounded set of users) and om:dept_members:{tenant}:{dept}
// (unbounded set of departments) — consistent with CACHE-2/9's advisory-
// cache philosophy: those entries simply expire on their existing TTL
// (seconds to minutes) rather than requiring a SCAN-based prefix delete.
func gdprWipeCacheKeys(tenantID uuid.UUID) []string {
	return []string{
		fmt.Sprintf("om:tenant:%s", tenantID),
		fmt.Sprintf("om:locale:%s", tenantID),
		fmt.Sprintf("om:roles:%s", tenantID),
		fmt.Sprintf("om:seat_usage:%s", tenantID),
		fmt.Sprintf("om:members:%s:50", tenantID), // CACHE-10: only limit=50 page-1 is ever cached
		fmt.Sprintf("om:grm:%s", tenantID),
		fmt.Sprintf("om:grm:stale:%s", tenantID),
		fmt.Sprintf("om:gdm:%s", tenantID),
		fmt.Sprintf("om:gdm:stale:%s", tenantID),
		fmt.Sprintf("om:gtrm:%s", tenantID),
		fmt.Sprintf("om:gtrm:stale:%s", tenantID),
	}
}

func (c *MembershipEventConsumer) applyProjection(ctx context.Context, tenantID uuid.UUID, env events.Envelope[json.RawMessage], prevStatus domain.SubscriptionStatus, prevPlan domain.TenantPlan, trialDurationDays int) (domain.SubscriptionStatus, domain.TenantPlan, error) {
	switch env.Type {
	// ── tenant-orgm-q (Realm-Provisioner-produced) ──────────────────────
	// TenantIdentityProvisioned never reaches here (handled before the
	// projection tx); TrialTenantProvisioned was retired by RP, which emits
	// TenantIdentityProvisioned instead — an in-flight one is acked as unknown.
	case "TenantRealmReady":
		// RP's frozen TenantRealmReadyPayload (§25) carries the realm name
		// under json:"realm", not "realm_id" — and has no realm_type field
		// at all (verified against iam-realm-provisioner's own
		// internal/core/domain/event.go — the realm_id/realm_type pair
		// exists only in RP's outbound/orgmembership client, its struct for
		// the *synchronous* PATCH /internal/tenants/:id REST call, a
		// different code path from this async event payload). Corrected: a
		// prior version of this handler read nonexistent "realm_id"/
		// "realm_type" fields, so every real event silently blanked
		// tenants.realm_id/realm_type to empty strings (execLifecyclePatch's
		// UPDATE has no COALESCE guard). RP's own doc comment on
		// TenantRealmReadyPayload confirms this event is "emitted by RP-2 or
		// RP-3 (never for trial)" — both dedicated-realm paths — so
		// realm_type is hardcoded rather than read from a field RP never
		// sends.
		var payload struct {
			Realm         string `json:"realm"`
			KeycloakShard string `json:"keycloak_shard"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return prevStatus, prevPlan, err
		}
		_, err := c.tenants.ApplyLifecyclePatch(ctx, tenantID, port.TenantLifecyclePatch{
			Op:            port.LifecycleSetRealm,
			RealmID:       payload.Realm,
			RealmType:     string(domain.RealmDedicated),
			KeycloakShard: payload.KeycloakShard,
		})
		return prevStatus, prevPlan, err
	case "TenantConverted":
		var payload struct {
			Plan string `json:"plan"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return prevStatus, prevPlan, err
		}
		newPlan := domain.TenantPlan(payload.Plan)
		if newPlan == "" {
			newPlan = prevPlan
		}
		_, err := c.tenants.ApplyLifecyclePatch(ctx, tenantID, port.TenantLifecyclePatch{
			Op:   port.LifecycleActivatePaid,
			Plan: newPlan,
		})
		return domain.StatusActive, newPlan, err
	case "DirectPaidSignup":
		// RP's DirectPaidSignupPayload includes a "plan" field
		// (iam-realm-provisioner internal/core/domain/event.go).
		// The fallback to prevPlan when plan is empty is intentional —
		// it handles the case where RP emits the event without a plan
		// value (e.g. older RP versions or a plan-unset payload).
		var payload struct {
			Plan string `json:"plan"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return prevStatus, prevPlan, err
		}
		newPlan := domain.TenantPlan(payload.Plan)
		if newPlan == "" {
			newPlan = prevPlan
		}
		_, err := c.tenants.ApplyLifecyclePatch(ctx, tenantID, port.TenantLifecyclePatch{
			Op:   port.LifecycleActivatePaid,
			Plan: newPlan,
		})
		return domain.StatusActive, newPlan, err
	case "TrialExpired":
		_, err := c.tenants.ApplyLifecyclePatch(ctx, tenantID, port.TenantLifecyclePatch{
			Op:     port.LifecycleSetStatusClearSuspension,
			Status: domain.StatusTrialExpired,
		})
		return domain.StatusTrialExpired, prevPlan, err
	case "TrialReactivated":
		// TR2 (§15.4, LLD line 4237): trial_ends_at uses per-tier plan.trial_duration_days,
		// not a hardcoded 30 (rev 1.32/A32(g)). T-14 caps trial_reactivation_count at 1;
		// the WHERE clause enforces the one-time cap and the CHECK constraint is the DB backstop.
		// trial_duration_days is resolved from the Catalog Service before this tx opened
		// (Handle's pre-tx peek) — the `plans` table moved out of this service's own
		// database under ADR-0007, so it can no longer be read via a local subquery.
		rows, err := c.tenants.ApplyLifecyclePatch(ctx, tenantID, port.TenantLifecyclePatch{
			Op:                port.LifecycleTrialReactivate,
			TrialDurationDays: trialDurationDays,
		})
		if err != nil {
			return prevStatus, prevPlan, err
		}
		if rows == 0 {
			c.logger.Warn("TrialReactivated: reactivation cap reached (TRIAL-5), no-op",
				"tenant_id", tenantID, "event_id", env.ID)
			return prevStatus, prevPlan, nil
		}
		return domain.StatusTrial, prevPlan, nil
	case "TenantSuspended":
		// T-16 (new, resolves RP-11): source distinguishes the normal Billing-
		// driven cancelled→suspended lapse from an RP-14 operator-sourced
		// administrative suspension that can land directly on active/trial.
		// Confirmed by RP (LLD §16 OQ-7): the field is always present, but we
		// still default to billing_lapse if it's ever absent — a defensive
		// fallback, never relied upon in practice.
		var payload struct {
			Source string `json:"source"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return prevStatus, prevPlan, err
		}
		op := port.LifecycleSuspendBillingLapse
		if domain.SuspensionSource(payload.Source) == domain.SuspensionSourceOperator {
			op = port.LifecycleSuspendOperator
		}
		_, err := c.tenants.ApplyLifecyclePatch(ctx, tenantID, port.TenantLifecyclePatch{Op: op})
		return domain.StatusSuspended, prevPlan, err
	case "TenantOffboarded":
		// PAID-1: terminal. Per tenant-offboarding-workflow doc — O&M
		// scrubs its own row; does NOT cascade-call UP's DELETE.
		// T-16: suspension_source cleared — 'offboarded' requires it NULL
		// (chk_suspension_source_required), including when reached from
		// an operator-suspended tenant.
		_, err := c.tenants.ApplyLifecyclePatch(ctx, tenantID, port.TenantLifecyclePatch{Op: port.LifecycleOffboard})
		return domain.StatusOffboarded, prevPlan, err

	// ── billing-orgm-q (Billing-produced) ───────────────────────────────
	case "TenantPlanChanged":
		var payload struct {
			Plan string `json:"plan"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return prevStatus, prevPlan, err
		}
		newPlan := domain.TenantPlan(payload.Plan)
		if newPlan == "" {
			newPlan = prevPlan
		}
		_, err := c.tenants.ApplyLifecyclePatch(ctx, tenantID, port.TenantLifecyclePatch{
			Op:   port.LifecycleSetPlan,
			Plan: newPlan,
		})
		return prevStatus, newPlan, err
	case "TenantPaymentPastDue":
		_, err := c.tenants.ApplyLifecyclePatch(ctx, tenantID, port.TenantLifecyclePatch{
			Op:     port.LifecycleSetStatusClearSuspension,
			Status: domain.StatusPastDue,
		})
		return domain.StatusPastDue, prevPlan, err
	case "TenantSubscriptionCancelled":
		// Preserve any existing cancelled_at (e.g. tenant was previously suspended
		// with cancelled_at set). Replaying this event must NOT reset the §15.5
		// retention/grace clock. Parity with TenantSuspended/TenantOffboarded.
		// T-16: suspension_source cleared — 'cancelled' requires it NULL.
		_, err := c.tenants.ApplyLifecyclePatch(ctx, tenantID, port.TenantLifecyclePatch{Op: port.LifecycleCancel})
		return domain.StatusCancelled, prevPlan, err
	case "TenantReactivated":
		if prevStatus == domain.StatusOffboarded {
			c.logger.Warn("TenantReactivated on offboarded tenant — rejecting (PAID-1)", "tenant_id", tenantID)
			return prevStatus, prevPlan, nil
		}
		// T-16 (resolves RP-11): clears suspension_source alongside cancelled_at,
		// regardless of which path (billing_lapse or operator) led to 'suspended'.
		// Target status is derived, not hardcoded to 'active': an operator can
		// suspend a never-converted trial tenant directly (subscription_started_at
		// still NULL), and reactivating that tenant to 'active' would violate
		// chk_subscription_started_required — it must return to 'trial' instead.
		// A tenant that was ever paid (subscription_started_at set) still resolves
		// to 'active', unchanged from the prior behavior. Two conditioned UPDATEs
		// (not a QueryRow+RETURNING) to keep this Exec-only, matching every other
		// case in this switch.
		rows, err := c.tenants.ApplyLifecyclePatch(ctx, tenantID, port.TenantLifecyclePatch{Op: port.LifecycleReactivatePaid})
		if err != nil {
			return prevStatus, prevPlan, err
		}
		if rows > 0 {
			return domain.StatusActive, prevPlan, nil
		}
		if _, err := c.tenants.ApplyLifecyclePatch(ctx, tenantID, port.TenantLifecyclePatch{Op: port.LifecycleReactivateTrial}); err != nil {
			return prevStatus, prevPlan, err
		}
		return domain.StatusTrial, prevPlan, nil
	case "TenantSeatsChanged":
		var payload struct {
			LicensedSeats int `json:"licensed_seats"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return prevStatus, prevPlan, err
		}
		_, err := c.tenants.ApplyLifecyclePatch(ctx, tenantID, port.TenantLifecyclePatch{
			Op:            port.LifecycleSetLicensedSeats,
			LicensedSeats: payload.LicensedSeats,
		})
		return prevStatus, prevPlan, err
	}
	return prevStatus, prevPlan, nil
}

// ── event classification ─────────────────────────────────────────────────

type eventKind int

const (
	kindUnknown eventKind = iota
	kindTenantLifecycle
	kindBilling
)

var knownEvents = map[string]eventKind{
	"TenantIdentityProvisioned":   kindTenantLifecycle,
	"TenantRealmReady":            kindTenantLifecycle,
	"TenantConverted":             kindTenantLifecycle,
	"DirectPaidSignup":            kindTenantLifecycle,
	"TrialExpired":                kindTenantLifecycle,
	"TrialReactivated":            kindTenantLifecycle,
	"TenantSuspended":             kindTenantLifecycle,
	"TenantOffboarded":            kindTenantLifecycle,
	"TenantPlanChanged":           kindBilling,
	"TenantPaymentPastDue":        kindBilling,
	"TenantSubscriptionCancelled": kindBilling,
	"TenantReactivated":           kindBilling,
	"TenantSeatsChanged":          kindBilling,
}

func classify(eventType string) eventKind {
	if k, ok := knownEvents[eventType]; ok {
		return k
	}
	return kindUnknown
}

// ── tenant provisioning backstop (TenantIdentityProvisioned) ─────────────

// tenantIdentityProvisionedPayload is the subset of RP's frozen payload
// (RP-D16) that I-1 needs. realm/realm_type are not read: I-1 creates every
// tenant on the shared trial realm and RP's TenantRealmReady (retried until
// the tenant exists) sets a direct-paid tenant's dedicated realm.
type tenantIdentityProvisionedPayload struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	OwnerUserID uuid.UUID `json:"owner_user_id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Plan        string    `json:"plan"`
	SignupType  string    `json:"signup_type"`
}

// handleTenantIdentityProvisioned is the signup provisioning backstop
// (Signup BFF BFF-2, option B). RP emits the event once the admin's Keycloak
// user exists; if the tenant was never created — the BFF's I-1 call failed
// and nobody retried — run I-1 from the payload. I-1 is idempotent on
// tenant_id, so racing the BFF's own retries is safe. A tenant that already
// exists, including a soft-deleted (offboarded) one, is left alone.
//
// I-1 calls the Catalog Service before its transaction, and an HTTP call has
// no business inside the processed_events claim transaction, so I-1 runs
// first and the dedup claim afterwards. A crash between the two redelivers
// the event; I-1 then finds the tenant and the claim is recorded.
func (c *MembershipEventConsumer) handleTenantIdentityProvisioned(ctx, gucCtx context.Context, tenantID uuid.UUID, env events.Envelope[json.RawMessage]) error {
	var p tenantIdentityProvisionedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return c.rejectProvisioning(env, fmt.Errorf("decode payload: %w", err))
	}
	if p.TenantID != tenantID {
		return c.rejectProvisioning(env, fmt.Errorf("payload tenant_id %s does not match envelope tenant_id %s", p.TenantID, tenantID))
	}

	result := metrics.ProvisioningBackstopAlreadyExists
	_, err := c.tenants.FindByIDIncludingDeleted(gucCtx, tenantID)
	switch {
	case err == nil:
		// Created already (by the BFF's I-1, or an earlier delivery), or
		// offboarded: never recreate.
	case !errors.Is(err, domain.ErrTenantNotFound):
		return fmt.Errorf("TenantIdentityProvisioned: look up tenant: %w", err)
	case c.provisioner == nil:
		return errors.New("TenantIdentityProvisioned: no TenantProvisioner configured")
	default:
		_, created, perr := c.provisioner.TrialSignup(ctx, service.TrialSignupInput{
			TenantID:    p.TenantID,
			Slug:        p.Slug,
			Name:        p.Name,
			Plan:        domain.TenantPlan(p.Plan),
			OwnerUserID: p.OwnerUserID,
		})
		switch {
		case perr == nil && created:
			result = metrics.ProvisioningBackstopCreated
		case perr == nil:
			// Lost the race to the BFF's own I-1 call — the tenant exists.
		case isPermanentProvisioningError(perr):
			return c.rejectProvisioning(env, perr)
		default:
			return fmt.Errorf("TenantIdentityProvisioned: provision tenant: %w", perr)
		}
	}

	if err := processOnce(gucCtx, c.inbox, consumerName, env, func(context.Context) error { return nil }); err != nil {
		return err
	}
	metrics.IncProvisioningBackstop(result)
	if result == metrics.ProvisioningBackstopCreated {
		c.logger.Warn("tenant_provisioned_by_backstop — signup's own I-1 call had not completed",
			"event_id", env.ID, "tenant_id", tenantID, "signup_type", p.SignupType)
	}
	return nil
}

// isPermanentProvisioningError reports I-1 failures no retry can fix: the
// payload fails I-1's validation, or the slug belongs to another tenant.
func isPermanentProvisioningError(err error) bool {
	return errors.Is(err, domain.ErrValidation) ||
		errors.Is(err, domain.ErrInvalidPlan) ||
		errors.Is(err, domain.ErrSlugAlreadyTaken)
}

func (c *MembershipEventConsumer) rejectProvisioning(env events.Envelope[json.RawMessage], cause error) error {
	metrics.IncConsumedEventRejected(env.Type, "provisioning_rejected")
	c.logger.Error("TenantIdentityProvisioned rejected — DLQ; the signup needs manual completion",
		"event_id", env.ID, "tenant_id", env.TenantID, "error", cause.Error())
	return fmt.Errorf("%w: %v", ErrProvisioningRejected, cause)
}

// tenantAbsent decides what happens to a lifecycle/billing event whose
// tenant has no live row. A soft-deleted (offboarded) row: record and drop.
// TenantOffboarded (nothing to offboard): drop. No row at all: the tenant is
// simply not created yet — iam.tenant.events and billing.events are
// unordered (CD-50) — so the event is parked in pending_tenant_events and
// acknowledged (EVT-18); I-1 replays it as soon as it creates the row.
//
// Parking holds the tenant's provisioning lock, which I-1 also holds while
// creating the row, then re-checks the row: if I-1 committed in the
// meantime, the returned lock lets the caller apply the event at once. So an
// event is never parked after I-1's replay has run.
//
// Without a parked-event store (tests, legacy wiring) it falls back to the
// EVT-17 retry: the tx rolls back and SQS redelivers until the tenant
// exists or the message reaches the DLQ.
func (c *MembershipEventConsumer) tenantAbsent(txCtx context.Context, tenantID uuid.UUID, env events.Envelope[json.RawMessage]) (*port.TenantProjectionLock, error) {
	if env.Type == "TenantOffboarded" {
		return nil, nil
	}
	_, err := c.tenants.FindByIDIncludingDeleted(txCtx, tenantID)
	switch {
	case err == nil:
		return nil, nil // soft-deleted: drop
	case !errors.Is(err, domain.ErrTenantNotFound):
		return nil, err
	}
	if c.parked == nil {
		c.logger.Info("event for a tenant not provisioned yet — retrying",
			"event_id", env.ID, "event_type", env.Type, "tenant_id", tenantID)
		return nil, errTenantNotYetProvisioned
	}

	if err := c.parked.LockTenantProvisioning(txCtx, tenantID); err != nil {
		return nil, err
	}
	locked, err := c.tenants.LockForProjection(txCtx, tenantID)
	if err != nil || locked != nil {
		return locked, err
	}

	eventID, err := uuid.Parse(env.ID)
	if err != nil {
		return nil, fmt.Errorf("park event: envelope id %q is not a uuid: %w", env.ID, err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("park event %s: encode envelope: %w", env.ID, err)
	}
	if err := c.parked.Park(txCtx, port.ParkedTenantEvent{
		EventID:    eventID,
		TenantID:   tenantID,
		EventType:  env.Type,
		OccurredAt: env.Timestamp,
		Envelope:   raw,
	}); err != nil {
		return nil, err
	}
	metrics.IncTenantEventParked(env.Type)
	c.logger.Info("event for a tenant not provisioned yet — parked until I-1 creates it",
		"event_id", env.ID, "event_type", env.Type, "tenant_id", tenantID)
	return nil, nil
}

// ReplayParked applies the tenant's parked events (EVT-18) now that its row
// exists. I-1 calls it right after creating a tenant.
func (c *MembershipEventConsumer) ReplayParked(ctx context.Context, tenantID uuid.UUID) (int, error) {
	return c.replayParked(ctx, tenantID, metrics.ReplayTriggerProvisioning)
}

// ReplayParkedSweep is ReplayParked for the parked-events-replay CronJob,
// which backstops a replay I-1 never ran (a crash after its commit) or one
// that failed.
func (c *MembershipEventConsumer) ReplayParkedSweep(ctx context.Context, tenantID uuid.UUID) (int, error) {
	return c.replayParked(ctx, tenantID, metrics.ReplayTriggerSweep)
}

var _ port.ParkedTenantEventReplayer = (*MembershipEventConsumer)(nil)

// replayParked applies each parked event in occurred_at order, one
// transaction per event: claim (DELETE) the parked row, lock the tenant row,
// then run the same projection the original delivery would have. A
// concurrent replay that already claimed an event makes the claim a no-op.
// The EVT-14 recency guard still applies, so a parked event older than one
// already applied is skipped, exactly as a late delivery would be. It stops
// at the first failure so later events never apply ahead of an earlier one;
// the next replay resumes from there.
func (c *MembershipEventConsumer) replayParked(ctx context.Context, tenantID uuid.UUID, trigger string) (int, error) {
	if c.parked == nil || c.txRunner == nil {
		return 0, nil
	}
	gucCtx := tenantGUC(ctx, tenantID)
	pending, err := c.parked.ListForTenant(gucCtx, tenantID)
	if err != nil {
		return 0, fmt.Errorf("list parked events: %w", err)
	}
	replayed := 0
	for _, p := range pending {
		var env events.Envelope[json.RawMessage]
		if err := json.Unmarshal(p.Envelope, &env); err != nil {
			return replayed, fmt.Errorf("decode parked event %s: %w", p.EventID, err)
		}
		trialDurationDays, err := c.trialDurationDays(ctx, gucCtx, tenantID, env)
		if err != nil {
			return replayed, err
		}
		var applied, gdprWipeRan bool
		err = c.txRunner.RunInTx(gucCtx, func(txCtx context.Context) error {
			claimed, err := c.parked.Claim(txCtx, p.EventID)
			if err != nil || !claimed {
				return err
			}
			locked, err := c.tenants.LockForProjection(txCtx, tenantID)
			if err != nil {
				return err
			}
			if locked == nil {
				_, ferr := c.tenants.FindByIDIncludingDeleted(txCtx, tenantID)
				switch {
				case ferr == nil:
					return nil // offboarded since it was parked: drop, as live delivery would
				case errors.Is(ferr, domain.ErrTenantNotFound):
					return errTenantNotYetProvisioned // roll back: stays parked
				default:
					return ferr
				}
			}
			applied = true
			gdprWipeRan, err = c.project(txCtx, tenantID, env, locked, trialDurationDays)
			return err
		})
		if errors.Is(err, errTenantNotYetProvisioned) {
			return replayed, nil
		}
		if err != nil {
			return replayed, fmt.Errorf("replay parked %s %s: %w", env.Type, p.EventID, err)
		}
		if gdprWipeRan {
			c.evictAfterWipe(ctx, tenantID)
		}
		if applied {
			metrics.IncTenantEventReplayed(env.Type, trigger)
			replayed++
		}
	}
	if replayed > 0 {
		c.logger.Info("replayed parked tenant events", "tenant_id", tenantID, "replayed", replayed, "trigger", trigger)
	}
	return replayed, nil
}
