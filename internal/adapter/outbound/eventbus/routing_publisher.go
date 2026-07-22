package eventbus

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// _ ensures encoding/json is treated as used — the type parameter
// json.RawMessage below references it, but some linters flag it as unused.
var _ = json.RawMessage(nil)

// RoutingPublisher sits at the outbox-runner boundary. It receives every
// envelope pulled from outbox_events and dispatches it to one of two
// destination SNS publishers based on the event type (§7.3):
//   - iam.tenant.events for TenantCreated / TrialStarted
//   - iam.membership.events for everything else
//
// This is O&M's structural departure from the sibling User Profile service,
// which publishes to a single topic. The wrapper is intentionally local:
// platform-events v1.3.0 exposes NewSNSPublisher (single topic) only.
type RoutingPublisher struct {
	membership events.Publisher // iam.membership.events
	tenant     events.Publisher // iam.tenant.events
}

// NewRoutingPublisher constructs a two-topic publisher. Pass nil for either
// argument to disable that lane in dev — an envelope routed to the nil lane
// returns a descriptive error so misconfiguration is caught loudly rather
// than silently swallowed.
func NewRoutingPublisher(membership, tenant events.Publisher) *RoutingPublisher {
	return &RoutingPublisher{membership: membership, tenant: tenant}
}

// Publish dispatches env to the correct topic per domain.TopicForEvent.
func (r *RoutingPublisher) Publish(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	pub, topic := r.pubForType(env.Type)
	if pub == nil {
		return fmt.Errorf("eventbus: no publisher wired for topic %q (event type %q)", topic, env.Type)
	}
	return pub.Publish(ctx, env)
}

// PublishBatch dispatches each envelope independently. Batching across
// topics is not supported by SNS; the outbox runner already publishes at
// bounded concurrency, so a per-item Publish loop preserves ordering per
// topic without adding a second batching layer.
func (r *RoutingPublisher) PublishBatch(ctx context.Context, envs []events.Envelope[json.RawMessage]) error {
	// Group by topic so a batch of same-topic envelopes uses the underlying
	// publisher's batch API (reduces SNS calls).
	membershipBatch := make([]events.Envelope[json.RawMessage], 0, len(envs))
	tenantBatch := make([]events.Envelope[json.RawMessage], 0, len(envs))
	for _, e := range envs {
		switch domain.TopicForEvent(e.Type) {
		case domain.TopicTenant:
			tenantBatch = append(tenantBatch, e)
		default:
			membershipBatch = append(membershipBatch, e)
		}
	}
	if len(membershipBatch) > 0 {
		if r.membership == nil {
			return fmt.Errorf("eventbus: no publisher wired for topic %q", domain.TopicMembership)
		}
		if err := r.membership.PublishBatch(ctx, membershipBatch); err != nil {
			return err
		}
	}
	if len(tenantBatch) > 0 {
		if r.tenant == nil {
			return fmt.Errorf("eventbus: no publisher wired for topic %q", domain.TopicTenant)
		}
		if err := r.tenant.PublishBatch(ctx, tenantBatch); err != nil {
			return err
		}
	}
	return nil
}

func (r *RoutingPublisher) pubForType(eventType string) (events.Publisher, string) {
	topic := domain.TopicForEvent(eventType)
	switch topic {
	case domain.TopicTenant:
		return r.tenant, topic
	default:
		return r.membership, topic
	}
}

// noopPublisher silently discards envelopes. The composition root wires
// this when the corresponding SNS_TOPIC_*_ARN env var is unset (dev/test
// with no broker running). Ensures RoutingPublisher itself never handles a
// nil publisher on the hot path.
type NoopPublisher struct{}

func (NoopPublisher) Publish(_ context.Context, _ events.Envelope[json.RawMessage]) error {
	return nil
}
func (NoopPublisher) PublishBatch(_ context.Context, _ []events.Envelope[json.RawMessage]) error {
	return nil
}
