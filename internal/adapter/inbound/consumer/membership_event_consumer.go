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
// iam-user-profile's IdempotencyStore, but MarkProcessedInTx joins the
// caller's transaction (rather than iam-user-profile's decoupled,
// mark-after-success call) so the dedup write commits atomically with the
// EVT-14 row lock and projection update below. Beyond-window duplicates
// (SQS max 14d + DLQ dwell) are backstopped by EVT-14 recency.
//
// Unknown event types are silently acknowledged, logged at INFO, and
// counted by iam_unknown_event_acknowledged_total (§6 event consumer
// scope — forward-compat, avoids DLQ storm on producer schema additions).
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
	"github.com/jackc/pgx/v5"
)

// consumerName is the (event_id, consumer) key component in processed_events.
// Both queues share one consumer identity so PE-1 dedup covers both.
const consumerName = "iam-org-membership"

// ErrPoisonPill signals the SQS runner to move the message to DLQ without
// recording processed_events. Used for EVT-15 future-time clamp.
var ErrPoisonPill = errors.New("event rejected as poison pill (EVT-15 future-time clamp)")

// OutboxEnqueuer is the contract the consumer uses to emit TenantStateChanged
// (EVT-16) inside the projection tx. The concrete implementation is the
// eventbus Publisher's Enqueue method, wrapped in a tx-scoped closure.
type OutboxEnqueuer interface {
	EnqueueInTx(ctx context.Context, tx pgx.Tx, event *domain.DomainEvent) error
}

type MembershipEventConsumer struct {
	pool        *pgcommon.Pool
	outbox      OutboxEnqueuer
	idempotency port.IdempotencyStore
	skew        time.Duration
	logger      port.SlogStyleLogger
}

// NewMembershipEventConsumer builds a MembershipEventConsumer. logger may be
// nil — see MembershipService's constructor doc comment for the fallback/
// production-wiring contract, which applies identically here. idempotency
// may be nil only in tests that never reach a live dedup check/write (e.g.
// constructor-validation tests); production wiring always supplies a real
// port.IdempotencyStore (postgres.IdempotencyRepository).
func NewMembershipEventConsumer(pool *pgcommon.Pool, outbox OutboxEnqueuer, idempotency port.IdempotencyStore, skew time.Duration, logger port.Logger) *MembershipEventConsumer {
	if skew <= 0 {
		skew = 300 * time.Second
	}
	return &MembershipEventConsumer{pool: pool, outbox: outbox, idempotency: idempotency, skew: skew, logger: port.NewSlogStyleLogger(logger)}
}

// Handle is the entry point for platform-events SQS consumer.
func (c *MembershipEventConsumer) Handle(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	// ── EVT-15 future-time clamp ─────────────────────────────────────────
	if !env.Timestamp.IsZero() && env.Timestamp.After(time.Now().UTC().Add(c.skew)) {
		if metrics.FutureLifecycleEventRejected != nil {
			metrics.FutureLifecycleEventRejected.WithLabelValues(env.Type).Inc()
		}
		c.logger.Warn("EVT-15 future-time clamp — DLQ",
			"event_id", env.ID, "event_type", env.Type, "event_time", env.Timestamp)
		return ErrPoisonPill
	}

	kind := classify(env.Type)
	if kind == kindUnknown {
		if metrics.UnknownEventAcknowledged != nil {
			metrics.UnknownEventAcknowledged.WithLabelValues("unknown", env.Type).Inc()
		}
		c.logger.Info("unknown event type — silently acknowledging",
			"event_id", env.ID, "event_type", env.Type)
		return pgcommon.RunInTx(ctx, c.pool, pgx.TxOptions{}, func(txCtx context.Context, tx pgx.Tx) error {
			return c.idempotency.MarkProcessedInTx(txCtx, tx, consumerName, env.ID)
		})
	}

	tenantID, err := uuid.Parse(env.TenantID)
	if err != nil {
		return fmt.Errorf("parse tenant_id from envelope: %w", err)
	}

	// Cheap dedup probe outside the tx to save a lock acquisition on replay.
	seen, err := c.idempotency.IsProcessed(ctx, consumerName, env.ID)
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

	return pgcommon.RunInTx(gucCtx, c.pool, pgx.TxOptions{}, func(txCtx context.Context, tx pgx.Tx) error {
		// Lock the tenant row (EVT-14 needs consistent last_event_at read).
		var currentStatus, currentPlan string
		var lastEventAt *time.Time
		err := tx.QueryRow(txCtx, `
			SELECT status, plan, last_event_at
			FROM tenants WHERE id = $1 AND deleted_at IS NULL
			FOR UPDATE`, tenantID).Scan(&currentStatus, &currentPlan, &lastEventAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Tenant absent (already offboarded / never provisioned) —
				// record dedup and drop silently.
				return c.idempotency.MarkProcessedInTx(txCtx, tx, consumerName, env.ID)
			}
			return err
		}

		// ── EVT-14 recency guard ─────────────────────────────────────────
		if lastEventAt != nil && !env.Timestamp.After(*lastEventAt) {
			if metrics.StaleLifecycleEventSkipped != nil {
				metrics.StaleLifecycleEventSkipped.WithLabelValues(env.Type).Inc()
			}
			c.logger.Info("EVT-14 stale — projection unchanged", "event_id", env.ID, "event_type", env.Type)
			return c.idempotency.MarkProcessedInTx(txCtx, tx, consumerName, env.ID)
		}

		prevStatus := domain.SubscriptionStatus(currentStatus)
		prevPlan := domain.TenantPlan(currentPlan)

		newStatus, newPlan, err := c.applyProjection(txCtx, tx, tenantID, env, prevStatus, prevPlan)
		if err != nil {
			return err
		}

		// Bump last_event_at only when projection actually ran.
		if _, err := tx.Exec(txCtx, `UPDATE tenants SET last_event_at = $2 WHERE id = $1`, tenantID, env.Timestamp); err != nil {
			return err
		}

		// ── EVT-16 tenant-state relay ────────────────────────────────────
		if c.outbox != nil && (newStatus != prevStatus || newPlan != prevPlan) {
			if err := c.outbox.EnqueueInTx(txCtx, tx, &domain.DomainEvent{
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
		if c.outbox != nil && prevStatus != domain.StatusOffboarded && newStatus == domain.StatusOffboarded {
			if err := c.outbox.EnqueueInTx(txCtx, tx, &domain.DomainEvent{
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

		return c.idempotency.MarkProcessedInTx(txCtx, tx, consumerName, env.ID)
	})
}

func (c *MembershipEventConsumer) applyProjection(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, env events.Envelope[json.RawMessage], prevStatus domain.SubscriptionStatus, prevPlan domain.TenantPlan) (domain.SubscriptionStatus, domain.TenantPlan, error) {
	switch env.Type {
	// ── tenant-orgm-q (Realm-Provisioner-produced) ──────────────────────
	case "TrialTenantProvisioned":
		return prevStatus, prevPlan, nil
	case "TenantRealmReady":
		var payload struct {
			RealmID       string `json:"realm_id"`
			RealmType     string `json:"realm_type"`
			KeycloakShard string `json:"keycloak_shard"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return prevStatus, prevPlan, err
		}
		_, err := tx.Exec(ctx, `
			UPDATE tenants SET realm_id = $2, realm_type = $3, keycloak_shard = $4
			WHERE id = $1`, tenantID, payload.RealmID, payload.RealmType, payload.KeycloakShard)
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
		_, err := tx.Exec(ctx, `
			UPDATE tenants SET status = 'active', subscription_started_at = now(), plan = $2
			WHERE id = $1`, tenantID, string(newPlan))
		return domain.StatusActive, newPlan, err
	case "DirectPaidSignup":
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
		_, err := tx.Exec(ctx, `
			UPDATE tenants SET status = 'active', subscription_started_at = now(), plan = $2
			WHERE id = $1`, tenantID, string(newPlan))
		return domain.StatusActive, newPlan, err
	case "TrialExpired":
		_, err := tx.Exec(ctx, `UPDATE tenants SET status = 'trial_expired' WHERE id = $1`, tenantID)
		return domain.StatusTrialExpired, prevPlan, err
	case "TrialReactivated":
		// TR2 (§15.4, LLD line 4237): trial_ends_at uses per-tier plan.trial_duration_days,
		// not a hardcoded 30 (rev 1.32/A32(g)). T-14 caps trial_reactivation_count at 1;
		// the WHERE clause enforces the one-time cap and the CHECK constraint is the DB backstop.
		tag, err := tx.Exec(ctx, `
			UPDATE tenants t
			SET status = 'trial',
			    trial_ends_at = now() + make_interval(days => (SELECT trial_duration_days FROM plans WHERE code = t.plan)),
			    trial_reactivation_count = trial_reactivation_count + 1
			WHERE t.id = $1 AND t.trial_reactivation_count < 1`, tenantID)
		if err != nil {
			return prevStatus, prevPlan, err
		}
		if tag.RowsAffected() == 0 {
			c.logger.Warn("TrialReactivated: reactivation cap reached (TRIAL-5), no-op",
				"tenant_id", tenantID, "event_id", env.ID)
			return prevStatus, prevPlan, nil
		}
		return domain.StatusTrial, prevPlan, nil
	case "TenantSuspended":
		// T-11 biconditional (LLD line 704): cancelled_at IS NOT NULL iff status IN
		// (cancelled, suspended, offboarded). COALESCE preserves an existing timestamp
		// (idempotent replay after a manual suspend).
		_, err := tx.Exec(ctx, `
			UPDATE tenants SET status = 'suspended',
			                    cancelled_at = COALESCE(cancelled_at, now())
			WHERE id = $1`, tenantID)
		return domain.StatusSuspended, prevPlan, err
	case "TenantOffboarded":
		// PAID-1: terminal. Per tenant-offboarding-workflow doc — O&M
		// scrubs its own row; does NOT cascade-call UP's DELETE.
		_, err := tx.Exec(ctx, `
			UPDATE tenants SET status = 'offboarded', deleted_at = now(),
			                    cancelled_at = COALESCE(cancelled_at, now())
			WHERE id = $1`, tenantID)
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
		// feature_flags untouched (T-9).
		_, err := tx.Exec(ctx, `UPDATE tenants SET plan = $2 WHERE id = $1`, tenantID, string(newPlan))
		return prevStatus, newPlan, err
	case "TenantPaymentPastDue":
		_, err := tx.Exec(ctx, `UPDATE tenants SET status = 'past_due' WHERE id = $1`, tenantID)
		return domain.StatusPastDue, prevPlan, err
	case "TenantSubscriptionCancelled":
		// Preserve any existing cancelled_at (e.g. tenant was previously suspended
		// with cancelled_at set). Replaying this event must NOT reset the §15.5
		// retention/grace clock. Parity with TenantSuspended/TenantOffboarded.
		_, err := tx.Exec(ctx, `UPDATE tenants SET status = 'cancelled', cancelled_at = COALESCE(cancelled_at, now()) WHERE id = $1`, tenantID)
		return domain.StatusCancelled, prevPlan, err
	case "TenantReactivated":
		if prevStatus == domain.StatusOffboarded {
			c.logger.Warn("TenantReactivated on offboarded tenant — rejecting (PAID-1)", "tenant_id", tenantID)
			return prevStatus, prevPlan, nil
		}
		_, err := tx.Exec(ctx, `UPDATE tenants SET status = 'active', cancelled_at = NULL WHERE id = $1`, tenantID)
		return domain.StatusActive, prevPlan, err
	case "TenantSeatsChanged":
		var payload struct {
			LicensedSeats int `json:"licensed_seats"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return prevStatus, prevPlan, err
		}
		// SEAT-2: unconditional accept.
		_, err := tx.Exec(ctx, `UPDATE tenants SET licensed_seats = $2 WHERE id = $1`, tenantID, payload.LicensedSeats)
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
