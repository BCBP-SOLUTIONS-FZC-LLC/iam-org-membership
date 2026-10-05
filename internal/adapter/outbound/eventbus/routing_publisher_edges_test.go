package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
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

// laneErrPublisher returns a fixed error from PublishBatch and records what
// it was asked to send.
type laneErrPublisher struct {
	spyPublisher
	err error
}

func (p *laneErrPublisher) PublishBatch(ctx context.Context, envs []events.Envelope[json.RawMessage]) error {
	_ = p.spyPublisher.PublishBatch(ctx, envs)
	return p.err
}

func mixedBatch() (membership, tenant events.Envelope[json.RawMessage], all []events.Envelope[json.RawMessage]) {
	membership = mkRouterEnv(domain.EventMembershipRevoked)
	membership.ID = "m-1"
	tenant = mkRouterEnv(domain.EventTenantCreated)
	tenant.ID = "t-1"
	return membership, tenant, []events.Envelope[json.RawMessage]{membership, tenant}
}

// A partial failure on the membership lane must not stop the tenant lane:
// the runner marks every record missing from Failures as published, so an
// early return would silently lose the tenant event.
func TestPublishBatch_MixedBatch_MembershipPartialFailure_StillPublishesTenant(t *testing.T) {
	m, ten, all := mixedBatch()
	mem := &laneErrPublisher{err: &events.BatchError{Failures: []events.BatchFailure{{ID: m.ID, Code: "InternalError", Message: "boom"}}}}
	tenant := &spyPublisher{}
	err := NewRoutingPublisher(mem, tenant).PublishBatch(context.Background(), all)

	var be *events.BatchError
	require.ErrorAs(t, err, &be)
	require.Len(t, be.Failures, 1)
	assert.Equal(t, m.ID, be.Failures[0].ID)
	require.Len(t, tenant.got, 1, "tenant lane must still be attempted")
	assert.Equal(t, ten.ID, tenant.got[0].ID)
}

// A plain error on one lane is reported per message for that lane only, so
// the lane that went out is not re-sent.
func TestPublishBatch_MixedBatch_PlainLaneError_ListsOnlyThatLane(t *testing.T) {
	m, ten, all := mixedBatch()
	mem := &spyPublisher{}
	tenant := &laneErrPublisher{err: errors.New("encode failed")}
	err := NewRoutingPublisher(mem, tenant).PublishBatch(context.Background(), all)

	var be *events.BatchError
	require.ErrorAs(t, err, &be)
	require.Len(t, be.Failures, 1)
	assert.Equal(t, ten.ID, be.Failures[0].ID)
	assert.False(t, be.Failures[0].Retryable, "a non-transient lane error counts an attempt")
	require.Len(t, mem.got, 1)
	assert.Equal(t, m.ID, mem.got[0].ID)
}

func TestPublishBatch_MixedBatch_TransientLaneError_IsRetryable(t *testing.T) {
	_, ten, all := mixedBatch()
	tenant := &laneErrPublisher{err: fmt.Errorf("sns throttled: %w", events.ErrRetryable)}
	err := NewRoutingPublisher(&spyPublisher{}, tenant).PublishBatch(context.Background(), all)

	var be *events.BatchError
	require.ErrorAs(t, err, &be)
	require.Len(t, be.Failures, 1)
	assert.Equal(t, ten.ID, be.Failures[0].ID)
	assert.True(t, be.Failures[0].Retryable)
}

func TestPublishBatch_MixedBatch_NilLane_ListsThatLane(t *testing.T) {
	m, _, all := mixedBatch()
	tenant := &spyPublisher{}
	err := NewRoutingPublisher(nil, tenant).PublishBatch(context.Background(), all)

	var be *events.BatchError
	require.ErrorAs(t, err, &be)
	require.Len(t, be.Failures, 1)
	assert.Equal(t, m.ID, be.Failures[0].ID)
	assert.Contains(t, be.Failures[0].Message, "no publisher wired")
	assert.Len(t, tenant.got, 1)
}

func TestPublishBatch_MixedBatch_BothSucceed(t *testing.T) {
	_, _, all := mixedBatch()
	mem, tenant := &spyPublisher{}, &spyPublisher{}
	require.NoError(t, NewRoutingPublisher(mem, tenant).PublishBatch(context.Background(), all))
	assert.Len(t, mem.got, 1)
	assert.Len(t, tenant.got, 1)
}
