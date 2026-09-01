// Unit tests for internal/core/service/membership_service.go ResetUserMFA
// (P-34, §16 OQ-8/F6). Covers:
//   - target not found → 404 member_not_found
//   - target not active → 422 member_not_active
//   - RP-9 failure is fail-closed: 503 realm_provisioner_unavailable, no
//     MFAReset event emitted
//   - happy path: RP-9 called once, MFAReset emitted with the right payload
package unit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildMembershipSvcForResetMFA(m *fakeMembershipRepo, rp port.RealmProvisionerClient, pub *ruPublisher) *service.MembershipService {
	txRunner := &ruTxRunner{tx: &ruFakeTx{}, pub: pub}
	return service.NewMembershipService(m, nil, nil, nil, nil, nil, rp, nil, txRunner, nil, 30)
}

func TestMembership_ResetUserMFA_MemberNotFound(t *testing.T) {
	notFoundErr := domain.NewError(domain.ErrMemberNotFound, "member not found")
	m := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return nil, notFoundErr
		},
	}
	rp := &fakeRPClient{}
	svc := buildMembershipSvcForResetMFA(m, rp, &ruPublisher{})

	err := svc.ResetUserMFA(context.Background(), uuid.New(), uuid.New(), uuid.New())
	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrMemberNotFound, de.Cause)
	assert.False(t, rp.resetMFACalled, "RP must not be called when the target isn't found")
}

func TestMembership_ResetUserMFA_MemberNotActive(t *testing.T) {
	m := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: domain.MembershipSuspended, RecordVersion: 1}, nil
		},
	}
	rp := &fakeRPClient{}
	svc := buildMembershipSvcForResetMFA(m, rp, &ruPublisher{})

	err := svc.ResetUserMFA(context.Background(), uuid.New(), uuid.New(), uuid.New())
	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrMemberNotActive, de.Cause)
	assert.False(t, rp.resetMFACalled, "RP must not be called when the target isn't active")
}

func TestMembership_ResetUserMFA_RPFailure_FailClosedNoEvent(t *testing.T) {
	m := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: domain.MembershipActive, RecordVersion: 1}, nil
		},
	}
	rp := &fakeRPClient{
		resetMFAFn: func(context.Context, uuid.UUID, uuid.UUID) error {
			return errors.New("rp-9 outage")
		},
	}
	pub := &ruPublisher{}
	svc := buildMembershipSvcForResetMFA(m, rp, pub)

	err := svc.ResetUserMFA(context.Background(), uuid.New(), uuid.New(), uuid.New())
	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrRealmProvisionerUnavailable, de.Cause)
	assert.True(t, rp.resetMFACalled)
	assert.Empty(t, pub.events, "fail-closed: no MFAReset may be emitted when RP-9 fails")
}

func TestMembership_ResetUserMFA_HappyPathCallsRPAndEmitsEvent(t *testing.T) {
	tenantID, userID, actorID := uuid.New(), uuid.New(), uuid.New()
	m := &fakeMembershipRepo{
		findByUserIDFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
			assert.Equal(t, tenantID, tid)
			assert.Equal(t, userID, uid)
			return &domain.TenantMembership{Status: domain.MembershipActive, RecordVersion: 1}, nil
		},
	}
	rp := &fakeRPClient{}
	pub := &ruPublisher{}
	svc := buildMembershipSvcForResetMFA(m, rp, pub)

	err := svc.ResetUserMFA(context.Background(), tenantID, userID, actorID)
	require.NoError(t, err)
	assert.True(t, rp.resetMFACalled, "RP-9 must be called on the happy path")

	require.Len(t, pub.events, 1, "exactly one MFAReset must be emitted")
	evt := pub.events[0]
	assert.Equal(t, domain.EventMFAReset, evt.Type)
	assert.Equal(t, tenantID, evt.TenantID)
	assert.Equal(t, userID.String(), evt.Subject)
	assert.Equal(t, actorID.String(), evt.Actor)
	payload, ok := evt.Data.(domain.MFAResetPayload)
	require.True(t, ok, "event data must be domain.MFAResetPayload")
	assert.Equal(t, tenantID, payload.TenantID)
	assert.Equal(t, userID, payload.UserID)
	assert.Equal(t, actorID, payload.ActorID)
}

func TestMembership_ResetUserMFA_NilRealmProvisionerUnavailable(t *testing.T) {
	m := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: domain.MembershipActive, RecordVersion: 1}, nil
		},
	}
	svc := buildMembershipSvcForResetMFA(m, nil, &ruPublisher{})

	err := svc.ResetUserMFA(context.Background(), uuid.New(), uuid.New(), uuid.New())
	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrRealmProvisionerUnavailable, de.Cause)
}
