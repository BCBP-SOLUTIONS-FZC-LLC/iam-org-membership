// Unit tests for internal/core/service/tender_acl_service.go.
// Pure hand-rolled port stubs — no testcontainers, no DB, no HTTP.
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

// ── Stubs ──────────────────────────────────────────────────────────────

type fakeACLRepo struct {
	listByTenderFn      func(ctx context.Context, tenantID, tenderID uuid.UUID) ([]domain.TenderACLEntry, error)
	findActiveForUserFn func(ctx context.Context, tenantID, tenderID, userID uuid.UUID) (*domain.TenderACLEntry, error)
	grantFn             func(ctx context.Context, e *domain.TenderACLEntry) (*domain.TenderACLEntry, error)
	revokeFn            func(ctx context.Context, tenantID, tenderID, userID uuid.UUID) (*domain.TenderACLEntry, error)
	softDeleteForUserFn func(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenderACLEntry, error)
}

func (f *fakeACLRepo) ListByTender(ctx context.Context, tenantID, tenderID uuid.UUID) ([]domain.TenderACLEntry, error) {
	return f.listByTenderFn(ctx, tenantID, tenderID)
}
func (f *fakeACLRepo) FindActiveForUser(ctx context.Context, tenantID, tenderID, userID uuid.UUID) (*domain.TenderACLEntry, error) {
	return f.findActiveForUserFn(ctx, tenantID, tenderID, userID)
}
func (f *fakeACLRepo) Grant(ctx context.Context, e *domain.TenderACLEntry) (*domain.TenderACLEntry, error) {
	return f.grantFn(ctx, e)
}
func (f *fakeACLRepo) Revoke(ctx context.Context, tenantID, tenderID, userID uuid.UUID) (*domain.TenderACLEntry, error) {
	return f.revokeFn(ctx, tenantID, tenderID, userID)
}
func (f *fakeACLRepo) SoftDeleteForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenderACLEntry, error) {
	return f.softDeleteForUserFn(ctx, tenantID, userID)
}

// Compile-time check.
var _ port.TenderACLRepository = (*fakeACLRepo)(nil)

type fakeMembershipRepo struct {
	findByUserIDFn func(ctx context.Context, tenantID, userID uuid.UUID) (*domain.TenantMembership, error)
	setStatusFn    func(ctx context.Context, tenantID, userID uuid.UUID, status domain.MembershipStatus, expectedVersion int64) (*domain.TenantMembership, error)
}

func (f *fakeMembershipRepo) List(ctx context.Context, tenantID uuid.UUID, cursor *domain.MembershipListCursor, limit int) (*domain.MembershipListPage, error) {
	return nil, errors.New("not used")
}
func (f *fakeMembershipRepo) FindByUserID(ctx context.Context, tenantID, userID uuid.UUID) (*domain.TenantMembership, error) {
	return f.findByUserIDFn(ctx, tenantID, userID)
}
func (f *fakeMembershipRepo) Insert(ctx context.Context, tm *domain.TenantMembership) (*domain.TenantMembership, error) {
	return nil, errors.New("not used")
}
func (f *fakeMembershipRepo) SetStatus(ctx context.Context, tenantID, userID uuid.UUID, status domain.MembershipStatus, expectedVersion int64) (*domain.TenantMembership, error) {
	if f.setStatusFn == nil {
		return nil, errors.New("not used")
	}
	return f.setStatusFn(ctx, tenantID, userID, status, expectedVersion)
}
func (f *fakeMembershipRepo) SoftDelete(ctx context.Context, tenantID, userID uuid.UUID, expectedVersion int64) error {
	return errors.New("not used")
}
func (f *fakeMembershipRepo) CountActive(ctx context.Context, tenantID uuid.UUID) (int, error) {
	return 0, errors.New("not used")
}

var _ port.MembershipRepository = (*fakeMembershipRepo)(nil)

// ── List (P-21) ────────────────────────────────────────────────────────

func TestTenderACLService_List_DelegatesToRepo(t *testing.T) {
	tenantID, tenderID := uuid.New(), uuid.New()
	want := []domain.TenderACLEntry{{ID: uuid.New(), TenantID: tenantID, TenderID: tenderID}}
	acl := &fakeACLRepo{
		listByTenderFn: func(_ context.Context, tt, td uuid.UUID) ([]domain.TenderACLEntry, error) {
			assert.Equal(t, tenantID, tt)
			assert.Equal(t, tenderID, td)
			return want, nil
		},
	}
	svc := service.NewTenderACLService(acl, &fakeMembershipRepo{})

	got, err := svc.List(context.Background(), tenantID, tenderID)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestTenderACLService_List_PropagatesRepoError(t *testing.T) {
	repoErr := errors.New("db down")
	acl := &fakeACLRepo{
		listByTenderFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenderACLEntry, error) {
			return nil, repoErr
		},
	}
	svc := service.NewTenderACLService(acl, &fakeMembershipRepo{})

	_, err := svc.List(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, repoErr)
}

// ── Grant (P-22) — level validation ────────────────────────────────────

func TestTenderACLService_Grant_RejectsInvalidLevel(t *testing.T) {
	svc := service.NewTenderACLService(&fakeACLRepo{}, &fakeMembershipRepo{})

	_, err := svc.Grant(context.Background(), uuid.New(), uuid.New(), uuid.New(),
		domain.TenderACLLevel("wizard"), uuid.New(), "reason", nil)

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
	assert.Equal(t, "invalid_access_level", de.Details["code"])
}

func TestTenderACLService_Grant_AcceptsAllValidLevels(t *testing.T) {
	for _, lvl := range []domain.TenderACLLevel{domain.ACLView, domain.ACLEdit, domain.ACLApprove} {
		t.Run(string(lvl), func(t *testing.T) {
			tenantID, tenderID, userID := uuid.New(), uuid.New(), uuid.New()
			mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID}
			acl := &fakeACLRepo{
				grantFn: func(_ context.Context, e *domain.TenderACLEntry) (*domain.TenderACLEntry, error) {
					assert.Equal(t, lvl, e.AccessLevel)
					assert.Equal(t, mem.ID, e.TenantMembershipID, "TAE-8 composite FK anchor")
					e.ID = uuid.New()
					return e, nil
				},
			}
			mr := &fakeMembershipRepo{
				findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
					assert.Equal(t, tenantID, tt)
					assert.Equal(t, userID, uu)
					return mem, nil
				},
			}
			svc := service.NewTenderACLService(acl, mr)

			got, err := svc.Grant(context.Background(), tenantID, tenderID, userID, lvl,
				uuid.New(), "reason", nil)
			require.NoError(t, err)
			assert.Equal(t, lvl, got.AccessLevel)
		})
	}
}

// ── Grant — expires_at validation (TAE-3 boundary) ─────────────────────

func TestTenderACLService_Grant_RejectsPastExpiry(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	svc := service.NewTenderACLService(&fakeACLRepo{}, &fakeMembershipRepo{})

	_, err := svc.Grant(context.Background(), uuid.New(), uuid.New(), uuid.New(),
		domain.ACLView, uuid.New(), "", &past)

	assert.ErrorIs(t, err, domain.ErrInvalidExpiresAt)
}

func TestTenderACLService_Grant_AcceptsFutureExpiry(t *testing.T) {
	future := time.Now().Add(time.Hour)
	tenantID, userID := uuid.New(), uuid.New()
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID}
	acl := &fakeACLRepo{
		grantFn: func(_ context.Context, e *domain.TenderACLEntry) (*domain.TenderACLEntry, error) {
			assert.NotNil(t, e.ExpiresAt)
			assert.Equal(t, future, *e.ExpiresAt)
			e.ID = uuid.New()
			return e, nil
		},
	}
	mr := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return mem, nil
		},
	}
	svc := service.NewTenderACLService(acl, mr)

	_, err := svc.Grant(context.Background(), tenantID, uuid.New(), userID,
		domain.ACLView, uuid.New(), "", &future)
	require.NoError(t, err)
}

// ── Grant — membership lookup failure surfaces ─────────────────────────

func TestTenderACLService_Grant_MembershipNotFound(t *testing.T) {
	mr := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return nil, domain.NewError(domain.ErrMemberNotFound, "no membership")
		},
	}
	svc := service.NewTenderACLService(&fakeACLRepo{}, mr)

	_, err := svc.Grant(context.Background(), uuid.New(), uuid.New(), uuid.New(),
		domain.ACLView, uuid.New(), "", nil)
	assert.ErrorIs(t, err, domain.ErrMemberNotFound)
}

// ── Grant — repo failure propagates ────────────────────────────────────

func TestTenderACLService_Grant_RepoFailurePropagates(t *testing.T) {
	repoErr := errors.New("insert conflict")
	mr := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New()}, nil
		},
	}
	acl := &fakeACLRepo{
		grantFn: func(context.Context, *domain.TenderACLEntry) (*domain.TenderACLEntry, error) {
			return nil, repoErr
		},
	}
	svc := service.NewTenderACLService(acl, mr)

	_, err := svc.Grant(context.Background(), uuid.New(), uuid.New(), uuid.New(),
		domain.ACLView, uuid.New(), "", nil)
	assert.ErrorIs(t, err, repoErr)
}

// ── Revoke (P-23) ──────────────────────────────────────────────────────

func TestTenderACLService_Revoke_DelegatesToRepo(t *testing.T) {
	tenantID, tenderID, userID := uuid.New(), uuid.New(), uuid.New()
	want := &domain.TenderACLEntry{ID: uuid.New(), TenantID: tenantID}
	acl := &fakeACLRepo{
		revokeFn: func(_ context.Context, tt, td, uu uuid.UUID) (*domain.TenderACLEntry, error) {
			assert.Equal(t, tenantID, tt)
			assert.Equal(t, tenderID, td)
			assert.Equal(t, userID, uu)
			return want, nil
		},
	}
	svc := service.NewTenderACLService(acl, &fakeMembershipRepo{})

	got, err := svc.Revoke(context.Background(), tenantID, tenderID, userID)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// ── CheckAccess (I-12) ─────────────────────────────────────────────────

func TestTenderACLService_CheckAccess_ReturnsActiveEntry(t *testing.T) {
	tenantID, tenderID, userID := uuid.New(), uuid.New(), uuid.New()
	want := &domain.TenderACLEntry{ID: uuid.New(), AccessLevel: domain.ACLApprove}
	acl := &fakeACLRepo{
		findActiveForUserFn: func(_ context.Context, tt, td, uu uuid.UUID) (*domain.TenderACLEntry, error) {
			assert.Equal(t, tenantID, tt)
			assert.Equal(t, tenderID, td)
			assert.Equal(t, userID, uu)
			return want, nil
		},
	}
	svc := service.NewTenderACLService(acl, &fakeMembershipRepo{})

	got, err := svc.CheckAccess(context.Background(), tenantID, tenderID, userID)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestTenderACLService_CheckAccess_NoGrantReturnsNilNoError(t *testing.T) {
	// FindActiveForUser returns (nil, nil) when no active grant exists.
	acl := &fakeACLRepo{
		findActiveForUserFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.TenderACLEntry, error) {
			return nil, nil
		},
	}
	svc := service.NewTenderACLService(acl, &fakeMembershipRepo{})

	got, err := svc.CheckAccess(context.Background(), uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Nil(t, got)
}
