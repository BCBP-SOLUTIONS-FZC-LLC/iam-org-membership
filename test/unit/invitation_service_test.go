// Unit tests for internal/core/service/invitation_service.go List (P-30)
// and Revoke (P-31). Invite / AddFromRegister are exercised via postgres
// integration tests — they need TxRunner + RP client wiring.
package unit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── InvitationRepository stub ──────────────────────────────────────────

type fakeInviteRepo struct {
	listFn                func(ctx context.Context, tenantID uuid.UUID) ([]domain.PendingInvitation, error)
	setStatusFn           func(ctx context.Context, tenantID, id uuid.UUID, status domain.InvitationStatus, expectedVersion int64) (*domain.PendingInvitation, error)
	setKCCleanupPendingFn func(ctx context.Context, tenantID, id uuid.UUID, pending bool, expectedVersion int64) error
	countPendingFn        func(ctx context.Context, tenantID uuid.UUID) (int, error)
}

func (f *fakeInviteRepo) List(ctx context.Context, tenantID uuid.UUID) ([]domain.PendingInvitation, error) {
	return f.listFn(ctx, tenantID)
}
func (f *fakeInviteRepo) FindByID(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
	return nil, errors.New("not used")
}
func (f *fakeInviteRepo) FindPendingByEmail(context.Context, uuid.UUID, string) (*domain.PendingInvitation, error) {
	return nil, errors.New("not used")
}
func (f *fakeInviteRepo) FindPendingByKeycloakUser(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
	return nil, errors.New("not used")
}
func (f *fakeInviteRepo) Insert(context.Context, *domain.PendingInvitation) (*domain.PendingInvitation, error) {
	return nil, errors.New("not used")
}
func (f *fakeInviteRepo) SetKeycloakUserID(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int64) error {
	return errors.New("not used")
}
func (f *fakeInviteRepo) SetStatus(ctx context.Context, tenantID, id uuid.UUID, status domain.InvitationStatus, expectedVersion int64) (*domain.PendingInvitation, error) {
	return f.setStatusFn(ctx, tenantID, id, status, expectedVersion)
}
func (f *fakeInviteRepo) SetKCCleanupPending(ctx context.Context, tenantID, id uuid.UUID, pending bool, expectedVersion int64) error {
	return f.setKCCleanupPendingFn(ctx, tenantID, id, pending, expectedVersion)
}
func (f *fakeInviteRepo) CountPending(ctx context.Context, tenantID uuid.UUID) (int, error) {
	if f.countPendingFn == nil {
		return 0, errors.New("not used")
	}
	return f.countPendingFn(ctx, tenantID)
}
func (f *fakeInviteRepo) ListExpiring(context.Context, time.Time, int) ([]domain.PendingInvitation, error) {
	return nil, errors.New("not used")
}
func (f *fakeInviteRepo) ListPendingKCCleanup(context.Context, int) ([]domain.PendingInvitation, error) {
	return nil, errors.New("not used")
}

var _ port.InvitationRepository = (*fakeInviteRepo)(nil)

// buildInvitationSvc wires only the fields the target methods use;
// unused collaborators stay nil.
func buildInvitationSvc(inv port.InvitationRepository, cache port.Cache) *service.InvitationService {
	return service.NewInvitationService(inv, nil, nil, nil, nil, nil, cache, nil, nil, 7)
}

// ── List (P-30) ────────────────────────────────────────────────────────

func TestInvitation_List_DelegatesToRepo(t *testing.T) {
	tenantID := uuid.New()
	want := []domain.PendingInvitation{{ID: uuid.New(), TenantID: tenantID, Email: "a@x.com"}}
	repo := &fakeInviteRepo{
		listFn: func(_ context.Context, tt uuid.UUID) ([]domain.PendingInvitation, error) {
			assert.Equal(t, tenantID, tt)
			return want, nil
		},
	}
	got, err := buildInvitationSvc(repo, nil).List(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestInvitation_List_PropagatesRepoError(t *testing.T) {
	repoErr := errors.New("db down")
	repo := &fakeInviteRepo{
		listFn: func(context.Context, uuid.UUID) ([]domain.PendingInvitation, error) {
			return nil, repoErr
		},
	}
	_, err := buildInvitationSvc(repo, nil).List(context.Background(), uuid.New())
	assert.ErrorIs(t, err, repoErr)
}

// ── Revoke (P-31) ──────────────────────────────────────────────────────

func TestInvitation_Revoke_UpdatesStatusThenFlagsForCleanup(t *testing.T) {
	tenantID, id := uuid.New(), uuid.New()
	statusCalled, cleanupCalled := false, false
	repo := &fakeInviteRepo{
		setStatusFn: func(_ context.Context, tt, ii uuid.UUID, st domain.InvitationStatus, ver int64) (*domain.PendingInvitation, error) {
			statusCalled = true
			assert.Equal(t, tenantID, tt)
			assert.Equal(t, id, ii)
			assert.Equal(t, domain.InviteRevoked, st, "must transition status to 'revoked'")
			assert.EqualValues(t, 5, ver, "expected_version passed through unchanged")
			return &domain.PendingInvitation{ID: ii, RecordVersion: ver + 1}, nil
		},
		setKCCleanupPendingFn: func(_ context.Context, tt, ii uuid.UUID, pending bool, ver int64) error {
			cleanupCalled = true
			assert.Equal(t, tenantID, tt)
			assert.Equal(t, id, ii)
			assert.True(t, pending, "PI-9: revoke must flag the row for the KC cleanup reconciler")
			assert.EqualValues(t, 6, ver, "must pass the bumped record_version from SetStatus")
			return nil
		},
	}
	got, err := buildInvitationSvc(repo, nil).Revoke(context.Background(), tenantID, id, 5)
	require.NoError(t, err)
	assert.True(t, statusCalled)
	assert.True(t, cleanupCalled)
	assert.NotNil(t, got)
}

func TestInvitation_Revoke_SetStatusErrorShortCircuits(t *testing.T) {
	repoErr := errors.New("optimistic lock conflict")
	setKCCalled := false
	repo := &fakeInviteRepo{
		setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.InvitationStatus, int64) (*domain.PendingInvitation, error) {
			return nil, repoErr
		},
		setKCCleanupPendingFn: func(context.Context, uuid.UUID, uuid.UUID, bool, int64) error {
			setKCCalled = true
			return nil
		},
	}
	_, err := buildInvitationSvc(repo, nil).Revoke(context.Background(), uuid.New(), uuid.New(), 1)
	assert.ErrorIs(t, err, repoErr)
	assert.False(t, setKCCalled, "SetKCCleanupPending must not run when SetStatus failed")
}

func TestInvitation_Revoke_SetKCCleanupErrorPropagates(t *testing.T) {
	cleanupErr := errors.New("db down between updates")
	repo := &fakeInviteRepo{
		setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.InvitationStatus, int64) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: uuid.New(), RecordVersion: 2}, nil
		},
		setKCCleanupPendingFn: func(context.Context, uuid.UUID, uuid.UUID, bool, int64) error {
			return cleanupErr
		},
	}
	_, err := buildInvitationSvc(repo, nil).Revoke(context.Background(), uuid.New(), uuid.New(), 1)
	assert.ErrorIs(t, err, cleanupErr)
}

func TestInvitation_Revoke_InvalidatesSeatUsageCacheOnSuccess(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeInviteRepo{
		setStatusFn: func(_ context.Context, _, id uuid.UUID, _ domain.InvitationStatus, _ int64) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: id, RecordVersion: 2}, nil
		},
		setKCCleanupPendingFn: func(context.Context, uuid.UUID, uuid.UUID, bool, int64) error { return nil },
	}
	cache := &spyCache{}
	_, err := buildInvitationSvc(repo, cache).Revoke(context.Background(), tenantID, uuid.New(), 1)
	require.NoError(t, err)
	assert.Contains(t, cache.deleteCalls, "om:seat_usage:"+tenantID.String())
}

func TestInvitation_Revoke_NilCacheIsSafe(t *testing.T) {
	repo := &fakeInviteRepo{
		setStatusFn: func(_ context.Context, _, id uuid.UUID, _ domain.InvitationStatus, _ int64) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: id, RecordVersion: 2}, nil
		},
		setKCCleanupPendingFn: func(context.Context, uuid.UUID, uuid.UUID, bool, int64) error { return nil },
	}
	assert.NotPanics(t, func() {
		_, _ = buildInvitationSvc(repo, nil).Revoke(context.Background(), uuid.New(), uuid.New(), 1)
	})
}
