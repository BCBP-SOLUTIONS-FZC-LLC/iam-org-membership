// Extended ACL service tests: P21/P22/P23 scenarios not yet covered.
package unit_test

import (
	"context"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── P21 List ───────────────────────────────────────────────────────────

func TestACLService_List_HappyPath_ReturnsItems(t *testing.T) {
	tenantID, tenderID := uuid.New(), uuid.New()
	want := []domain.TenderACLEntry{
		{ID: uuid.New(), TenantID: tenantID, TenderID: tenderID, AccessLevel: domain.ACLView},
		{ID: uuid.New(), TenantID: tenantID, TenderID: tenderID, AccessLevel: domain.ACLEdit},
	}
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
	assert.Len(t, got, 2)
}

func TestACLService_List_EmptyTender_ReturnsEmptySlice(t *testing.T) {
	acl := &fakeACLRepo{
		listByTenderFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenderACLEntry, error) {
			return []domain.TenderACLEntry{}, nil
		},
	}
	svc := service.NewTenderACLService(acl, &fakeMembershipRepo{})
	got, err := svc.List(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestACLService_List_IncludesExpiredEntries_TAE7(t *testing.T) {
	// TAE-7 fix: ListByTender now returns active + passively expired (deleted_at IS NULL).
	// Service.List delegates straight to repo — the filter change is at the repo level.
	past := time.Now().Add(-24 * time.Hour)
	entries := []domain.TenderACLEntry{
		{ID: uuid.New(), AccessLevel: domain.ACLView},                      // active
		{ID: uuid.New(), AccessLevel: domain.ACLApprove, ExpiresAt: &past}, // expired
	}
	acl := &fakeACLRepo{
		listByTenderFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenderACLEntry, error) {
			return entries, nil
		},
	}
	svc := service.NewTenderACLService(acl, &fakeMembershipRepo{})
	got, err := svc.List(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Len(t, got, 2, "list must include expired entries (TAE-7)")
}

func TestACLService_List_RevokedEntriesAbsent_TAE4(t *testing.T) {
	// TAE-4: soft-deleted (deleted_at IS NOT NULL) entries not in list.
	// The repo filters them — service passes through what repo returns.
	acl := &fakeACLRepo{
		listByTenderFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenderACLEntry, error) {
			// Simulate repo already filtering revoked entries
			return []domain.TenderACLEntry{{ID: uuid.New(), AccessLevel: domain.ACLView}}, nil
		},
	}
	svc := service.NewTenderACLService(acl, &fakeMembershipRepo{})
	got, err := svc.List(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

// ── P22 extended ──────────────────────────────────────────────────────

func TestACLService_Grant_SelfGrant_Allowed(t *testing.T) {
	// P22-SELF-01: caller grants ACL to themselves — no restriction in LLD
	tenantID := uuid.New()
	callerID := uuid.New()
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: callerID, Status: domain.MembershipActive}
	acl := &fakeACLRepo{
		grantFn: func(_ context.Context, e *domain.TenderACLEntry) (*domain.TenderACLEntry, error) {
			assert.Equal(t, callerID, e.UserID)
			assert.Equal(t, callerID, e.GrantedBy)
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
	got, err := svc.Grant(context.Background(), tenantID, uuid.New(), callerID,
		domain.ACLView, callerID, "", nil)
	require.NoError(t, err)
	assert.Equal(t, callerID, got.UserID)
}

func TestACLService_Grant_LeftMember_Returns404(t *testing.T) {
	// P22-LEFT-MEMBER-01: member who left → deleted_at IS NOT NULL → ErrMemberNotFound
	mr := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return nil, domain.NewError(domain.ErrMemberNotFound, "member not found")
		},
	}
	svc := service.NewTenderACLService(&fakeACLRepo{}, mr)
	_, err := svc.Grant(context.Background(), uuid.New(), uuid.New(), uuid.New(),
		domain.ACLView, uuid.New(), "", nil)
	assert.ErrorIs(t, err, domain.ErrMemberNotFound)
}

func TestACLService_Grant_RegrantAfterRevoke_Succeeds(t *testing.T) {
	// P22-REGRANT-01: after revoke (deleted_at set), new INSERT succeeds (TAE-2)
	tenantID, userID := uuid.New(), uuid.New()
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID, Status: domain.MembershipActive}
	grantCalled := false
	acl := &fakeACLRepo{
		grantFn: func(_ context.Context, e *domain.TenderACLEntry) (*domain.TenderACLEntry, error) {
			grantCalled = true
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
		domain.ACLEdit, uuid.New(), "", nil)
	require.NoError(t, err)
	assert.True(t, grantCalled)
}

func TestACLService_Grant_ExpiresAtNow_Returns422(t *testing.T) {
	// P22-VAL-EXPIRY-NOW-01: expires_at = now() → !After(now) → 422
	now := time.Now().UTC()
	svc := service.NewTenderACLService(&fakeACLRepo{}, &fakeMembershipRepo{})
	_, err := svc.Grant(context.Background(), uuid.New(), uuid.New(), uuid.New(),
		domain.ACLView, uuid.New(), "", &now)
	assert.ErrorIs(t, err, domain.ErrInvalidExpiresAt)
}

func TestACLService_Grant_UppercaseLevel_Returns400(t *testing.T) {
	// P22-VAL-CASE-01: "VIEW" (uppercase) not in enum → 400 invalid_access_level
	svc := service.NewTenderACLService(&fakeACLRepo{}, &fakeMembershipRepo{})
	_, err := svc.Grant(context.Background(), uuid.New(), uuid.New(), uuid.New(),
		domain.TenderACLLevel("VIEW"), uuid.New(), "", nil)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "invalid_access_level", de.Details["code"])
}

func TestACLService_Grant_CrossTenantUser_Returns404(t *testing.T) {
	// P22-CROSS-USER-01: user from different tenant → FindByUserID returns not found
	mr := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return nil, domain.NewError(domain.ErrMemberNotFound, "member not found")
		},
	}
	svc := service.NewTenderACLService(&fakeACLRepo{}, mr)
	_, err := svc.Grant(context.Background(), uuid.New(), uuid.New(), uuid.New(),
		domain.ACLView, uuid.New(), "", nil)
	assert.ErrorIs(t, err, domain.ErrMemberNotFound)
}

// ── P23 extended ──────────────────────────────────────────────────────

func TestACLService_Revoke_ThenRegrant_Succeeds(t *testing.T) {
	// P23-REGRANT-01: revoke removes active grant, re-grant creates new row
	tenantID, tenderID, userID := uuid.New(), uuid.New(), uuid.New()
	revoked := false
	acl := &fakeACLRepo{
		revokeFn: func(_ context.Context, tt, td, uu uuid.UUID) (*domain.TenderACLEntry, error) {
			revoked = true
			return &domain.TenderACLEntry{ID: uuid.New()}, nil
		},
		grantFn: func(_ context.Context, e *domain.TenderACLEntry) (*domain.TenderACLEntry, error) {
			assert.True(t, revoked, "must revoke before re-granting")
			e.ID = uuid.New()
			return e, nil
		},
	}
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID, Status: domain.MembershipActive}
	mr := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return mem, nil
		},
	}
	svc := service.NewTenderACLService(acl, mr)

	_, err := svc.Revoke(context.Background(), tenantID, tenderID, userID)
	require.NoError(t, err)

	_, err = svc.Grant(context.Background(), tenantID, tenderID, userID,
		domain.ACLApprove, uuid.New(), "", nil)
	require.NoError(t, err)
}

func TestACLService_Revoke_ExpiredEntry_Succeeds(t *testing.T) {
	// P23-REVOKE-EXPIRED-01: passively expired entry has deleted_at IS NULL
	// → UPDATE WHERE deleted_at IS NULL succeeds → 200
	acl := &fakeACLRepo{
		revokeFn: func(_ context.Context, _, _, _ uuid.UUID) (*domain.TenderACLEntry, error) {
			return &domain.TenderACLEntry{ID: uuid.New()}, nil
		},
	}
	svc := service.NewTenderACLService(acl, &fakeMembershipRepo{})
	got, err := svc.Revoke(context.Background(), uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.NotNil(t, got)
}

// ── I-12 CheckAccess ──────────────────────────────────────────────────

func TestACLService_CheckAccess_ActiveGrant_ReturnsEntry(t *testing.T) {
	// I12-HAPPY-01: active grant → returns entry (has_access:true at handler level)
	want := &domain.TenderACLEntry{ID: uuid.New(), AccessLevel: domain.ACLApprove}
	acl := &fakeACLRepo{
		findActiveForUserFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.TenderACLEntry, error) {
			return want, nil
		},
	}
	svc := service.NewTenderACLService(acl, &fakeMembershipRepo{})
	got, err := svc.CheckAccess(context.Background(), uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Equal(t, want.ID, got.ID)
}

func TestACLService_CheckAccess_ExpiredGrant_ReturnsNil(t *testing.T) {
	// I12-EXPIRED-01: TAE-3 filter in FindActiveForUser excludes expired → nil
	acl := &fakeACLRepo{
		findActiveForUserFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.TenderACLEntry, error) {
			return nil, nil // expired entry filtered by repo
		},
	}
	svc := service.NewTenderACLService(acl, &fakeMembershipRepo{})
	got, err := svc.CheckAccess(context.Background(), uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Nil(t, got, "expired grant must not authorize (TAE-3)")
}
