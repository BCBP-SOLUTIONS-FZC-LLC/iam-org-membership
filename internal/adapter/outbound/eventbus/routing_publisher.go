package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
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

// PublishBatch splits envs by topic and publishes each lane through its
// own publisher's batch API (SNS cannot batch across topics).
//
// Both lanes are always attempted. The outbox runner treats a returned
// *events.BatchError as authoritative per message: every record NOT listed
// in Failures is marked published. Returning early on the first lane's
// error would therefore either mark the unattempted lane published (lost
// events) or, for a plain error, re-send the lane that already went out.
// When both lanes are present, their outcomes are merged into one
// *events.BatchError; a single-lane batch returns that lane's error as is.
func (r *RoutingPublisher) PublishBatch(ctx context.Context, envs []events.Envelope[json.RawMessage]) error {
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
	switch {
	case len(membershipBatch) == 0 && len(tenantBatch) == 0:
		return nil
	case len(tenantBatch) == 0:
		return publishLane(ctx, r.membership, domain.TopicMembership, membershipBatch)
	case len(membershipBatch) == 0:
		return publishLane(ctx, r.tenant, domain.TopicTenant, tenantBatch)
	}

	var failures []events.BatchFailure
	failures = appendLaneFailures(failures, membershipBatch,
		publishLane(ctx, r.membership, domain.TopicMembership, membershipBatch))
	failures = appendLaneFailures(failures, tenantBatch,
		publishLane(ctx, r.tenant, domain.TopicTenant, tenantBatch))
	if len(failures) == 0 {
		return nil
	}
	return &events.BatchError{Failures: failures}
}

func publishLane(ctx context.Context, pub events.Publisher, topic string, batch []events.Envelope[json.RawMessage]) error {
	if pub == nil {
		return fmt.Errorf("eventbus: no publisher wired for topic %q", topic)
	}
	return pub.PublishBatch(ctx, batch)
}

// appendLaneFailures adds one lane's outcome to failures. A *BatchError is
// already per message. Any other error failed the whole lane, so each of its
// envelopes is listed, retryable exactly when the runner would have treated
// the plain error as transient.
func appendLaneFailures(failures []events.BatchFailure, batch []events.Envelope[json.RawMessage], err error) []events.BatchFailure {
	if err == nil {
		return failures
	}
	var batchErr *events.BatchError
	if errors.As(err, &batchErr) {
		return append(failures, batchErr.Failures...)
	}
	retryable := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, events.ErrRetryable)
	for _, e := range batch {
		failures = append(failures, events.BatchFailure{ID: e.ID, Code: "LaneError", Message: err.Error(), Retryable: retryable})
	}
	return failures
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
