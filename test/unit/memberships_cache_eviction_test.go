// CACHE-7 / CACHE-8 regression tests: every write that changes what I-8
// (GET /internal/users/:id/memberships) projects for a user must evict that
// user's om:memberships:{tenant}:{user} entry after commit. Before this fix
// only P-28 ReconcileRoles, I-4 and O-7 did, so a dept demotion/removal,
// suspend, removal, I-5 delete, JIT assign or rejoin left I-8 serving the
// old projection for up to the 300 s ± 30 s TTL — wrong for AuthZ
// Enrichment, and wrong for any assignee-eligibility check built on I-8.
package unit_test

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func membershipsKey(tenantID, userID uuid.UUID) string {
	return "om:memberships:" + tenantID.String() + ":" + userID.String()
}

func okImpactWorkflow() *fakeWorkflowClient {
	return &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		},
	}
}

// ── P-10 Assign (grant / level change) ───────────────────────────────────

func TestMembershipsCache_DeptAssign_EvictsUserProjection(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	cache := &spyCache{}
	svc := service.NewDeptMembershipService(&fullDeptMemRepo{}, &activeMemberRepo{},
		&fakeTenantDeptRepoAssign{isActive: true}, nil, nil, nil, cache, &deptTxRunner{pub: &deptPub{}})

	_, err := svc.Assign(context.Background(), tenantID, userID, uuid.New(), domain.DeptApprover, uuid.New())
	require.NoError(t, err)
	assert.Contains(t, cache.deleteCalls, membershipsKey(tenantID, userID),
		"P-10 must evict the user's I-8 projection so a new/changed level is visible immediately")
}

// ── P-11 Remove ──────────────────────────────────────────────────────────

func TestMembershipsCache_DeptRemove_EvictsUserProjection(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	repo := &fakeDeptMemRepo{
		removeFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.DeptMembership, error) {
			return &domain.DeptMembership{ID: uuid.New()}, nil
		},
	}
	cache := &spyCache{}
	svc := service.NewDeptMembershipService(repo, nil, nil, nil, nil, okImpactWorkflow(), cache, &passthroughTxRunner{})

	_, err := svc.Remove(context.Background(), tenantID, userID, uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Contains(t, cache.deleteCalls, membershipsKey(tenantID, userID),
		"P-11 must evict the user's I-8 projection so the removed department disappears immediately")
}

// A failed write must not evict — eviction is post-commit only (CACHE-6).
func TestMembershipsCache_DeptRemove_WorkflowBlocked_DoesNotEvict(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{ActiveWorkflows: 1}, nil
		},
	}
	cache := &spyCache{}
	svc := service.NewDeptMembershipService(&fakeDeptMemRepo{}, nil, nil, nil, nil, wf, cache, &passthroughTxRunner{})

	_, err := svc.Remove(context.Background(), tenantID, userID, uuid.New(), uuid.New())
	require.Error(t, err)
	assert.NotContains(t, cache.deleteCalls, membershipsKey(tenantID, userID))
}

// ── P-7 SetStatus (suspend / reactivate) ─────────────────────────────────

func TestMembershipsCache_SetStatus_EvictsUserProjection(t *testing.T) {
	for _, status := range []domain.MembershipStatus{domain.MembershipSuspended, domain.MembershipActive} {
		t.Run(string(status), func(t *testing.T) {
			tenantID, userID := uuid.New(), uuid.New()
			m := &fakeMembershipRepo{
				setStatusFn: func(_ context.Context, tt, uu uuid.UUID, st domain.MembershipStatus, ver int64) (*domain.TenantMembership, error) {
					return &domain.TenantMembership{ID: uuid.New(), TenantID: tt, UserID: uu, Status: st, RecordVersion: ver + 1}, nil
				},
			}
			cache := &spyCache{}
			svc := buildMembershipSvcForSetStatus(m, cache, &fakeRPClient{}, okImpactWorkflow())

			_, err := svc.SetStatus(context.Background(), tenantID, userID, status, 1)
			require.NoError(t, err)
			assert.Contains(t, cache.deleteCalls, membershipsKey(tenantID, userID),
				"P-7 must evict the user's I-8 projection so the new status is visible immediately")
		})
	}
}

// ── P-28 ReconcileRoles (already evicted; guards the helper refactor) ────

func TestMembershipsCache_ReconcileRoles_EvictsUserProjection(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID, Status: domain.MembershipActive}
	roles := &extRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantAdmin}}, nil
		},
		revokeFn: func(context.Context, uuid.UUID, uuid.UUID, domain.TenantRoleCode) (*domain.TenantRole, error) {
			return &domain.TenantRole{}, nil
		},
	}
	m := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) { return mem, nil },
	}
	cache := &spyCache{}
	svc := service.NewMembershipService(m, roles, nil, &port.TenantRepositoryNoop{}, nil, cache, nil, nil, &passthroughTxRunner{}, nil, 30)

	_, _, err := svc.ReconcileRoles(context.Background(), tenantID, userID, nil, uuid.New())
	require.NoError(t, err)
	assert.Contains(t, cache.deleteCalls, membershipsKey(tenantID, userID))
}

// ── P-8 RemoveUser ───────────────────────────────────────────────────────

func TestMembershipsCache_RemoveUser_EvictsUserProjection(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), UserID: userID, RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
	}
	r := &ruRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil
		},
	}
	dm := &ruDeptMemRepo{
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			return nil, nil
		},
	}
	cache := &spyCache{}
	svc := service.NewMembershipService(m, r, dm, &port.TenantRepositoryNoop{}, nil, cache,
		&ruRPClient{}, okImpactWorkflow(), &ruTxRunner{pub: &ruPublisher{}}, nil, 30)

	require.NoError(t, svc.RemoveUser(context.Background(), tenantID, userID, uuid.New()))
	assert.Contains(t, cache.deleteCalls, membershipsKey(tenantID, userID),
		"P-8 must evict the removed user's I-8 projection")
}

// ── I-5 DeleteMember ─────────────────────────────────────────────────────

func TestMembershipsCache_DeleteMember_EvictsUserProjection(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	mem := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
	}
	roles := &ruRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil
		},
	}
	deptMems := &ruDeptMemRepo{softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
		return nil, nil
	}}
	cache := &spyCache{}
	svc := service.NewProvisioningService(&invTenantRepo{}, mem, roles, deptMems, nil, nil, nil, nil,
		&arTxRunner{pub: &arPub{}}, cache, nil)

	require.NoError(t, svc.DeleteMember(context.Background(), tenantID, userID))
	assert.Contains(t, cache.deleteCalls, membershipsKey(tenantID, userID),
		"I-5 must evict the deleted user's I-8 projection")
	assert.Contains(t, cache.deleteCalls, "om:members:"+tenantID.String()+":50")
}

// ── I-10 AssignFromGroups (SAML JIT) ─────────────────────────────────────

func TestMembershipsCache_AssignFromGroups_EvictsUserProjection(t *testing.T) {
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	client := &fakeGMClient{
		resolveFn: func(context.Context, uuid.UUID, []string) (*port.GroupResolution, error) {
			return &port.GroupResolution{
				DeptMappings:     []port.ResolvedDeptMapping{{KeycloakGroupName: "eng", DepartmentID: deptID}},
				DeptRoleMappings: []port.ResolvedDeptRoleMapping{{KeycloakGroupName: "eng", RoleCode: domain.DeptReviewer}},
			}, nil
		},
	}
	cache := &spyCache{}
	svc := buildGMSvc(&activeMemberRepo{}, &fakeTenantRoleRepo{}, &fullDeptMemRepo{}, &gmTxRunner{}, cache, client)

	_, err := svc.AssignFromGroups(context.Background(), tenantID, userID, []string{"eng"})
	require.NoError(t, err)
	assert.Contains(t, cache.deleteCalls, membershipsKey(tenantID, userID),
		"I-10 must evict the user's I-8 projection after a JIT grant")
}

// ── I-3 AddFromRegister (accept / rejoin) ────────────────────────────────

func TestMembershipsCache_AddFromRegister_EvictsUserProjection(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	cache := &spyCache{}
	svc := buildAddFromRegisterSvc(&arInviteRepo{}, &arTenantRepo{}, &arMembershipRepo{}, &arRoleRepo{}, &arDeptMemRepo{}, cache, &arTxRunner{})

	_, err := svc.AddFromRegister(context.Background(), tenantID, userID, uuid.New(), "rejoin@example.com")
	require.NoError(t, err)
	assert.Contains(t, cache.deleteCalls, membershipsKey(tenantID, userID),
		"I-3 must evict any I-8 entry left over from a previous membership")
	assert.Contains(t, cache.deleteCalls, "om:members:"+tenantID.String()+":50")
}
