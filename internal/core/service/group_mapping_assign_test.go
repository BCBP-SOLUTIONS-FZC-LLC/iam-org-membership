package service

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Unit tests for AssignFromGroups (I-10, §8.5), entirely untested in the
// unit suite before this file — resolveMappings/getCachedResolution/
// setCachedResolution already have thorough coverage in
// group_mapping_resolve_test.go, but the JIT-apply half of the method
// (dept/role resolution, membership lookup, the RunInTx grant/assign
// loop, and its event-classification branches) had none.

// ── fakes local to this file ────────────────────────────────────────────

type gmaMembershipRepo struct {
	findByUserIDFn func(ctx context.Context, tenantID, userID uuid.UUID) (*domain.TenantMembership, error)
}

func (f *gmaMembershipRepo) List(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
	return nil, nil
}
func (f *gmaMembershipRepo) FindByUserID(ctx context.Context, tenantID, userID uuid.UUID) (*domain.TenantMembership, error) {
	return f.findByUserIDFn(ctx, tenantID, userID)
}
func (f *gmaMembershipRepo) Insert(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
	return nil, nil
}
func (f *gmaMembershipRepo) SetStatus(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
	return nil, nil
}
func (f *gmaMembershipRepo) SoftDelete(context.Context, uuid.UUID, uuid.UUID, int64) error {
	return nil
}
func (f *gmaMembershipRepo) CountActive(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (f *gmaMembershipRepo) ListActiveUserIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

var _ port.MembershipRepository = (*gmaMembershipRepo)(nil)

type gmaRoleRepo struct {
	listByUserFn func(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenantRole, error)
	grantFn      func(ctx context.Context, tr *domain.TenantRole) (*domain.TenantRole, error)
}

func (f *gmaRoleRepo) ListByUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenantRole, error) {
	if f.listByUserFn != nil {
		return f.listByUserFn(ctx, tenantID, userID)
	}
	return nil, nil
}
func (f *gmaRoleRepo) ListByRole(context.Context, uuid.UUID, domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (f *gmaRoleRepo) CountActiveOwners(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (f *gmaRoleRepo) Grant(ctx context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
	if f.grantFn != nil {
		return f.grantFn(ctx, tr)
	}
	return tr, nil
}
func (f *gmaRoleRepo) Revoke(context.Context, uuid.UUID, uuid.UUID, domain.TenantRoleCode) (*domain.TenantRole, error) {
	return nil, nil
}
func (f *gmaRoleRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}

var _ port.TenantRoleRepository = (*gmaRoleRepo)(nil)

type gmaDeptMemRepo struct {
	assignFn func(ctx context.Context, tenantID, userID, deptID, memID uuid.UUID, level domain.DeptRole, actorID uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error)
}

func (f *gmaDeptMemRepo) ListByUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (f *gmaDeptMemRepo) ListByDepartment(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (f *gmaDeptMemRepo) Assign(ctx context.Context, tenantID, userID, deptID, memID uuid.UUID, level domain.DeptRole, actorID uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
	if f.assignFn != nil {
		return f.assignFn(ctx, tenantID, userID, deptID, memID, level, actorID)
	}
	return &domain.DeptMembership{TenantID: tenantID, UserID: userID, DepartmentID: deptID, RoleLevel: level}, nil, nil
}
func (f *gmaDeptMemRepo) Remove(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.DeptMembership, error) {
	return nil, nil
}
func (f *gmaDeptMemRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (f *gmaDeptMemRepo) SoftDeleteAllForDept(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}

var _ port.DeptMembershipRepository = (*gmaDeptMemRepo)(nil)

type gmaPub struct{ events []*domain.DomainEvent }

func (p *gmaPub) Enqueue(_ context.Context, e *domain.DomainEvent) error {
	p.events = append(p.events, e)
	return nil
}

type gmaTxRunner struct {
	pub    port.EventPublisher
	runErr error
}

func (r *gmaTxRunner) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	if r.runErr != nil {
		return r.runErr
	}
	if r.pub != nil {
		ctx = port.WithEventPublisher(ctx, r.pub)
	}
	return fn(ctx)
}

// buildAssignFromGroupsSvc wires a GroupMappingService with a pre-warmed
// cache so resolveMappings never needs a real GroupMappingClient — this
// file is only exercising AssignFromGroups' own logic, not resolution
// (already covered by group_mapping_resolve_test.go).
func buildAssignFromGroupsSvc(t *testing.T, tenantID uuid.UUID, dm []domain.GroupDeptMapping, dr []domain.GroupDeptRoleMapping, tr []domain.GroupTenantRoleMapping,
	mem port.MembershipRepository, roles port.TenantRoleRepository, deptMems port.DeptMembershipRepository, txRunner port.TxRunner, cache port.Cache,
) *GroupMappingService {
	t.Helper()
	// The resolution cache doubles as the pre-warm mechanism below, so a
	// nil `cache` argument (meaning "the caller doesn't care") still needs
	// a real backing store — only an explicitly-provided cache is used
	// as-is (e.g. the invalidation test, which asserts against it).
	if cache == nil {
		cache = newGMCache()
	}
	svc := NewGroupMappingService(mem, roles, deptMems, txRunner, cache, nil)
	// Pre-populate the resolution cache directly so resolveMappings hits
	// the warm-cache path deterministically (client stays nil/unused).
	svc.setCachedResolution(context.Background(), tenantID, dm, dr, tr)
	return svc
}

// ── AssignFromGroups — empty group set short-circuits ──────────────────

func TestAssignFromGroups_EmptyGroupNames_ReturnsEmptyResult(t *testing.T) {
	svc := NewGroupMappingService(nil, nil, nil, nil, nil, nil)
	got, err := svc.AssignFromGroups(context.Background(), uuid.New(), uuid.New(), nil)
	require.NoError(t, err)
	assert.Empty(t, got.AssignedDepts)
	assert.Empty(t, got.GrantedTenantRoles)
}

// ── AssignFromGroups — membership lookup error propagates ──────────────

func TestAssignFromGroups_MembershipLookupErrorPropagates(t *testing.T) {
	tenantID := uuid.New()
	memErr := errors.New("membership not found")
	mem := &gmaMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return nil, memErr
	}}
	svc := buildAssignFromGroupsSvc(t, tenantID, nil, nil, nil, mem, nil, nil, nil, nil)

	_, err := svc.AssignFromGroups(context.Background(), tenantID, uuid.New(), []string{"eng-team"})
	assert.ErrorIs(t, err, memErr)
}

// ── AssignFromGroups — happy path: dept grant + tenant role grant, events ──

func TestAssignFromGroups_HappyPath_GrantsDeptAndRoleWithEvents(t *testing.T) {
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	membershipID := uuid.New()

	dm := []domain.GroupDeptMapping{{TenantID: tenantID, KeycloakGroupName: "eng-team", DepartmentID: deptID}}
	dr := []domain.GroupDeptRoleMapping{{TenantID: tenantID, KeycloakGroupName: "eng-team", RoleCode: domain.DeptReviewer}}
	tr := []domain.GroupTenantRoleMapping{{TenantID: tenantID, KeycloakGroupName: "admins", RoleCode: domain.RoleTenantAdmin}}

	mem := &gmaMembershipRepo{findByUserIDFn: func(_ context.Context, _, uu uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: membershipID, UserID: uu}, nil
	}}
	roles := &gmaRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
	}
	deptMems := &gmaDeptMemRepo{}
	pub := &gmaPub{}
	txr := &gmaTxRunner{pub: pub}

	svc := buildAssignFromGroupsSvc(t, tenantID, dm, dr, tr, mem, roles, deptMems, txr, nil)
	got, err := svc.AssignFromGroups(context.Background(), tenantID, userID, []string{"eng-team", "admins"})
	require.NoError(t, err)

	require.Len(t, got.AssignedDepts, 1)
	assert.Equal(t, deptID, got.AssignedDepts[0])
	require.Len(t, got.GrantedTenantRoles, 1)
	assert.Equal(t, domain.RoleTenantAdmin, got.GrantedTenantRoles[0])

	require.Len(t, pub.events, 2)
	assert.Equal(t, domain.EventDepartmentMembershipGranted, pub.events[0].Type)
	assert.Equal(t, domain.EventTenantRoleGranted, pub.events[1].Type)
}

// ── AssignFromGroups — dept already at same level → LevelChanged skipped, no event ──

func TestAssignFromGroups_DeptAlreadyAtSameLevel_NoLevelChangedEvent(t *testing.T) {
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	dm := []domain.GroupDeptMapping{{TenantID: tenantID, KeycloakGroupName: "eng-team", DepartmentID: deptID}}
	dr := []domain.GroupDeptRoleMapping{{TenantID: tenantID, KeycloakGroupName: "eng-team", RoleCode: domain.DeptReviewer}}

	mem := &gmaMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New()}, nil
	}}
	roles := &gmaRoleRepo{}
	existing := &domain.DeptMembership{DepartmentID: deptID, RoleLevel: domain.DeptReviewer}
	deptMems := &gmaDeptMemRepo{
		assignFn: func(_ context.Context, tid, uid, did, _ uuid.UUID, level domain.DeptRole, _ uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
			return &domain.DeptMembership{TenantID: tid, UserID: uid, DepartmentID: did, RoleLevel: level}, existing, nil
		},
	}
	pub := &gmaPub{}
	txr := &gmaTxRunner{pub: pub}

	svc := buildAssignFromGroupsSvc(t, tenantID, dm, dr, nil, mem, roles, deptMems, txr, nil)
	got, err := svc.AssignFromGroups(context.Background(), tenantID, userID, []string{"eng-team"})
	require.NoError(t, err)
	require.Len(t, got.AssignedDepts, 1)
	assert.Empty(t, pub.events, "TRG-3 no-op: unchanged level must not emit an event")
}

// ── AssignFromGroups — dept level change → LevelChanged event ──────────

func TestAssignFromGroups_DeptLevelChange_EmitsLevelChangedEvent(t *testing.T) {
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	dm := []domain.GroupDeptMapping{{TenantID: tenantID, KeycloakGroupName: "eng-team", DepartmentID: deptID}}
	dr := []domain.GroupDeptRoleMapping{{TenantID: tenantID, KeycloakGroupName: "eng-team", RoleCode: domain.DeptApprover}}

	mem := &gmaMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New()}, nil
	}}
	existing := &domain.DeptMembership{DepartmentID: deptID, RoleLevel: domain.DeptPreparator}
	deptMems := &gmaDeptMemRepo{
		assignFn: func(_ context.Context, tid, uid, did, _ uuid.UUID, level domain.DeptRole, _ uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
			return &domain.DeptMembership{TenantID: tid, UserID: uid, DepartmentID: did, RoleLevel: level}, existing, nil
		},
	}
	pub := &gmaPub{}
	txr := &gmaTxRunner{pub: pub}

	svc := buildAssignFromGroupsSvc(t, tenantID, dm, dr, nil, mem, &gmaRoleRepo{}, deptMems, txr, nil)
	_, err := svc.AssignFromGroups(context.Background(), tenantID, userID, []string{"eng-team"})
	require.NoError(t, err)
	require.Len(t, pub.events, 1)
	assert.Equal(t, domain.EventDepartmentMembershipLevelChanged, pub.events[0].Type)
	payload, ok := pub.events[0].Data.(domain.DepartmentMembershipLevelChangedPayload)
	require.True(t, ok)
	assert.Equal(t, domain.DeptPreparator, payload.PreviousLevel)
	assert.Equal(t, domain.DeptApprover, payload.NewLevel)
}

// ── AssignFromGroups — deptMems.Assign error propagates ────────────────

func TestAssignFromGroups_DeptAssignErrorPropagates(t *testing.T) {
	tenantID, deptID := uuid.New(), uuid.New()
	dm := []domain.GroupDeptMapping{{TenantID: tenantID, KeycloakGroupName: "eng-team", DepartmentID: deptID}}
	dr := []domain.GroupDeptRoleMapping{{TenantID: tenantID, KeycloakGroupName: "eng-team", RoleCode: domain.DeptReviewer}}
	assignErr := errors.New("fk_dm_tenant_dept violation")

	mem := &gmaMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New()}, nil
	}}
	deptMems := &gmaDeptMemRepo{
		assignFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, domain.DeptRole, uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
			return nil, nil, assignErr
		},
	}
	svc := buildAssignFromGroupsSvc(t, tenantID, dm, dr, nil, mem, &gmaRoleRepo{}, deptMems, &gmaTxRunner{}, nil)

	_, err := svc.AssignFromGroups(context.Background(), tenantID, uuid.New(), []string{"eng-team"})
	assert.ErrorIs(t, err, assignErr)
}

// ── AssignFromGroups — GTRM-4 additive-only: already-held role skipped ──

func TestAssignFromGroups_AlreadyHeldTenantRole_NotReGranted(t *testing.T) {
	tenantID := uuid.New()
	tr := []domain.GroupTenantRoleMapping{{TenantID: tenantID, KeycloakGroupName: "admins", RoleCode: domain.RoleTenantAdmin}}

	mem := &gmaMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New()}, nil
	}}
	grantCalled := false
	roles := &gmaRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantAdmin}}, nil // already held
		},
		grantFn: func(context.Context, *domain.TenantRole) (*domain.TenantRole, error) {
			grantCalled = true
			return nil, nil
		},
	}
	svc := buildAssignFromGroupsSvc(t, tenantID, nil, nil, tr, mem, roles, &gmaDeptMemRepo{}, &gmaTxRunner{}, nil)

	got, err := svc.AssignFromGroups(context.Background(), tenantID, uuid.New(), []string{"admins"})
	require.NoError(t, err)
	assert.Empty(t, got.GrantedTenantRoles, "GTRM-4: an already-held role must not be re-granted or re-emitted")
	assert.False(t, grantCalled)
}

// ── AssignFromGroups — roles.ListByUser error propagates ───────────────

func TestAssignFromGroups_RolesListByUserErrorPropagates(t *testing.T) {
	tenantID := uuid.New()
	tr := []domain.GroupTenantRoleMapping{{TenantID: tenantID, KeycloakGroupName: "admins", RoleCode: domain.RoleTenantAdmin}}
	listErr := errors.New("roles unavailable")

	mem := &gmaMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New()}, nil
	}}
	roles := &gmaRoleRepo{listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
		return nil, listErr
	}}
	svc := buildAssignFromGroupsSvc(t, tenantID, nil, nil, tr, mem, roles, &gmaDeptMemRepo{}, &gmaTxRunner{}, nil)

	_, err := svc.AssignFromGroups(context.Background(), tenantID, uuid.New(), []string{"admins"})
	assert.ErrorIs(t, err, listErr)
}

// ── AssignFromGroups — roles.Grant error propagates ─────────────────────

func TestAssignFromGroups_RoleGrantErrorPropagates(t *testing.T) {
	tenantID := uuid.New()
	tr := []domain.GroupTenantRoleMapping{{TenantID: tenantID, KeycloakGroupName: "admins", RoleCode: domain.RoleTenantAdmin}}
	grantErr := errors.New("grant failed")

	mem := &gmaMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New()}, nil
	}}
	roles := &gmaRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		grantFn: func(context.Context, *domain.TenantRole) (*domain.TenantRole, error) {
			return nil, grantErr
		},
	}
	svc := buildAssignFromGroupsSvc(t, tenantID, nil, nil, tr, mem, roles, &gmaDeptMemRepo{}, &gmaTxRunner{}, nil)

	_, err := svc.AssignFromGroups(context.Background(), tenantID, uuid.New(), []string{"admins"})
	assert.ErrorIs(t, err, grantErr)
}

// ── AssignFromGroups — tx error propagates ──────────────────────────────

func TestAssignFromGroups_TxErrorPropagates(t *testing.T) {
	tenantID := uuid.New()
	txErr := errors.New("tx aborted")
	mem := &gmaMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New()}, nil
	}}
	svc := buildAssignFromGroupsSvc(t, tenantID, nil, nil, nil, mem, &gmaRoleRepo{}, &gmaDeptMemRepo{}, &gmaTxRunner{runErr: txErr}, nil)

	_, err := svc.AssignFromGroups(context.Background(), tenantID, uuid.New(), []string{"eng-team"})
	assert.ErrorIs(t, err, txErr)
}

// ── AssignFromGroups — cache invalidation on success ────────────────────

func TestAssignFromGroups_InvalidatesCacheOnSuccess(t *testing.T) {
	tenantID := uuid.New()
	mem := &gmaMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New()}, nil
	}}
	cache := newGMCache()
	cache.values[cacheKeyMembers(tenantID, 50)] = []byte(`[]`)
	cache.values[cacheKeySeatUsage(tenantID)] = []byte(`{}`)
	svc := buildAssignFromGroupsSvc(t, tenantID, nil, nil, nil, mem, &gmaRoleRepo{}, &gmaDeptMemRepo{}, &gmaTxRunner{}, cache)

	_, err := svc.AssignFromGroups(context.Background(), tenantID, uuid.New(), []string{"eng-team"})
	require.NoError(t, err)
	_, membersStillCached := cache.values[cacheKeyMembers(tenantID, 50)]
	_, seatStillCached := cache.values[cacheKeySeatUsage(tenantID)]
	assert.False(t, membersStillCached, "AssignFromGroups must invalidate the members-list cache on success")
	assert.False(t, seatStillCached, "AssignFromGroups must invalidate the seat-usage cache on success")
}

// ── AssignFromGroups — pairing requires the SAME group for dept + role ──

func TestAssignFromGroups_DeptAndRoleFromDifferentGroups_NotPaired(t *testing.T) {
	tenantID, deptID := uuid.New(), uuid.New()
	// dept mapping comes from "eng-team", role mapping from "reviewers" —
	// different group names must NOT be paired into a (dept, level) grant.
	dm := []domain.GroupDeptMapping{{TenantID: tenantID, KeycloakGroupName: "eng-team", DepartmentID: deptID}}
	dr := []domain.GroupDeptRoleMapping{{TenantID: tenantID, KeycloakGroupName: "reviewers", RoleCode: domain.DeptReviewer}}

	mem := &gmaMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New()}, nil
	}}
	assignCalled := false
	deptMems := &gmaDeptMemRepo{
		assignFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, domain.DeptRole, uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
			assignCalled = true
			return nil, nil, nil
		},
	}
	svc := buildAssignFromGroupsSvc(t, tenantID, dm, dr, nil, mem, &gmaRoleRepo{}, deptMems, &gmaTxRunner{}, nil)

	got, err := svc.AssignFromGroups(context.Background(), tenantID, uuid.New(), []string{"eng-team", "reviewers"})
	require.NoError(t, err)
	assert.Empty(t, got.AssignedDepts)
	assert.False(t, assignCalled)
}
