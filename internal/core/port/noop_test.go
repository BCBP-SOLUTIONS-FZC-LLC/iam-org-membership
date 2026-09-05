package port

import (
	"context"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// TestTenantRepositoryNoop_AllMethods exercises every TenantRepositoryNoop method
// so the coverage tool counts each one-liner body as covered.
func TestTenantRepositoryNoop_AllMethods(t *testing.T) {
	n := TenantRepositoryNoop{}
	ctx := context.Background()
	id := uuid.New()

	v, err := n.FindByID(ctx, id)
	assert.Nil(t, v)
	assert.NoError(t, err)

	v, err = n.FindByIDIncludingDeleted(ctx, id)
	assert.Nil(t, v)
	assert.NoError(t, err)

	v2, err := n.Update(ctx, id, &domain.TenantPatch{})
	assert.Nil(t, v2)
	assert.NoError(t, err)

	assert.NoError(t, n.SetRealmSyncPending(ctx, id))

	v3, created, err := n.Insert(ctx, &domain.Tenant{})
	assert.Nil(t, v3)
	assert.False(t, created)
	assert.NoError(t, err)

	v4, err := n.ListSubscriptionLapses(ctx, 30)
	assert.Nil(t, v4)
	assert.NoError(t, err)

	assert.NoError(t, n.LockByID(ctx, id))

	seats, err := n.LicensedSeatsForUpdate(ctx, id)
	assert.Equal(t, 0, seats)
	assert.NoError(t, err)

	assert.NoError(t, n.SetFeatureFlags(ctx, id, nil, 0))
	assert.NoError(t, n.ClearOwnerlessSince(ctx, id))

	set, err := n.MarkOwnerlessIfUnset(ctx, id)
	assert.False(t, set)
	assert.NoError(t, err)

	assert.NoError(t, n.SetRealmFields(ctx, id, "", domain.RealmType(""), "", 0))

	occ, err := n.LockSeatOccupancy(ctx, id)
	assert.Equal(t, SeatOccupancy{}, occ)
	assert.NoError(t, err)

	assert.NoError(t, n.SetOverageSince(ctx, id, nil))

	lock, err := n.LockForProjection(ctx, id)
	assert.Nil(t, lock)
	assert.NoError(t, err)

	assert.NoError(t, n.SetLastEventAt(ctx, id, time.Time{}))

	rows, err := n.ApplyLifecyclePatch(ctx, id, TenantLifecyclePatch{})
	assert.Equal(t, int64(0), rows)
	assert.NoError(t, err)

	assert.NoError(t, n.WipeTenantChildren(ctx, id))
}

// TestInvitationRepositoryNoop_AllMethods exercises every InvitationRepositoryNoop method.
func TestInvitationRepositoryNoop_AllMethods(t *testing.T) {
	n := InvitationRepositoryNoop{}
	ctx := context.Background()

	v, err := n.LockByID(ctx, uuid.New())
	assert.Nil(t, v)
	assert.NoError(t, err)

	count, err := n.ExpireOverdue(ctx, 100)
	assert.Equal(t, 0, count)
	assert.NoError(t, err)

	assert.NoError(t, n.ClearKCCleanupPendingByID(ctx, uuid.New()))
}

// TestEventPublisher_ContextRoundTrip verifies WithEventPublisher stores and
// EventPublisherFromContext retrieves the publisher.
func TestEventPublisher_ContextRoundTrip(t *testing.T) {
	pub := &noopPublisher{}
	ctx := WithEventPublisher(context.Background(), pub)
	got, ok := EventPublisherFromContext(ctx)
	assert.True(t, ok)
	assert.Equal(t, pub, got)
}

// TestEventPublisherFromContext_MissingReturnsFalse verifies the zero-value path.
func TestEventPublisherFromContext_MissingReturnsFalse(t *testing.T) {
	_, ok := EventPublisherFromContext(context.Background())
	assert.False(t, ok)
}

type noopPublisher struct{}

func (p *noopPublisher) Enqueue(context.Context, *domain.DomainEvent) error { return nil }

var _ EventPublisher = (*noopPublisher)(nil)
