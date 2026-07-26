// Unit tests for internal/core/service/group_mapping_service.go — the
// List/Replace CRUD paths for the three group-mapping tables (P-14..P-17,
// P-29). AssignFromGroups (the JIT SAML flow, §8.5) is exercised in
// postgres integration tests; here we cover only what's cheap to fake.
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

// ── GroupMappingRepository stub ────────────────────────────────────────

type fakeGroupMappingRepo struct {
	listDeptRoleFn      func(ctx context.Context, tenantID uuid.UUID) ([]domain.GroupDeptRoleMapping, error)
	replaceDeptRoleFn   func(ctx context.Context, tenantID uuid.UUID, desired []domain.GroupDeptRoleMapping) ([]domain.GroupDeptRoleMapping, error)
	listTenantRoleFn    func(ctx context.Context, tenantID uuid.UUID) ([]domain.GroupTenantRoleMapping, error)
	replaceTenantRoleFn func(ctx context.Context, tenantID uuid.UUID, desired []domain.GroupTenantRoleMapping) ([]domain.GroupTenantRoleMapping, error)
	listDeptFn          func(ctx context.Context, tenantID uuid.UUID) ([]domain.GroupDeptMapping, error)
	replaceDeptFn       func(ctx context.Context, tenantID uuid.UUID, desired []domain.GroupDeptMapping) ([]domain.GroupDeptMapping, error)
}

func (f *fakeGroupMappingRepo) ListDeptRoleMappings(ctx context.Context, tenantID uuid.UUID) ([]domain.GroupDeptRoleMapping, error) {
	return f.listDeptRoleFn(ctx, tenantID)
}
func (f *fakeGroupMappingRepo) ReplaceDeptRoleMappings(ctx context.Context, tenantID uuid.UUID, desired []domain.GroupDeptRoleMapping) ([]domain.GroupDeptRoleMapping, error) {
	return f.replaceDeptRoleFn(ctx, tenantID, desired)
}
func (f *fakeGroupMappingRepo) ListTenantRoleMappings(ctx context.Context, tenantID uuid.UUID) ([]domain.GroupTenantRoleMapping, error) {
	return f.listTenantRoleFn(ctx, tenantID)
}
func (f *fakeGroupMappingRepo) ReplaceTenantRoleMappings(ctx context.Context, tenantID uuid.UUID, desired []domain.GroupTenantRoleMapping) ([]domain.GroupTenantRoleMapping, error) {
	return f.replaceTenantRoleFn(ctx, tenantID, desired)
}
func (f *fakeGroupMappingRepo) ListDeptMappings(ctx context.Context, tenantID uuid.UUID) ([]domain.GroupDeptMapping, error) {
	return f.listDeptFn(ctx, tenantID)
}
func (f *fakeGroupMappingRepo) ReplaceDeptMappings(ctx context.Context, tenantID uuid.UUID, desired []domain.GroupDeptMapping) ([]domain.GroupDeptMapping, error) {
	return f.replaceDeptFn(ctx, tenantID, desired)
}

var _ port.GroupMappingRepository = (*fakeGroupMappingRepo)(nil)

// buildSvc wires up a GroupMappingService with the given repo + cache and
// nil stubs for collaborators unused by the list/replace paths.
func buildSvc(repo *fakeGroupMappingRepo, cache port.Cache) *service.GroupMappingService {
	return service.NewGroupMappingService(repo, nil, nil, nil, nil, cache)
}

// ── ListDeptRole (P-14) ────────────────────────────────────────────────

func TestGroupMapping_ListDeptRole_DelegatesToRepo(t *testing.T) {
	tenantID := uuid.New()
	want := []domain.GroupDeptRoleMapping{{ID: uuid.New(), TenantID: tenantID}}
	repo := &fakeGroupMappingRepo{
		listDeptRoleFn: func(_ context.Context, tt uuid.UUID) ([]domain.GroupDeptRoleMapping, error) {
			assert.Equal(t, tenantID, tt)
			return want, nil
		},
	}
	got, err := buildSvc(repo, nil).ListDeptRole(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// ── ReplaceDeptRole (P-15) — validation ────────────────────────────────

func TestGroupMapping_ReplaceDeptRole_RejectsEmptyGroupName(t *testing.T) {
	svc := buildSvc(&fakeGroupMappingRepo{}, nil)
	_, err := svc.ReplaceDeptRole(context.Background(), uuid.New(), []domain.GroupDeptRoleMapping{
		{KeycloakGroupName: "", RoleCode: domain.DeptPreparator},
	})

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
}

func TestGroupMapping_ReplaceDeptRole_RejectsInvalidRoleCode(t *testing.T) {
	svc := buildSvc(&fakeGroupMappingRepo{}, nil)
	_, err := svc.ReplaceDeptRole(context.Background(), uuid.New(), []domain.GroupDeptRoleMapping{
		{KeycloakGroupName: "engineers", RoleCode: domain.DeptRole("wizard")},
	})

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
	assert.Equal(t, "invalid_role_level", de.Details["code"])
}

func TestGroupMapping_ReplaceDeptRole_AcceptsAllValidRoles(t *testing.T) {
	tenantID := uuid.New()
	desired := []domain.GroupDeptRoleMapping{
		{KeycloakGroupName: "prep", RoleCode: domain.DeptPreparator},
		{KeycloakGroupName: "rev", RoleCode: domain.DeptReviewer},
		{KeycloakGroupName: "app", RoleCode: domain.DeptApprover},
	}
	repo := &fakeGroupMappingRepo{
		replaceDeptRoleFn: func(_ context.Context, tt uuid.UUID, d []domain.GroupDeptRoleMapping) ([]domain.GroupDeptRoleMapping, error) {
			assert.Equal(t, tenantID, tt)
			return d, nil
		},
	}
	got, err := buildSvc(repo, nil).ReplaceDeptRole(context.Background(), tenantID, desired)
	require.NoError(t, err)
	assert.Len(t, got, 3)
}

// ── ReplaceDeptRole — repo error propagates + no cache invalidation ────

func TestGroupMapping_ReplaceDeptRole_RepoErrorSkipsInvalidate(t *testing.T) {
	repoErr := errors.New("db down")
	repo := &fakeGroupMappingRepo{
		replaceDeptRoleFn: func(context.Context, uuid.UUID, []domain.GroupDeptRoleMapping) ([]domain.GroupDeptRoleMapping, error) {
			return nil, repoErr
		},
	}
	cache := &spyCache{}
	_, err := buildSvc(repo, cache).ReplaceDeptRole(context.Background(), uuid.New(),
		[]domain.GroupDeptRoleMapping{{KeycloakGroupName: "g", RoleCode: domain.DeptPreparator}})
	assert.ErrorIs(t, err, repoErr)
	assert.Empty(t, cache.deleteCalls,
		"cache must not be invalidated when the repo write failed")
}

// ── ReplaceDeptRole — cache invalidation happens on success ────────────

func TestGroupMapping_ReplaceDeptRole_InvalidatesGRMAndGDMOnSuccess(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeGroupMappingRepo{
		replaceDeptRoleFn: func(_ context.Context, _ uuid.UUID, d []domain.GroupDeptRoleMapping) ([]domain.GroupDeptRoleMapping, error) {
			return d, nil
		},
	}
	cache := &spyCache{}
	_, err := buildSvc(repo, cache).ReplaceDeptRole(context.Background(), tenantID,
		[]domain.GroupDeptRoleMapping{{KeycloakGroupName: "g", RoleCode: domain.DeptPreparator}})
	require.NoError(t, err)

	require.Len(t, cache.deleteCalls, 2, "invalidate must delete both GRM and GDM keys")
	assert.Contains(t, cache.deleteCalls, "om:grm:"+tenantID.String())
	assert.Contains(t, cache.deleteCalls, "om:gdm:"+tenantID.String())
}

// ── ListTenantRole (P-29) ──────────────────────────────────────────────

func TestGroupMapping_ListTenantRole_DelegatesToRepo(t *testing.T) {
	tenantID := uuid.New()
	want := []domain.GroupTenantRoleMapping{{ID: uuid.New(), TenantID: tenantID, RoleCode: domain.RoleTenantAdmin}}
	repo := &fakeGroupMappingRepo{
		listTenantRoleFn: func(_ context.Context, tt uuid.UUID) ([]domain.GroupTenantRoleMapping, error) {
			assert.Equal(t, tenantID, tt)
			return want, nil
		},
	}
	got, err := buildSvc(repo, nil).ListTenantRole(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// ── ReplaceTenantRole — validation ─────────────────────────────────────

func TestGroupMapping_ReplaceTenantRole_RejectsEmptyGroupName(t *testing.T) {
	svc := buildSvc(&fakeGroupMappingRepo{}, nil)
	_, err := svc.ReplaceTenantRole(context.Background(), uuid.New(), []domain.GroupTenantRoleMapping{
		{KeycloakGroupName: "", RoleCode: domain.RoleTenantAdmin},
	})
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
}

func TestGroupMapping_ReplaceTenantRole_RejectsMember(t *testing.T) {
	// GTRM-6 / TR-7: 'member' is derived-only, must not be persisted.
	svc := buildSvc(&fakeGroupMappingRepo{}, nil)
	_, err := svc.ReplaceTenantRole(context.Background(), uuid.New(), []domain.GroupTenantRoleMapping{
		{KeycloakGroupName: "everyone", RoleCode: domain.RoleMember},
	})
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "invalid_role", de.Details["code"])
}

func TestGroupMapping_ReplaceTenantRole_RejectsUnknownRole(t *testing.T) {
	svc := buildSvc(&fakeGroupMappingRepo{}, nil)
	_, err := svc.ReplaceTenantRole(context.Background(), uuid.New(), []domain.GroupTenantRoleMapping{
		{KeycloakGroupName: "g", RoleCode: domain.TenantRoleCode("god")},
	})
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "invalid_role", de.Details["code"])
}

func TestGroupMapping_ReplaceTenantRole_AcceptsAllElevatedRoles(t *testing.T) {
	tenantID := uuid.New()
	desired := []domain.GroupTenantRoleMapping{
		{KeycloakGroupName: "owners", RoleCode: domain.RoleTenantOwner},
		{KeycloakGroupName: "admins", RoleCode: domain.RoleTenantAdmin},
		{KeycloakGroupName: "tenderers", RoleCode: domain.RoleTenderAdmin},
	}
	repo := &fakeGroupMappingRepo{
		replaceTenantRoleFn: func(_ context.Context, _ uuid.UUID, d []domain.GroupTenantRoleMapping) ([]domain.GroupTenantRoleMapping, error) {
			return d, nil
		},
	}
	got, err := buildSvc(repo, nil).ReplaceTenantRole(context.Background(), tenantID, desired)
	require.NoError(t, err)
	assert.Len(t, got, 3)
}

// ── ReplaceTenantRole — repo error propagates ──────────────────────────

func TestGroupMapping_ReplaceTenantRole_RepoErrorPropagates(t *testing.T) {
	repoErr := errors.New("boom")
	repo := &fakeGroupMappingRepo{
		replaceTenantRoleFn: func(context.Context, uuid.UUID, []domain.GroupTenantRoleMapping) ([]domain.GroupTenantRoleMapping, error) {
			return nil, repoErr
		},
	}
	_, err := buildSvc(repo, nil).ReplaceTenantRole(context.Background(), uuid.New(),
		[]domain.GroupTenantRoleMapping{{KeycloakGroupName: "g", RoleCode: domain.RoleTenantAdmin}})
	assert.ErrorIs(t, err, repoErr)
}

// ── ListDept (P-16) ────────────────────────────────────────────────────

func TestGroupMapping_ListDept_DelegatesToRepo(t *testing.T) {
	tenantID := uuid.New()
	want := []domain.GroupDeptMapping{{ID: uuid.New(), TenantID: tenantID}}
	repo := &fakeGroupMappingRepo{
		listDeptFn: func(_ context.Context, tt uuid.UUID) ([]domain.GroupDeptMapping, error) {
			assert.Equal(t, tenantID, tt)
			return want, nil
		},
	}
	got, err := buildSvc(repo, nil).ListDept(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// ── ReplaceDept (P-17) — validation ────────────────────────────────────

func TestGroupMapping_ReplaceDept_RejectsEmptyGroupName(t *testing.T) {
	svc := buildSvc(&fakeGroupMappingRepo{}, nil)
	_, err := svc.ReplaceDept(context.Background(), uuid.New(), []domain.GroupDeptMapping{
		{KeycloakGroupName: "", DepartmentID: uuid.New()},
	})
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
}

func TestGroupMapping_ReplaceDept_RejectsNilDepartmentID(t *testing.T) {
	svc := buildSvc(&fakeGroupMappingRepo{}, nil)
	_, err := svc.ReplaceDept(context.Background(), uuid.New(), []domain.GroupDeptMapping{
		{KeycloakGroupName: "eng", DepartmentID: uuid.Nil},
	})
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "invalid_uuid", de.Details["code"])
}

func TestGroupMapping_ReplaceDept_AcceptsValidMapping(t *testing.T) {
	tenantID := uuid.New()
	desired := []domain.GroupDeptMapping{{KeycloakGroupName: "eng", DepartmentID: uuid.New()}}
	repo := &fakeGroupMappingRepo{
		replaceDeptFn: func(_ context.Context, _ uuid.UUID, d []domain.GroupDeptMapping) ([]domain.GroupDeptMapping, error) {
			return d, nil
		},
	}
	cache := &spyCache{}
	got, err := buildSvc(repo, cache).ReplaceDept(context.Background(), tenantID, desired)
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Len(t, cache.deleteCalls, 2, "invalidate must fire on success")
}

func TestGroupMapping_ReplaceDept_RepoErrorPropagates(t *testing.T) {
	repoErr := errors.New("db bang")
	repo := &fakeGroupMappingRepo{
		replaceDeptFn: func(context.Context, uuid.UUID, []domain.GroupDeptMapping) ([]domain.GroupDeptMapping, error) {
			return nil, repoErr
		},
	}
	_, err := buildSvc(repo, nil).ReplaceDept(context.Background(), uuid.New(),
		[]domain.GroupDeptMapping{{KeycloakGroupName: "g", DepartmentID: uuid.New()}})
	assert.ErrorIs(t, err, repoErr)
}

// ── invalidate — nil cache is safe (no panic) ──────────────────────────

func TestGroupMapping_ReplaceOps_NilCacheIsSafe(t *testing.T) {
	// invalidate returns early when cache==nil; every Replace* path calls
	// invalidate, so a nil-cache path must not panic.
	repo := &fakeGroupMappingRepo{
		replaceDeptRoleFn: func(_ context.Context, _ uuid.UUID, d []domain.GroupDeptRoleMapping) ([]domain.GroupDeptRoleMapping, error) {
			return d, nil
		},
		replaceTenantRoleFn: func(_ context.Context, _ uuid.UUID, d []domain.GroupTenantRoleMapping) ([]domain.GroupTenantRoleMapping, error) {
			return d, nil
		},
		replaceDeptFn: func(_ context.Context, _ uuid.UUID, d []domain.GroupDeptMapping) ([]domain.GroupDeptMapping, error) {
			return d, nil
		},
	}
	svc := buildSvc(repo, nil)
	tenantID := uuid.New()
	assert.NotPanics(t, func() {
		_, _ = svc.ReplaceDeptRole(context.Background(), tenantID,
			[]domain.GroupDeptRoleMapping{{KeycloakGroupName: "g", RoleCode: domain.DeptPreparator}})
		_, _ = svc.ReplaceTenantRole(context.Background(), tenantID,
			[]domain.GroupTenantRoleMapping{{KeycloakGroupName: "g", RoleCode: domain.RoleTenantAdmin}})
		_, _ = svc.ReplaceDept(context.Background(), tenantID,
			[]domain.GroupDeptMapping{{KeycloakGroupName: "g", DepartmentID: uuid.New()}})
	})
}
