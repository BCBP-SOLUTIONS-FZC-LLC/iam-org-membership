package eventbus

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PublishBatch has 4 branches that the existing suite didn't cover:
//   A. membershipBatch non-empty + r.membership == nil → error
//   B. membershipBatch non-empty + r.membership.PublishBatch error → error
//   C. tenantBatch non-empty + r.tenant == nil → error
//   D. tenantBatch non-empty + r.tenant.PublishBatch error → error

func TestPublishBatch_NilMembershipPublisher_ReturnsError(t *testing.T) {
	rp := NewRoutingPublisher(nil, &spyPublisher{})
	err := rp.PublishBatch(context.Background(),
		[]events.Envelope[json.RawMessage]{mkRouterEnv(domain.EventTenantRoleGranted)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no publisher wired")
	assert.Contains(t, err.Error(), domain.TopicMembership)
}

func TestPublishBatch_MembershipPublisherError_Propagates(t *testing.T) {
	spy := &spyPublisher{failNext: true}
	rp := NewRoutingPublisher(spy, &spyPublisher{})
	err := rp.PublishBatch(context.Background(),
		[]events.Envelope[json.RawMessage]{mkRouterEnv(domain.EventMembershipRevoked)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spy: forced failure")
}

func TestPublishBatch_NilTenantPublisher_ReturnsError(t *testing.T) {
	rp := NewRoutingPublisher(&spyPublisher{}, nil)
	err := rp.PublishBatch(context.Background(),
		[]events.Envelope[json.RawMessage]{mkRouterEnv(domain.EventTenantCreated)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no publisher wired")
	assert.Contains(t, err.Error(), domain.TopicTenant)
}

func TestPublishBatch_TenantPublisherError_Propagates(t *testing.T) {
	spy := &spyPublisher{failNext: true}
	rp := NewRoutingPublisher(&spyPublisher{}, spy)
	err := rp.PublishBatch(context.Background(),
		[]events.Envelope[json.RawMessage]{mkRouterEnv(domain.EventTrialStarted)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spy: forced failure")
}

func TestPublishBatch_EmptyBatchIsNoop(t *testing.T) {
	// Both batches empty → no publisher touched, no error.
	rp := NewRoutingPublisher(nil, nil)
	err := rp.PublishBatch(context.Background(), nil)
	assert.NoError(t, err, "empty batch must not require any publisher wired")
}
