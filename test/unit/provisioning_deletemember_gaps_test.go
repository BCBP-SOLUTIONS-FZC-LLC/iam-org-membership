// Additional unit tests closing remaining branch gaps in
// internal/core/service/provisioning_service.go DeleteMember (I-5): the
// TM-12 idempotency short-circuit (ErrMemberNotFound → nil) vs. a generic
// FindByUserID error, every cascade-step error propagation (roles
// ListByUser/SoftDeleteAllForUser, deptMems SoftDeleteAllForUser,
// memberships SoftDelete), the pub!=nil event-emission branches (role-
// revoked / dept-revoked / membership-revoked), and the full TM-12
// ownerless-escalation branch tree (CountActiveOwners error,
// still-has-other-owners no-op, MarkOwnerlessIfUnset error, flipped=true
// alert vs. flipped=false idempotent no-op).
//
// The only pre-existing coverage
// (TestProvisioning_DeleteMember_WorkflowNotRequired) is a single
// non-owner happy-path call with an untracked TxRunner — none of these
// branches.
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

func buildDeleteMemberSvcFull(mem port.MembershipRepository, roles port.TenantRoleRepository,
	deptMems port.DeptMembershipRepository, tenants port.TenantRepository, txRunner port.TxRunner,
) *service.ProvisioningService {
	return service.NewProvisioningService(
		tenants, mem, roles, deptMems, nil, nil, nil, nil, txRunner, nil, nil,
	)
}

// ── FindByUserID branches ───────────────────────────────────────────────

func TestDeleteMember_MemberNotFound_IdempotentNoOp(t *testing.T) {
	mem := &ruMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return nil, domain.NewError(domain.ErrMemberNotFound, "member not found")
	}}
	svc := buildDeleteMemberSvcFull(mem, &ruRoleRepo{}, &ruDeptMemRepo{}, &invTenantRepo{}, &arTxRunner{})

	err := svc.DeleteMember(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err, "TM-12: a KC USER_DELETE webhook retry against an already-deleted member is a safe no-op")
}

func TestDeleteMember_FindByUserIDOtherErrorPropagates(t *testing.T) {
	findErr := errors.New("db down")
	mem := &ruMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return nil, findErr
	}}
	svc := buildDeleteMemberSvcFull(mem, &ruRoleRepo{}, &ruDeptMemRepo{}, &invTenantRepo{}, &arTxRunner{})

	err := svc.DeleteMember(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, findErr)
}

// ── roles.ListByUser error ──────────────────────────────────────────────

func TestDeleteMember_RolesListByUserErrorPropagates(t *testing.T) {
	listErr := errors.New("roles unavailable")
	mem := &ruMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
	}}
	roles := &ruRoleRepo{listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
		return nil, listErr
	}}
	svc := buildDeleteMemberSvcFull(mem, roles, &ruDeptMemRepo{}, &invTenantRepo{}, &arTxRunner{})

	err := svc.DeleteMember(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, listErr)
}

// ── cascade step errors ─────────────────────────────────────────────────

func TestDeleteMember_RolesSoftDeleteAllForUserErrorPropagates(t *testing.T) {
	cascadeErr := errors.New("cascade blew up")
	mem := &ruMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
	}}
	roles := &ruRoleRepo{
		listByUserFn:           func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, cascadeErr },
	}
	svc := buildDeleteMemberSvcFull(mem, roles, &ruDeptMemRepo{}, &invTenantRepo{}, &arTxRunner{})

	err := svc.DeleteMember(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, cascadeErr)
}

func TestDeleteMember_DeptMemsSoftDeleteAllForUserErrorPropagates(t *testing.T) {
	deptErr := errors.New("dept cascade blew up")
	mem := &ruMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
	}}
	roles := &ruRoleRepo{
		listByUserFn:           func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
	}
	deptMems := &ruDeptMemRepo{softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
		return nil, deptErr
	}}
	svc := buildDeleteMemberSvcFull(mem, roles, deptMems, &invTenantRepo{}, &arTxRunner{})

	err := svc.DeleteMember(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, deptErr)
}

func TestDeleteMember_MembershipSoftDeleteErrorPropagates(t *testing.T) {
	softDeleteErr := errors.New("optimistic_lock_conflict")
	mem := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return softDeleteErr },
	}
	roles := &ruRoleRepo{
		listByUserFn:           func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
	}
	deptMems := &ruDeptMemRepo{softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) { return nil, nil }}
	svc := buildDeleteMemberSvcFull(mem, roles, deptMems, &invTenantRepo{}, &arTxRunner{})

	err := svc.DeleteMember(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, softDeleteErr)
}

// ── event emission: live publisher carries role/dept/membership events ─

func TestDeleteMember_NonOwner_HappyPath_EmitsAllThreeEventKinds(t *testing.T) {
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	mem := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
	}
	roles := &ruRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil }, // not an owner
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantAdmin}}, nil
		},
	}
	deptMems := &ruDeptMemRepo{softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
		return []domain.DeptMembership{{DepartmentID: deptID}}, nil
	}}
	pub := &arPub{}
	svc := buildDeleteMemberSvcFull(mem, roles, deptMems, &invTenantRepo{}, &arTxRunner{pub: pub})

	err := svc.DeleteMember(context.Background(), tenantID, userID)
	require.NoError(t, err)

	require.Len(t, pub.events, 3)
	assert.Equal(t, domain.EventTenantRoleRevoked, pub.events[0].Type)
	assert.Equal(t, domain.EventDepartmentMembershipRevoked, pub.events[1].Type)
	assert.Equal(t, domain.EventMembershipRevoked, pub.events[2].Type)
}

func TestDeleteMember_NoEventPublisherInContext_SkipsAllEmissionsButStillSucceeds(t *testing.T) {
	mem := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
	}
	roles := &ruRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantAdmin}}, nil
		},
	}
	deptMems := &ruDeptMemRepo{softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
		return []domain.DeptMembership{{DepartmentID: uuid.New()}}, nil
	}}
	// arTxRunner{} with no pub set never injects an EventPublisher.
	svc := buildDeleteMemberSvcFull(mem, roles, deptMems, &invTenantRepo{}, &arTxRunner{})

	err := svc.DeleteMember(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err, "a missing EventPublisher must not fail I-5, only skip emission")
}

// ── TM-12 ownerless-escalation branch tree ──────────────────────────────

func ownerRoleListRepo(t *testing.T, softDeleteAllForUserFn func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error), countActiveOwnersFn func(context.Context, uuid.UUID) (int, error)) *ruRoleRepo {
	t.Helper()
	return &ruRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		softDeleteAllForUserFn: softDeleteAllForUserFn,
		countActiveOwnersFn:    countActiveOwnersFn,
	}
}

func TestDeleteMember_WasOwner_CountActiveOwnersErrorPropagates(t *testing.T) {
	countErr := errors.New("count query failed")
	mem := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
	}
	roles := ownerRoleListRepo(t,
		func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		func(context.Context, uuid.UUID) (int, error) { return 0, countErr },
	)
	svc := buildDeleteMemberSvcFull(mem, roles, &ruDeptMemRepo{softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) { return nil, nil }},
		&invTenantRepo{}, &arTxRunner{})

	err := svc.DeleteMember(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, countErr)
}

func TestDeleteMember_WasOwner_OtherOwnersRemain_NoEscalation(t *testing.T) {
	mem := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
	}
	markCalled := false
	roles := ownerRoleListRepo(t,
		func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		func(context.Context, uuid.UUID) (int, error) { return 2, nil }, // others remain
	)
	tenants := &invTenantRepo{markOwnerlessIfUnsetFn: func(context.Context, uuid.UUID) (bool, error) {
		markCalled = true
		return true, nil
	}}
	svc := buildDeleteMemberSvcFull(mem, roles, &ruDeptMemRepo{softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) { return nil, nil }},
		tenants, &arTxRunner{})

	err := svc.DeleteMember(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.False(t, markCalled, "ownerRemaining > 0 must skip MarkOwnerlessIfUnset entirely")
}

func TestDeleteMember_WasOwner_MarkOwnerlessIfUnsetErrorPropagates(t *testing.T) {
	markErr := errors.New("db down")
	mem := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
	}
	roles := ownerRoleListRepo(t,
		func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		func(context.Context, uuid.UUID) (int, error) { return 0, nil }, // last owner just removed
	)
	tenants := &invTenantRepo{markOwnerlessIfUnsetFn: func(context.Context, uuid.UUID) (bool, error) {
		return false, markErr
	}}
	svc := buildDeleteMemberSvcFull(mem, roles, &ruDeptMemRepo{softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) { return nil, nil }},
		tenants, &arTxRunner{})

	err := svc.DeleteMember(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, markErr)
}

func TestDeleteMember_WasOwner_Flipped_LogsAndIncrementsEscalationMetric(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	mem := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
	}
	roles := ownerRoleListRepo(t,
		func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		func(context.Context, uuid.UUID) (int, error) { return 0, nil },
	)
	tenants := &invTenantRepo{markOwnerlessIfUnsetFn: func(context.Context, uuid.UUID) (bool, error) {
		return true, nil // this call is the one that flips it
	}}
	svc := buildDeleteMemberSvcFull(mem, roles, &ruDeptMemRepo{softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) { return nil, nil }},
		tenants, &arTxRunner{})

	err := svc.DeleteMember(context.Background(), tenantID, userID)
	require.NoError(t, err, "TM-12: the escalation itself is advisory logging/metrics only, never blocks the delete")
}

func TestDeleteMember_WasOwner_NotFlipped_IdempotentNoDoubleAlert(t *testing.T) {
	mem := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
	}
	roles := ownerRoleListRepo(t,
		func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		func(context.Context, uuid.UUID) (int, error) { return 0, nil },
	)
	tenants := &invTenantRepo{markOwnerlessIfUnsetFn: func(context.Context, uuid.UUID) (bool, error) {
		return false, nil // already flagged by an earlier retry — idempotent no-op
	}}
	svc := buildDeleteMemberSvcFull(mem, roles, &ruDeptMemRepo{softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) { return nil, nil }},
		tenants, &arTxRunner{})

	err := svc.DeleteMember(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
}
