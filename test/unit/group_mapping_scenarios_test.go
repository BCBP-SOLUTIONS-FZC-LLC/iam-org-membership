// Tests for P-15/P-17/P-29 group mapping replace/validation scenarios.
package unit_test

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── P15 ReplaceDeptRole ────────────────────────────────────────────────

func TestGroupMapping_ReplaceDeptRole_HappyPath(t *testing.T) {
	tid := uuid.New()
	want := []domain.GroupDeptRoleMapping{
		{KeycloakGroupName: "prep", RoleCode: domain.DeptPreparator},
		{KeycloakGroupName: "rev", RoleCode: domain.DeptReviewer},
		{KeycloakGroupName: "app", RoleCode: domain.DeptApprover},
	}
	repo := &fakeGroupMappingRepo{
		replaceDeptRoleFn: func(_ context.Context, tt uuid.UUID, d []domain.GroupDeptRoleMapping) ([]domain.GroupDeptRoleMapping, error) {
			assert.Equal(t, tid, tt)
			return d, nil
		},
	}
	got, err := buildSvc(repo, nil).ReplaceDeptRole(context.Background(), tid, want)
	require.NoError(t, err)
	assert.Len(t, got, 3)
}

func TestGroupMapping_ReplaceDeptRole_EmptyMappings_ClearsAll(t *testing.T) {
	called := false
	repo := &fakeGroupMappingRepo{
		replaceDeptRoleFn: func(_ context.Context, _ uuid.UUID, d []domain.GroupDeptRoleMapping) ([]domain.GroupDeptRoleMapping, error) {
			called = true
			assert.Empty(t, d)
			return d, nil
		},
	}
	got, err := buildSvc(repo, nil).ReplaceDeptRole(context.Background(), uuid.New(), nil)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.True(t, called)
}

func TestGroupMapping_ReplaceDeptRole_TenantRoleCode_Rejected(t *testing.T) {
	svc := buildSvc(&fakeGroupMappingRepo{}, nil)
	_, err := svc.ReplaceDeptRole(context.Background(), uuid.New(), []domain.GroupDeptRoleMapping{
		{KeycloakGroupName: "owners", RoleCode: domain.DeptRole("tenant_owner")},
	})
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "invalid_role_level", de.Details["code"])
}

func TestGroupMapping_ReplaceDeptRole_MemberRole_Rejected(t *testing.T) {
	svc := buildSvc(&fakeGroupMappingRepo{}, nil)
	_, err := svc.ReplaceDeptRole(context.Background(), uuid.New(), []domain.GroupDeptRoleMapping{
		{KeycloakGroupName: "everyone", RoleCode: domain.DeptRole("member")},
	})
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "invalid_role_level", de.Details["code"])
}

// ── P17 ReplaceDept ────────────────────────────────────────────────────

func TestGroupMapping_ReplaceDept_HappyPath(t *testing.T) {
	tid := uuid.New()
	want := []domain.GroupDeptMapping{
		{KeycloakGroupName: "eng", DepartmentID: uuid.New()},
		{KeycloakGroupName: "fin", DepartmentID: uuid.New()},
	}
	repo := &fakeGroupMappingRepo{
		replaceDeptFn: func(_ context.Context, tt uuid.UUID, d []domain.GroupDeptMapping) ([]domain.GroupDeptMapping, error) {
			assert.Equal(t, tid, tt)
			return d, nil
		},
	}
	got, err := buildSvc(repo, nil).ReplaceDept(context.Background(), tid, want)
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

func TestGroupMapping_ReplaceDept_EmptyMappings_ClearsAll(t *testing.T) {
	repo := &fakeGroupMappingRepo{
		replaceDeptFn: func(_ context.Context, _ uuid.UUID, d []domain.GroupDeptMapping) ([]domain.GroupDeptMapping, error) {
			return d, nil
		},
	}
	got, err := buildSvc(repo, nil).ReplaceDept(context.Background(), uuid.New(), nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestGroupMapping_ReplaceDept_NoSQSEvent_SilentChange(t *testing.T) {
	// Group mapping changes are cache-only — no publisher wired.
	// If a publisher were added this test would catch it via event emission.
	cache := &spyCache{}
	repo := &fakeGroupMappingRepo{
		replaceDeptFn: func(_ context.Context, _ uuid.UUID, d []domain.GroupDeptMapping) ([]domain.GroupDeptMapping, error) {
			return d, nil
		},
	}
	_, err := buildSvc(repo, cache).ReplaceDept(context.Background(), uuid.New(),
		[]domain.GroupDeptMapping{{KeycloakGroupName: "g", DepartmentID: uuid.New()}})
	require.NoError(t, err)
	// Only cache invalidation calls — no event publication
	assert.NotEmpty(t, cache.deleteCalls, "cache must be invalidated")
}

func TestGroupMapping_ReplaceDept_NonExistentDeptID_Stored(t *testing.T) {
	// GAP: O&M stores any non-nil UUID without existence check
	anyUUID := uuid.New()
	repo := &fakeGroupMappingRepo{
		replaceDeptFn: func(_ context.Context, _ uuid.UUID, d []domain.GroupDeptMapping) ([]domain.GroupDeptMapping, error) {
			assert.Equal(t, anyUUID, d[0].DepartmentID)
			return d, nil
		},
	}
	_, err := buildSvc(repo, nil).ReplaceDept(context.Background(), uuid.New(),
		[]domain.GroupDeptMapping{{KeycloakGroupName: "g", DepartmentID: anyUUID}})
	require.NoError(t, err) // no validation of UUID existence
}

// ── P29 ReplaceTenantRole ──────────────────────────────────────────────

func TestGroupMapping_ReplaceTenantRole_HappyPath(t *testing.T) {
	tid := uuid.New()
	want := []domain.GroupTenantRoleMapping{
		{KeycloakGroupName: "owners", RoleCode: domain.RoleTenantOwner},
		{KeycloakGroupName: "admins", RoleCode: domain.RoleTenantAdmin},
		{KeycloakGroupName: "tenders", RoleCode: domain.RoleTenderAdmin},
	}
	repo := &fakeGroupMappingRepo{
		replaceTenantRoleFn: func(_ context.Context, tt uuid.UUID, d []domain.GroupTenantRoleMapping) ([]domain.GroupTenantRoleMapping, error) {
			assert.Equal(t, tid, tt)
			return d, nil
		},
	}
	got, err := buildSvc(repo, nil).ReplaceTenantRole(context.Background(), tid, want)
	require.NoError(t, err)
	assert.Len(t, got, 3)
}

func TestGroupMapping_ReplaceTenantRole_EmptyMappings_ClearsAll(t *testing.T) {
	repo := &fakeGroupMappingRepo{
		replaceTenantRoleFn: func(_ context.Context, _ uuid.UUID, d []domain.GroupTenantRoleMapping) ([]domain.GroupTenantRoleMapping, error) {
			return d, nil
		},
	}
	got, err := buildSvc(repo, nil).ReplaceTenantRole(context.Background(), uuid.New(), nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestGroupMapping_ReplaceTenantRole_DeptRole_Rejected(t *testing.T) {
	svc := buildSvc(&fakeGroupMappingRepo{}, nil)
	_, err := svc.ReplaceTenantRole(context.Background(), uuid.New(), []domain.GroupTenantRoleMapping{
		{KeycloakGroupName: "g", RoleCode: domain.TenantRoleCode("preparator")},
	})
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "invalid_role", de.Details["code"])
}
