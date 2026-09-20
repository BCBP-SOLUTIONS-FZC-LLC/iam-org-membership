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
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
)

// consumerName is the (event_id, consumer) key component in processed_events.
// Both queues share one consumer identity so PE-1 dedup covers both.
const consumerName = "iam-org-membership"

// ErrPoisonPill signals the SQS runner to move the message to DLQ without
// recording processed_events. Used for EVT-15 future-time clamp.
var ErrPoisonPill = errors.New("event rejected as poison pill (EVT-15 future-time clamp)")

type MembershipEventConsumer struct {
	txRunner    port.TxRunner
	tenants     port.TenantRepository
	idempotency port.IdempotencyStore
	catalog     port.PlanCatalogReader
	cache       port.Cache
	skew        time.Duration
	logger      port.SlogStyleLogger
}

// NewMembershipEventConsumer builds a MembershipEventConsumer. logger may be
// nil — see MembershipService's constructor doc comment for the fallback/
// production-wiring contract, which applies identically here. idempotency
// may be nil only in tests that never reach a live dedup check/write (e.g.
// constructor-validation tests); production wiring always supplies a real
// port.IdempotencyStore (postgres.IdempotencyRepository). catalog may be nil
// only in tests that never exercise a TrialReactivated event — it resolves
// the reactivated plan's trial_duration_days from the Catalog Service
// (Plans moved out of this service's own database under ADR-0007). cache may
// be nil — the TenantOffboarded GDPR wipe's cache-invalidation step (§15.5)
// is best-effort and skipped entirely when cache is nil, consistent with
// CACHE-2/9 (advisory-only, never a correctness dependency).
func NewMembershipEventConsumer(txRunner port.TxRunner, tenants port.TenantRepository, idempotency port.IdempotencyStore, catalog port.PlanCatalogReader, cache port.Cache, skew time.Duration, logger port.Logger) *MembershipEventConsumer {
	if skew <= 0 {
		skew = 300 * time.Second
	}
	return &MembershipEventConsumer{txRunner: txRunner, tenants: tenants, idempotency: idempotency, catalog: catalog, cache: cache, skew: skew, logger: port.NewSlogStyleLogger(logger)}
}

// Handle is the entry point for platform-events SQS consumer.
func (c *MembershipEventConsumer) Handle(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	// ── EVT-15 future-time clamp ─────────────────────────────────────────
	if !env.Timestamp.IsZero() && env.Timestamp.After(time.Now().UTC().Add(c.skew)) {
		if metrics.DLQMessages != nil {
			metrics.DLQMessages.WithLabelValues(env.Type, "future_time_clamp").Inc()
		}
		c.logger.Warn("EVT-15 future-time clamp — DLQ",
			"event_id", env.ID, "event_type", env.Type, "event_time", env.Timestamp)
		return ErrPoisonPill
	}

	kind := classify(env.Type)
	if kind == kindUnknown {
		return ackUnknown(ctx, c.txRunner, c.idempotency, c.logger, consumerName, env)
	}

	tenantID, err := uuid.Parse(env.TenantID)
	if err != nil {
		return fmt.Errorf("parse tenant_id from envelope: %w", err)
	}

	// Cheap dedup probe outside the tx to save a lock acquisition on replay.
	seen, err := skipDuplicate(ctx, c.idempotency, consumerName, env.ID)
	if err != nil {
		return err
	}
	if seen {
		return nil
	}

	// Set the RLS GUC to the target tenant so RLS lets us see + update the
	// tenants row. Consumers run as the app pool (no BYPASSRLS) per RLS-4.
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.UserID = "iam-system"
	g.TenantID = tenantID.String()
	gucCtx := pgcommon.WithGUCSet(ctx, g)

	// TrialReactivated (§8.10.3/TR2) needs the tenant's current plan's
	// trial_duration_days from the Catalog Service to compute the new
	// trial_ends_at. Catalog is an HTTP call, so — like ProvisioningService.
	// TrialSignup — it must complete before the write transaction below
	// opens; an HTTP call has no business running while a Postgres tx is
	// held open. This short read-only peek at the current plan runs in its
	// own transaction, committed and closed before the HTTP call.
	var trialDurationDays int
	if env.Type == "TrialReactivated" {
		tenant, peekErr := c.tenants.FindByID(gucCtx, tenantID)
		switch {
		case peekErr != nil && errors.Is(peekErr, domain.ErrTenantNotFound):
			// Tenant absent — the write tx below will hit the same
			// missing-row path and take the standard "record dedup, drop" path.
		case peekErr != nil:
			return fmt.Errorf("peek current plan for TrialReactivated: %w", peekErr)
		case tenant == nil:
			// Same as not-found: write tx records dedup and drops.
		case c.catalog == nil:
			return errors.New("TrialReactivated: no PlanCatalogReader configured")
		default:
			plan, err := c.catalog.PlanByCode(ctx, tenant.Plan)
			if err != nil {
				return fmt.Errorf("resolve plan %q for TrialReactivated: %w", tenant.Plan, err)
			}
			trialDurationDays = plan.TrialDurationDays
		}
	}

	var gdprWipeRan bool
	txErr := c.txRunner.RunInTx(gucCtx, func(txCtx context.Context) error {
		locked, err := c.tenants.LockForProjection(txCtx, tenantID)
		if err != nil {
			return err
		}
		if locked == nil {
			// Tenant absent (already offboarded / never provisioned) —
			// record dedup and drop silently.
			return c.idempotency.MarkProcessed(txCtx, consumerName, env.ID)
		}

		// ── EVT-14 recency guard ─────────────────────────────────────────
		if locked.LastEventAt != nil && !env.Timestamp.After(*locked.LastEventAt) {
			if metrics.LifecycleEventSkipped != nil {
				metrics.LifecycleEventSkipped.WithLabelValues(env.Type).Inc()
			}
			c.logger.Info("EVT-14 stale — projection unchanged", "event_id", env.ID, "event_type", env.Type)
			return c.idempotency.MarkProcessed(txCtx, consumerName, env.ID)
		}

		prevStatus := locked.Status
		prevPlan := locked.Plan

		newStatus, newPlan, err := c.applyProjection(txCtx, tenantID, env, prevStatus, prevPlan, trialDurationDays)
		if err != nil {
			return err
		}

		// Bump last_event_at only when projection actually ran.
		if err := c.tenants.SetLastEventAt(txCtx, tenantID, env.Timestamp); err != nil {
			return err
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
				return err
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
				return err
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
				return err
			}
		}

		return c.idempotency.MarkProcessed(txCtx, consumerName, env.ID)
	})
	if txErr != nil {
		return txErr
	}

	// Cache invalidation (§15.5 step 3, CACHE-8) runs after the tx commits —
	// Valkey isn't part of the Postgres transaction, and cache is advisory
	// only (CACHE-2/9): a failure here never rolls back or fails the wipe
	// that already committed. Best-effort, skipped entirely if cache is nil.
	if gdprWipeRan && c.cache != nil {
		if err := c.cache.Delete(ctx, gdprWipeCacheKeys(tenantID)...); err != nil {
			c.logger.Warn("GDPR wipe: cache invalidation failed (advisory-only, not fatal)",
				"tenant_id", tenantID, "error", err)
		}
	}
	return nil
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
	case "TrialTenantProvisioned":
		return prevStatus, prevPlan, nil
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
		// RP's actual DirectPaidSignupPayload (iam-realm-provisioner
		// internal/core/domain/event.go) is {tenant_id, realm,
		// direct_signup} — it has no "plan" field, so this read always
		// misses and always falls through to prevPlan below. A prior
		// commit on this branch claimed RP had added "plan" to the
		// payload and that the fallback was now just a safety net for
		// "older RP versions" — that was never true; verified against
		// RP's real source, not the claim. Left as-is (harmless: the
		// fallback already does the right thing, activating the tenant
		// at its current plan) rather than removing the dead read, since
		// changing it isn't this fix's job.
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
	"TrialTenantProvisioned":      kindTenantLifecycle,
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
