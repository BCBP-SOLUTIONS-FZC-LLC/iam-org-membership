// Phase 10 — outbound clients · EventBus RoutingPublisher.
//
// Module:   iam-org-membership
// Feature:  Two-topic routing (§7.3 · §16 A61)
// File:     internal/adapter/outbound/eventbus/routing_publisher.go
//
// Test IDs: P10-ROUTER-NNN.
package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spyPublisher captures envelopes routed to it for later assertion.
type spyPublisher struct {
	mu       sync.Mutex
	got      []events.Envelope[json.RawMessage]
	failNext bool
}

func (s *spyPublisher) Publish(_ context.Context, env events.Envelope[json.RawMessage]) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext {
		s.failNext = false
		return errors.New("spy: forced failure")
	}
	s.got = append(s.got, env)
	return nil
}

func (s *spyPublisher) PublishBatch(_ context.Context, envs []events.Envelope[json.RawMessage]) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext {
		s.failNext = false
		return errors.New("spy: forced failure")
	}
	s.got = append(s.got, envs...)
	return nil
}

func (s *spyPublisher) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

// mkEnvelope for router tests.
func mkRouterEnv(eventType string) events.Envelope[json.RawMessage] {
	return events.Envelope[json.RawMessage]{
		ID:      "test-id",
		Type:    eventType,
		Source:  "iam-org-membership",
		Payload: json.RawMessage(`{}`),
	}
}

// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P10-ROUTER-001
// Feature:           TenantCreated routes to tenant lane
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestP10Router001_TenantCreatedGoesToTenantLane(t *testing.T) {
	membership, tenant := &spyPublisher{}, &spyPublisher{}
	rp := NewRoutingPublisher(membership, tenant)
	require.NoError(t, rp.Publish(context.Background(), mkRouterEnv(domain.EventTenantCreated)))
	assert.Equal(t, 1, tenant.count())
	assert.Equal(t, 0, membership.count())
}

// Test Case ID:      P10-ROUTER-002
// Feature:           TrialStarted routes to tenant lane
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10Router002_TrialStartedGoesToTenantLane(t *testing.T) {
	membership, tenant := &spyPublisher{}, &spyPublisher{}
	rp := NewRoutingPublisher(membership, tenant)
	require.NoError(t, rp.Publish(context.Background(), mkRouterEnv(domain.EventTrialStarted)))
	assert.Equal(t, 1, tenant.count())
	assert.Equal(t, 0, membership.count())
}

// Test Case ID:      P10-ROUTER-003
// Feature:           Every non-tenant event goes to membership lane
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestP10Router003_MembershipEventsRouteToMembership(t *testing.T) {
	membershipTypes := []string{
		domain.EventTenantRoleGranted,
		domain.EventTenantRoleRevoked,
		domain.EventDepartmentMembershipGranted,
		domain.EventDepartmentMembershipRevoked,
		domain.EventDepartmentMembershipLevelChanged,
		domain.EventMembershipRevoked,
		domain.EventTenderAssigneeOverridden,
		domain.EventMFAReset,
		domain.EventTenantSeatOverageStarted,
		domain.EventTenantSeatOverageResolved,
		domain.EventTenantStateChanged,
	}
	for _, et := range membershipTypes {
		t.Run(et, func(t *testing.T) {
			membership, tenant := &spyPublisher{}, &spyPublisher{}
			rp := NewRoutingPublisher(membership, tenant)
			require.NoError(t, rp.Publish(context.Background(), mkRouterEnv(et)))
			assert.Equal(t, 1, membership.count())
			assert.Equal(t, 0, tenant.count())
		})
	}
}

// Test Case ID:      P10-ROUTER-004
// Feature:           Unknown event type defaults to membership lane
// Scenario:          Forward-compat — new event types don't break routing
// Priority: P2 · Severity: Major · Automation Status: Automated
func TestP10Router004_UnknownEventDefaultsToMembership(t *testing.T) {
	membership, tenant := &spyPublisher{}, &spyPublisher{}
	rp := NewRoutingPublisher(membership, tenant)
	require.NoError(t, rp.Publish(context.Background(), mkRouterEnv("SomeFuturePayload")))
	assert.Equal(t, 1, membership.count(), "unknown event → membership lane (catch-all)")
}

// Test Case ID:      P10-ROUTER-005
// Feature:           Nil membership publisher → descriptive error
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10Router005_NilMembershipPublisherErrors(t *testing.T) {
	rp := NewRoutingPublisher(nil, &spyPublisher{})
	err := rp.Publish(context.Background(), mkRouterEnv(domain.EventTenantRoleGranted))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no publisher wired")
}

// Test Case ID:      P10-ROUTER-006
// Feature:           Nil tenant publisher → descriptive error for tenant events
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10Router006_NilTenantPublisherErrors(t *testing.T) {
	rp := NewRoutingPublisher(&spyPublisher{}, nil)
	err := rp.Publish(context.Background(), mkRouterEnv(domain.EventTenantCreated))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no publisher wired")
}

// Test Case ID:      P10-ROUTER-007
// Feature:           PublishBatch groups by topic before publishing
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10Router007_PublishBatchGroupsByTopic(t *testing.T) {
	membership, tenant := &spyPublisher{}, &spyPublisher{}
	rp := NewRoutingPublisher(membership, tenant)
	envs := []events.Envelope[json.RawMessage]{
		mkRouterEnv(domain.EventTenantCreated),
		mkRouterEnv(domain.EventTenantRoleGranted),
		mkRouterEnv(domain.EventTrialStarted),
		mkRouterEnv(domain.EventMembershipRevoked),
	}
	require.NoError(t, rp.PublishBatch(context.Background(), envs))
	assert.Equal(t, 2, tenant.count(), "TenantCreated + TrialStarted → tenant lane")
	assert.Equal(t, 2, membership.count(), "role + MembershipRevoked → membership lane")
}

// Test Case ID:      P10-ROUTER-008
// Feature:           NoopPublisher swallows envelopes silently
// Priority: P2 · Severity: Minor · Automation Status: Automated
func TestP10Router008_NoopPublisherAlwaysSucceeds(t *testing.T) {
	np := NoopPublisher{}
	require.NoError(t, np.Publish(context.Background(), mkRouterEnv(domain.EventTenantCreated)))
	require.NoError(t, np.PublishBatch(context.Background(), []events.Envelope[json.RawMessage]{
		mkRouterEnv(domain.EventTenantRoleGranted),
	}))
}

// Test Case ID:      P10-ROUTER-009
// Feature:           Publisher error propagates unchanged
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP10Router009_PublisherErrorPropagates(t *testing.T) {
	spy := &spyPublisher{failNext: true}
	rp := NewRoutingPublisher(spy, &spyPublisher{})
	err := rp.Publish(context.Background(), mkRouterEnv(domain.EventTenantRoleGranted))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spy: forced failure")
}
