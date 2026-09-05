package service

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Additional AssignFromGroups branch coverage not exercised by
// group_mapping_assign_test.go: the dedup-seen skip (two different
// qualifying groups resolving to the identical (dept, level) pair), the
// GTRM-6 member-role skip, and the no-event-publisher-in-context skip for
// both the dept-assign and tenant-role-grant loops.

// ── defensive re-filter: a cached mapping whose group isn't in the
//    caller's current groupNames must be skipped (belt-and-suspenders —
//    resolveMappings' cache snapshot may outlive a group membership
//    change between JIT logins) ───────────────────────────────────────

func TestAssignFromGroups_DeptMappingForGroupNotInCurrentSet_Skipped(t *testing.T) {
	tenantID, deptID := uuid.New(), uuid.New()
	// "stale-group" is present in the cached resolution snapshot but NOT
	// in the groupNames the caller currently asserts — its dm entry must
	// never reach the groupSet-filtered loop body.
	dm := []domain.GroupDeptMapping{{TenantID: tenantID, KeycloakGroupName: "stale-group", DepartmentID: deptID}}
	dr := []domain.GroupDeptRoleMapping{{TenantID: tenantID, KeycloakGroupName: "stale-group", RoleCode: domain.DeptReviewer}}
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

	got, err := svc.AssignFromGroups(context.Background(), tenantID, uuid.New(), []string{"a-current-group"})
	require.NoError(t, err)
	assert.Empty(t, got.AssignedDepts)
	assert.False(t, assignCalled, "a dept mapping for a group outside the current set must be skipped")
}

func TestAssignFromGroups_DeptRoleMappingForGroupNotInCurrentSet_Skipped(t *testing.T) {
	tenantID, deptID := uuid.New(), uuid.New()
	// dm's group ("eng-team") IS current; dr's group ("stale-role-group")
	// is not — the inner drMaps loop's own groupSet filter must skip it
	// (distinct from the dm/dr-mismatch "not paired" case, which tests a
	// dr group that IS current but differs from dm's).
	dm := []domain.GroupDeptMapping{{TenantID: tenantID, KeycloakGroupName: "eng-team", DepartmentID: deptID}}
	dr := []domain.GroupDeptRoleMapping{{TenantID: tenantID, KeycloakGroupName: "stale-role-group", RoleCode: domain.DeptReviewer}}
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

	got, err := svc.AssignFromGroups(context.Background(), tenantID, uuid.New(), []string{"eng-team"})
	require.NoError(t, err)
	assert.Empty(t, got.AssignedDepts)
	assert.False(t, assignCalled, "a dept-role mapping for a group outside the current set must be skipped")
}

func TestAssignFromGroups_TenantRoleMappingForGroupNotInCurrentSet_Skipped(t *testing.T) {
	tenantID := uuid.New()
	tr := []domain.GroupTenantRoleMapping{{TenantID: tenantID, KeycloakGroupName: "stale-admin-group", RoleCode: domain.RoleTenantAdmin}}
	mem := &gmaMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New()}, nil
	}}
	grantCalled := false
	roles := &gmaRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		grantFn: func(context.Context, *domain.TenantRole) (*domain.TenantRole, error) {
			grantCalled = true
			return nil, nil
		},
	}
	svc := buildAssignFromGroupsSvc(t, tenantID, nil, nil, tr, mem, roles, &gmaDeptMemRepo{}, &gmaTxRunner{}, nil)

	got, err := svc.AssignFromGroups(context.Background(), tenantID, uuid.New(), []string{"a-current-group"})
	require.NoError(t, err)
	assert.Empty(t, got.GrantedTenantRoles, "a tenant-role mapping for a group outside the current set must be skipped")
	assert.False(t, grantCalled)
}

// ── dedup: two groups resolving to the SAME (dept, level) pair ─────────

func TestAssignFromGroups_DedupSkipsRepeatedDeptRolePairAcrossGroups(t *testing.T) {
	tenantID, deptID := uuid.New(), uuid.New()
	// Both "eng-team" and "eng-team-2" independently pair to (deptID, DeptReviewer).
	dm := []domain.GroupDeptMapping{
		{TenantID: tenantID, KeycloakGroupName: "eng-team", DepartmentID: deptID},
		{TenantID: tenantID, KeycloakGroupName: "eng-team-2", DepartmentID: deptID},
	}
	dr := []domain.GroupDeptRoleMapping{
		{TenantID: tenantID, KeycloakGroupName: "eng-team", RoleCode: domain.DeptReviewer},
		{TenantID: tenantID, KeycloakGroupName: "eng-team-2", RoleCode: domain.DeptReviewer},
	}
	mem := &gmaMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New()}, nil
	}}
	assignCalls := 0
	deptMems := &gmaDeptMemRepo{
		assignFn: func(_ context.Context, tid, uid, did, _ uuid.UUID, level domain.DeptRole, _ uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
			assignCalls++
			return &domain.DeptMembership{TenantID: tid, UserID: uid, DepartmentID: did, RoleLevel: level}, nil, nil
		},
	}
	svc := buildAssignFromGroupsSvc(t, tenantID, dm, dr, nil, mem, &gmaRoleRepo{}, deptMems, &gmaTxRunner{}, nil)

	got, err := svc.AssignFromGroups(context.Background(), tenantID, uuid.New(), []string{"eng-team", "eng-team-2"})
	require.NoError(t, err)
	require.Len(t, got.AssignedDepts, 1, "the second group resolving to the identical (dept, level) pair must be deduped")
	assert.Equal(t, 1, assignCalls)
}

// ── GTRM-6: a resolved 'member' tenant-role mapping is skipped ─────────

func TestAssignFromGroups_MemberRoleMappingSkipped(t *testing.T) {
	tenantID := uuid.New()
	tr := []domain.GroupTenantRoleMapping{{TenantID: tenantID, KeycloakGroupName: "everyone", RoleCode: domain.RoleMember}}
	mem := &gmaMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New()}, nil
	}}
	grantCalled := false
	roles := &gmaRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		grantFn: func(context.Context, *domain.TenantRole) (*domain.TenantRole, error) {
			grantCalled = true
			return nil, nil
		},
	}
	svc := buildAssignFromGroupsSvc(t, tenantID, nil, nil, tr, mem, roles, &gmaDeptMemRepo{}, &gmaTxRunner{}, nil)

	got, err := svc.AssignFromGroups(context.Background(), tenantID, uuid.New(), []string{"everyone"})
	require.NoError(t, err)
	assert.Empty(t, got.GrantedTenantRoles, "TR-7/GTRM-6: 'member' is derived, never JIT-granted")
	assert.False(t, grantCalled)
}

// ── no EventPublisher in context: dept-grant and role-grant both skip ──

func TestAssignFromGroups_NoEventPublisherInContext_SkipsBothEmissions(t *testing.T) {
	tenantID, deptID := uuid.New(), uuid.New()
	dm := []domain.GroupDeptMapping{{TenantID: tenantID, KeycloakGroupName: "eng-team", DepartmentID: deptID}}
	dr := []domain.GroupDeptRoleMapping{{TenantID: tenantID, KeycloakGroupName: "eng-team", RoleCode: domain.DeptReviewer}}
	trMap := []domain.GroupTenantRoleMapping{{TenantID: tenantID, KeycloakGroupName: "admins", RoleCode: domain.RoleTenantAdmin}}

	mem := &gmaMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New()}, nil
	}}
	roles := &gmaRoleRepo{listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil }}
	deptMems := &gmaDeptMemRepo{}
	// gmaTxRunner{} with no pub set never injects an EventPublisher — the
	// production passthroughTxRunner analog to "no publisher configured".
	svc := buildAssignFromGroupsSvc(t, tenantID, dm, dr, trMap, mem, roles, deptMems, &gmaTxRunner{}, nil)

	got, err := svc.AssignFromGroups(context.Background(), tenantID, uuid.New(), []string{"eng-team", "admins"})
	require.NoError(t, err, "a missing EventPublisher must not fail the JIT apply, only skip emission")
	assert.Len(t, got.AssignedDepts, 1)
	assert.Len(t, got.GrantedTenantRoles, 1)
}
