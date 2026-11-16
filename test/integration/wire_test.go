//go:build integration

// Phase 12 · wire tests — outbox → RoutingPublisher → SNS → SQS → raw
// receive. Verifies:
//
//   - P12-RT-001: outbox row goes through the outbox runner, publishes to the
//     correct SNS topic, and materialises as an SQS message with a parseable
//     platform-events Envelope[json.RawMessage] body (RawMessageDelivery=true).
//   - P12-ROUTE-001/002: RoutingPublisher dispatches by domain.TopicForEvent
//     — tenant-lane events reach only tenant subscribers, membership-lane
//     events reach only membership subscribers.
//   - P12-FILTER-001..004: SNS FilterPolicy on the EventType attribute
//     matches the LLD §7.3.2 fan-out lists exactly (audit no-filter,
//     authz/realm/notification/workflow/billing per-list).
//   - P12-ATTR-001: the publisher stamps EventType as an SNS MessageAttribute
//     — without this, every filter policy would silently drop everything.
package integration_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/eventbus"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// startOutboxRunner spins up an outbox runner drained through the given
// RoutingPublisher and returns a stop func. Poll interval is tight (100ms)
// so wire tests observe results in <1s.
func startOutboxRunner(t *testing.T, env *phase12Env, rp *eventbusadapter.RoutingPublisher) func() {
	t.Helper()
	// Build a pgcommon.Pool over the raw DSN — the outbox runner needs
	// pgcommon.Pool (not pgxpool) for its Acquire semantics.
	appPool, err := pgcommon.NewPool(env.ctx, pgcommon.Config{
		DSN:           env.dsn,
		PGBouncerMode: false,
	})
	require.NoError(t, err)

	runner, err := outbox.NewRunner(outbox.Config{
		Pool:               appPool,
		Publisher:          rp,
		PollInterval:       100 * time.Millisecond,
		BatchSize:          20,
		MaxAttempts:        5,
		PublishConcurrency: 2,
		PublishTimeout:     5 * time.Second,
		DrainTimeout:       5 * time.Second,
	})
	require.NoError(t, err)

	runCtx, cancel := context.WithCancel(env.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runner.Start(runCtx)
	}()
	return func() {
		cancel()
		<-done
		_ = runner.Stop()
		appPool.Close()
	}
}

// enqueueDomainEvent writes one DomainEvent to outbox_events via the real
// eventbus Publisher — exercises the same path service code uses (Enqueue
// inside a rawPool tx). Returns the generated envelope ID.
func enqueueDomainEvent(t *testing.T, env *phase12Env, evt *domain.DomainEvent) string {
	t.Helper()
	rawCodec := eventbusadapter.Codec(eventbusadapter.NoopCodec{})
	codec, err := eventbusadapter.NewValidatingCodec(rawCodec)
	require.NoError(t, err)
	pub := eventbusadapter.New("iam-org-membership-test", codec)

	require.NoError(t, env.rawPool.WithTx(env.ctx, func(ctx context.Context, tx pgx.Tx) error {
		return pub.Enqueue(pgadapter.WithTx(ctx, tx), evt)
	}))

	// Grab the just-inserted envelope ID so callers can correlate the
	// downstream SQS message.
	var id string
	require.NoError(t, env.rawPool.QueryRow(env.ctx,
		`SELECT id FROM outbox_events ORDER BY created_at DESC LIMIT 1`).Scan(&id))
	return id
}

// ── P12-RT-001 ──────────────────────────────────────────────────────────────

// TestOutboxRoundTrip — insert one DomainEvent via the outbox
// publisher, start the outbox runner, and assert the corresponding message
// materialises on a subscribed SQS queue with the wire-format envelope
// intact (id, type, source, tenant_id, data).
func TestOutboxRoundTrip(t *testing.T) {
	e := newPhase12Env(t)

	membershipTopic := e.createTopic(t, "iam-membership-events")
	tenantTopic := e.createTopic(t, "iam-tenant-events")
	auditQ := e.createQueue(t, "membership-audit-q", "", 0)
	e.subscribeQueue(t, membershipTopic, auditQ, "")

	membershipPub, err := events.NewSNSPublisher(events.SNSConfig{
		TopicARN: membershipTopic, Region: flociRegion, EndpointURL: e.endpoint,
	})
	require.NoError(t, err)
	tenantPub, err := events.NewSNSPublisher(events.SNSConfig{
		TopicARN: tenantTopic, Region: flociRegion, EndpointURL: e.endpoint,
	})
	require.NoError(t, err)
	rp := eventbusadapter.NewRoutingPublisher(membershipPub, tenantPub)

	stopRunner := startOutboxRunner(t, e, rp)
	defer stopRunner()

	tenantID := uuid.New()
	actorID := uuid.New()
	userID := uuid.New()
	envelopeID := enqueueDomainEvent(t, e, &domain.DomainEvent{
		Type:       domain.EventTenantRoleGranted,
		TenantID:   tenantID,
		Subject:    userID.String(),
		Actor:      actorID.String(),
		OccurredAt: time.Now().UTC(),
		Data: domain.TenantRoleGrantedPayload{
			UserID:   userID,
			TenantID: tenantID,
			RoleCode: domain.RoleTenantAdmin,
			ActorID:  actorID,
		},
	})

	msgs := e.receiveMessages(t, auditQ, 1, 15*time.Second)
	require.Len(t, msgs, 1, "P12-RT-001: exactly one message must land on audit queue")

	var wire events.Envelope[json.RawMessage]
	require.NoError(t, json.Unmarshal([]byte(*msgs[0].Body), &wire))
	assert.Equal(t, envelopeID, wire.ID, "P12-RT-001: envelope ID round-trips")
	assert.Equal(t, domain.EventTenantRoleGranted, wire.Type, "P12-RT-001: event type round-trips")
	assert.Equal(t, tenantID.String(), wire.TenantID, "P12-RT-001: tenant_id round-trips")
	assert.Equal(t, "iam-org-membership-test", wire.Source, "P12-RT-001: source stamped")
	assert.NotEmpty(t, wire.Payload, "P12-RT-001: payload preserved")
}

// ── P12-ROUTE-001 ───────────────────────────────────────────────────────────

// TestTenantLaneIsolated — TenantCreated must land on the
// tenant-topic subscribers only. A membership subscriber to the OTHER topic
// must NOT observe it.
func TestTenantLaneIsolated(t *testing.T) {
	e := newPhase12Env(t)

	membershipTopic := e.createTopic(t, "iam-membership-events")
	tenantTopic := e.createTopic(t, "iam-tenant-events")

	tenantAuditQ := e.createQueue(t, "tenant-audit-q", "", 0)
	membershipAuditQ := e.createQueue(t, "membership-audit-q", "", 0)
	e.subscribeQueue(t, tenantTopic, tenantAuditQ, "")
	e.subscribeQueue(t, membershipTopic, membershipAuditQ, "")

	membershipPub, err := events.NewSNSPublisher(events.SNSConfig{
		TopicARN: membershipTopic, Region: flociRegion, EndpointURL: e.endpoint,
	})
	require.NoError(t, err)
	tenantPub, err := events.NewSNSPublisher(events.SNSConfig{
		TopicARN: tenantTopic, Region: flociRegion, EndpointURL: e.endpoint,
	})
	require.NoError(t, err)
	rp := eventbusadapter.NewRoutingPublisher(membershipPub, tenantPub)

	stopRunner := startOutboxRunner(t, e, rp)
	defer stopRunner()

	tenantID := uuid.New()
	enqueueDomainEvent(t, e, &domain.DomainEvent{
		Type:       domain.EventTenantCreated,
		TenantID:   tenantID,
		Subject:    tenantID.String(),
		Actor:      uuid.NewString(),
		OccurredAt: time.Now().UTC(),
		Data: domain.TenantCreatedPayload{
			TenantID: tenantID,
			Slug:     "acme-corp",
			Plan:     domain.PlanStarter,
			Status:   domain.StatusTrial,
		},
	})

	// Tenant queue must receive it.
	got := e.receiveMessages(t, tenantAuditQ, 1, 15*time.Second)
	require.Len(t, got, 1, "P12-ROUTE-001: TenantCreated must land on tenant-topic subscriber")

	// Membership queue must NOT receive it.
	other := e.drainQueue(t, membershipAuditQ, 3*time.Second)
	assert.Empty(t, other, "P12-ROUTE-001: TenantCreated must NOT cross-leak into membership topic")
}

// ── P12-ROUTE-002 ───────────────────────────────────────────────────────────

// TestMembershipLaneIsolated — a membership-lane event
// (DepartmentMembershipGranted) must NOT leak into a tenant-topic subscriber.
func TestMembershipLaneIsolated(t *testing.T) {
	e := newPhase12Env(t)

	membershipTopic := e.createTopic(t, "iam-membership-events")
	tenantTopic := e.createTopic(t, "iam-tenant-events")

	tenantAuditQ := e.createQueue(t, "tenant-audit-q", "", 0)
	membershipAuditQ := e.createQueue(t, "membership-audit-q", "", 0)
	e.subscribeQueue(t, tenantTopic, tenantAuditQ, "")
	e.subscribeQueue(t, membershipTopic, membershipAuditQ, "")

	membershipPub, err := events.NewSNSPublisher(events.SNSConfig{
		TopicARN: membershipTopic, Region: flociRegion, EndpointURL: e.endpoint,
	})
	require.NoError(t, err)
	tenantPub, err := events.NewSNSPublisher(events.SNSConfig{
		TopicARN: tenantTopic, Region: flociRegion, EndpointURL: e.endpoint,
	})
	require.NoError(t, err)
	rp := eventbusadapter.NewRoutingPublisher(membershipPub, tenantPub)

	stopRunner := startOutboxRunner(t, e, rp)
	defer stopRunner()

	tenantID := uuid.New()
	enqueueDomainEvent(t, e, &domain.DomainEvent{
		Type:       domain.EventDepartmentMembershipGranted,
		TenantID:   tenantID,
		Subject:    uuid.NewString(),
		Actor:      uuid.NewString(),
		OccurredAt: time.Now().UTC(),
		Data: domain.DepartmentMembershipGrantedPayload{
			UserID:       uuid.New(),
			TenantID:     tenantID,
			DepartmentID: uuid.New(),
			Level:        domain.DeptPreparator,
			ActorID:      uuid.New(),
		},
	})

	got := e.receiveMessages(t, membershipAuditQ, 1, 15*time.Second)
	require.Len(t, got, 1, "P12-ROUTE-002: membership event must land on membership audit queue")

	other := e.drainQueue(t, tenantAuditQ, 3*time.Second)
	assert.Empty(t, other, "P12-ROUTE-002: membership event must NOT cross-leak into tenant topic")
}

// ── P12-FILTER-002 ──────────────────────────────────────────────────────────

// TestWorkflowQueueDropsOverage — the membership-workflow-q
// filter policy accepts membership-revocation/purge/override/membership/relay
// events. An overage event must be dropped.
func TestWorkflowQueueDropsOverage(t *testing.T) {
	e := newPhase12Env(t)
	topic := e.createTopic(t, "iam-membership-events")
	workflowQ := e.createQueue(t, "membership-workflow-q", "", 0)

	filter := `{"EventType":["TenderAssigneeOverridden","DepartmentMembershipGranted","DepartmentMembershipRevoked","DepartmentMembershipLevelChanged","TenantStateChanged","MembershipRevoked","TenantMembershipsPurged"]}`
	e.subscribeQueue(t, topic, workflowQ, filter)

	// Should reach workflow queue — MembershipRevoked is on the accept list.
	e.publishEnvelope(t, topic, domain.EventMembershipRevoked,
		mustMarshal(t, map[string]string{"user_id": uuid.NewString()}), nil)
	got := e.receiveMessages(t, workflowQ, 1, 10*time.Second)
	require.Len(t, got, 1, "P12-FILTER-002: MembershipRevoked must reach workflow queue")

	// Should reach workflow queue — TenantMembershipsPurged is on the accept
	// list (the only signal Workflow gets for a trial hard-delete).
	e.publishEnvelope(t, topic, domain.EventTenantMembershipsPurged,
		mustMarshal(t, map[string]string{"tenant_id": uuid.NewString()}), nil)
	got = e.receiveMessages(t, workflowQ, 1, 10*time.Second)
	require.Len(t, got, 1, "P12-FILTER-002: TenantMembershipsPurged must reach workflow queue")
}

// ── P12-FILTER-003 ──────────────────────────────────────────────────────────

// TestAuthzQueueOnlyMembershipRoles — membership-authz-q's
// filter accepts dept-membership + tenant-role events AND MembershipRevoked
// (AE-20 / AE-OM-GAP-011: authz enrichment must evict cache on full tenant removal).
func TestAuthzQueueOnlyMembershipRoles(t *testing.T) {
	e := newPhase12Env(t)
	topic := e.createTopic(t, "iam-membership-events")
	authzQ := e.createQueue(t, "membership-authz-q", "", 0)

	filter := `{"EventType":["DepartmentMembershipGranted","DepartmentMembershipRevoked","DepartmentMembershipLevelChanged","TenantRoleGranted","TenantRoleRevoked","MembershipRevoked"]}`
	e.subscribeQueue(t, topic, authzQ, filter)

	// Should reach.
	e.publishEnvelope(t, topic, domain.EventTenantRoleGranted,
		mustMarshal(t, map[string]string{"role": "tender_admin"}), nil)
	got := e.receiveMessages(t, authzQ, 1, 10*time.Second)
	require.Len(t, got, 1, "P12-FILTER-003: TenantRoleGranted must reach authz queue")

	// Must also reach — MembershipRevoked triggers cache eviction in AuthZ Enrichment (AE-20).
	e.publishEnvelope(t, topic, domain.EventMembershipRevoked,
		mustMarshal(t, map[string]string{"user_id": uuid.NewString()}), nil)
	extra := e.drainQueue(t, authzQ, 3*time.Second)
	require.Len(t, extra, 1, "P12-FILTER-003: MembershipRevoked must reach authz queue")
}

// ── P12-FILTER-004 ──────────────────────────────────────────────────────────

// TestRealmQueueDropsRevocation — the realm-q filter
// per LLD §7.3.2 accepts Granted / LevelChanged / TenantRole{Granted,Revoked}
// but NOT DepartmentMembershipRevoked (revocations don't drive requires-mfa).
func TestRealmQueueDropsRevocation(t *testing.T) {
	e := newPhase12Env(t)
	topic := e.createTopic(t, "iam-membership-events")
	realmQ := e.createQueue(t, "membership-realm-q", "", 0)

	filter := `{"EventType":["DepartmentMembershipGranted","DepartmentMembershipLevelChanged","TenantRoleGranted","TenantRoleRevoked"]}`
	e.subscribeQueue(t, topic, realmQ, filter)

	// On-list — must reach.
	e.publishEnvelope(t, topic, domain.EventDepartmentMembershipLevelChanged,
		mustMarshal(t, map[string]string{"level": "approver"}), nil)
	got := e.receiveMessages(t, realmQ, 1, 10*time.Second)
	require.Len(t, got, 1, "P12-FILTER-004: LevelChanged must reach realm queue")

	// Off-list — must be dropped by filter.
	e.publishEnvelope(t, topic, domain.EventDepartmentMembershipRevoked,
		mustMarshal(t, map[string]string{"dept_id": uuid.NewString()}), nil)
	extra := e.drainQueue(t, realmQ, 3*time.Second)
	assert.Empty(t, extra, "P12-FILTER-004: DepartmentMembershipRevoked must NOT reach realm queue")
}

// ── P12-ATTR-001 ────────────────────────────────────────────────────────────

// TestPublisherStampsEventTypeAttribute — the outbox publisher
// MUST attach an "EventType" SNS MessageAttribute. Without it, SNS filter
// policies would silently drop every message. Assert directly by looking at
// the raw SQS message's MessageAttributes after the outbox → SNS → SQS trip.
func TestPublisherStampsEventTypeAttribute(t *testing.T) {
	e := newPhase12Env(t)

	topic := e.createTopic(t, "iam-membership-events")
	auditQ := e.createQueue(t, "membership-audit-q", "", 0)
	e.subscribeQueue(t, topic, auditQ, "")

	membershipPub, err := events.NewSNSPublisher(events.SNSConfig{
		TopicARN: topic, Region: flociRegion, EndpointURL: e.endpoint,
	})
	require.NoError(t, err)
	rp := eventbusadapter.NewRoutingPublisher(membershipPub, nil)

	stopRunner := startOutboxRunner(t, e, rp)
	defer stopRunner()

	tenantID := uuid.New()
	enqueueDomainEvent(t, e, &domain.DomainEvent{
		Type:       domain.EventMembershipRevoked,
		TenantID:   tenantID,
		Subject:    uuid.NewString(),
		Actor:      uuid.NewString(),
		OccurredAt: time.Now().UTC(),
		Data: domain.MembershipRevokedPayload{
			TenantID: tenantID,
			UserID:   uuid.New(),
			ActorID:  uuid.New(),
		},
	})

	msgs := e.receiveMessages(t, auditQ, 1, 15*time.Second)
	require.Len(t, msgs, 1)

	attr, ok := msgs[0].MessageAttributes["EventType"]
	require.True(t, ok, "P12-ATTR-001: EventType MessageAttribute missing — filter policies would fail")
	require.NotNil(t, attr.StringValue)
	assert.Equal(t, domain.EventMembershipRevoked, *attr.StringValue,
		"P12-ATTR-001: EventType attribute must equal event Type")
}

// ── P12-FILTER-CATALOG ──────────────────────────────────────────────────────

// TestCatalogQueueCarriesBothCatalogEvents — catalog-orgm-q is the one queue
// on Catalog's iam-catalog-events topic (LLD §7.1, rev 2.39, which folded in
// catalog-orgm-plans-q). Its filter must admit DepartmentCatalogChanged and
// PlanCatalogChanged — the latter drives the I-8 plan-generation bump — and
// nothing else. Same filter as scripts/init-floci.sh.
func TestCatalogQueueCarriesBothCatalogEvents(t *testing.T) {
	e := newPhase12Env(t)
	topic := e.createTopic(t, "iam-catalog-events")
	catalogQ := e.createQueue(t, "catalog-orgm-q", "", 0)

	filter := `{"EventType":["DepartmentCatalogChanged","PlanCatalogChanged"]}`
	e.subscribeQueue(t, topic, catalogQ, filter)

	catalogEnvelope := func(eventType string, payload any) []byte {
		return mustMarshal(t, events.Envelope[json.RawMessage]{
			ID: uuid.NewString(), Type: eventType, Source: "iam-catalog-admin",
			Timestamp: time.Now().UTC(), Payload: mustMarshal(t, payload),
		})
	}
	e.publishEnvelope(t, topic, "DepartmentCatalogChanged", catalogEnvelope("DepartmentCatalogChanged",
		map[string]string{"department_id": uuid.NewString(), "code": "ENGINEERING", "change_type": "renamed"}), nil)
	e.publishEnvelope(t, topic, "PlanCatalogChanged", catalogEnvelope("PlanCatalogChanged",
		map[string]any{"code": "enterprise", "change_type": "updated", "feature_set": map[string]any{}}), nil)
	got := e.receiveMessages(t, catalogQ, 2, 10*time.Second)
	require.Len(t, got, 2, "P12-FILTER-CATALOG: both catalog event types must reach catalog-orgm-q")

	types := map[string]bool{}
	for _, m := range got {
		var env events.Envelope[json.RawMessage]
		require.NoError(t, json.Unmarshal([]byte(*m.Body), &env))
		types[env.Type] = true
	}
	assert.Equal(t, map[string]bool{"DepartmentCatalogChanged": true, "PlanCatalogChanged": true}, types)

	e.publishEnvelope(t, topic, "SomeOtherCatalogEvent", catalogEnvelope("SomeOtherCatalogEvent", map[string]string{}), nil)
	extra := e.drainQueue(t, catalogQ, 3*time.Second)
	assert.Empty(t, extra, "P12-FILTER-CATALOG: an off-list type must be dropped by the filter")
}
