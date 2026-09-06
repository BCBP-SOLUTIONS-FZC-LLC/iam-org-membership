// svc_supplement_test.go fills coverage gaps in provisioning_service.go,
// operator_service.go, and tenant_service.go identified from coverage.out.
//
// All fakes use the "ss" prefix ("svc_supplement") to avoid redeclaring
// types already defined in other files in this package (unit_test).
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

// ══════════════════════════════════════════════════════════════════════════
// PART A — ProvisioningService gaps
// ══════════════════════════════════════════════════════════════════════════

// ── A1: WithLogger ────────────────────────────────────────────────────────

// ssLogger is a minimal port.Logger stub.
type ssLogger struct{}

func (l *ssLogger) Debug(string, map[string]any) {}
func (l *ssLogger) Info(string, map[string]any)  {}
func (l *ssLogger) Warn(string, map[string]any)  {}
func (l *ssLogger) Error(string, map[string]any) {}

var _ port.Logger = (*ssLogger)(nil)

// TestProvisioningService_WithLogger_ReturnsSelf verifies that WithLogger
// sets the logger and returns the same *ProvisioningService pointer so it
// can be chained at wire-up time.
func TestProvisioningService_WithLogger_ReturnsSelf(t *testing.T) {
	svc := service.NewProvisioningService(
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	got := svc.WithLogger(&ssLogger{})

	require.NotNil(t, got, "WithLogger must return a non-nil *ProvisioningService")
	assert.Same(t, svc, got, "WithLogger must return the same receiver pointer (fluent chain)")
}

// ── A2/A3: TrialSignup — catalog error paths ─────────────────────────────

// ssPlanReader is a configurable PlanCatalogReader stub.
type ssPlanReader struct {
	planByCodeFn func(context.Context, domain.TenantPlan) (*domain.Plan, error)
}

func (r *ssPlanReader) Plans(context.Context) ([]domain.Plan, error) { return nil, nil }
func (r *ssPlanReader) PlanByCode(ctx context.Context, code domain.TenantPlan) (*domain.Plan, error) {
	if r.planByCodeFn != nil {
		return r.planByCodeFn(ctx, code)
	}
	return &domain.Plan{Code: code, TrialDurationDays: 30}, nil
}

var _ port.PlanCatalogReader = (*ssPlanReader)(nil)

// ssDeptReader is a configurable DepartmentCatalogReader stub.
type ssDeptReader struct {
	departmentsFn func(context.Context) ([]domain.Department, error)
}

func (r *ssDeptReader) Departments(ctx context.Context) ([]domain.Department, error) {
	if r.departmentsFn != nil {
		return r.departmentsFn(ctx)
	}
	return nil, nil
}
func (r *ssDeptReader) DepartmentByID(_ context.Context, id uuid.UUID) (*domain.Department, error) {
	return nil, domain.NewError(domain.ErrDepartmentNotFound, "not found")
}

var _ port.DepartmentCatalogReader = (*ssDeptReader)(nil)

// TestProvisioningService_TrialSignup_PlanCatalogError verifies that a
// PlanByCode error propagates before RunInTx is called.
//
// This is the coverage gap inside TrialSignup: the s.plans.PlanByCode call
// after slug/locale/name validation. The error path was never exercised by
// the existing unit tests (all of which either return early on validation
// or panic on a nil plans reader).
func TestProvisioningService_TrialSignup_PlanCatalogError(t *testing.T) {
	catalogErr := errors.New("catalog_unavailable")
	svc := service.NewProvisioningService(
		nil, nil, nil, nil, nil, nil,
		&ssDeptReader{},
		&ssPlanReader{
			planByCodeFn: func(context.Context, domain.TenantPlan) (*domain.Plan, error) {
				return nil, catalogErr
			},
		},
		nil, nil, nil,
	)

	_, _, err := svc.TrialSignup(context.Background(), service.TrialSignupInput{
		TenantID:      uuid.New(),
		Slug:          "valid-slug",
		Name:          "Test Tenant",
		Plan:          domain.PlanStarter,
		OwnerUserID:   uuid.New(),
		DefaultLocale: "en-US",
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, catalogErr,
		"PlanByCode error must propagate from TrialSignup before RunInTx")
}

// TestProvisioningService_TrialSignup_DeptCatalogError verifies that a
// Departments() error (fetched before the tx) propagates correctly.
func TestProvisioningService_TrialSignup_DeptCatalogError(t *testing.T) {
	catalogErr := errors.New("departments_unavailable")
	svc := service.NewProvisioningService(
		nil, nil, nil, nil, nil, nil,
		&ssDeptReader{
			departmentsFn: func(context.Context) ([]domain.Department, error) {
				return nil, catalogErr
			},
		},
		&ssPlanReader{}, // PlanByCode succeeds (returns default plan)
		nil, nil, nil,
	)

	_, _, err := svc.TrialSignup(context.Background(), service.TrialSignupInput{
		TenantID:      uuid.New(),
		Slug:          "valid-slug",
		Name:          "Test Tenant",
		Plan:          domain.PlanStarter,
		OwnerUserID:   uuid.New(),
		DefaultLocale: "en-US",
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, catalogErr,
		"Departments() error must propagate from TrialSignup before RunInTx")
}

// ── A4: DeleteMember — idempotency branch (ErrMemberNotFound → nil) ──────

// ssMembershipRepoDM is a MembershipRepository stub for DeleteMember tests.
type ssMembershipRepoDM struct {
	findByUserIDFn func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error)
	softDeleteFn   func(context.Context, uuid.UUID, uuid.UUID, int64) error
}

func (f *ssMembershipRepoDM) List(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
	return nil, nil
}
func (f *ssMembershipRepoDM) FindByUserID(ctx context.Context, tenantID, userID uuid.UUID) (*domain.TenantMembership, error) {
	if f.findByUserIDFn != nil {
		return f.findByUserIDFn(ctx, tenantID, userID)
	}
	return nil, domain.NewError(domain.ErrMemberNotFound, "not found")
}
func (f *ssMembershipRepoDM) Insert(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
	return nil, nil
}
func (f *ssMembershipRepoDM) SetStatus(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
	return nil, nil
}
func (f *ssMembershipRepoDM) SoftDelete(ctx context.Context, tenantID, userID uuid.UUID, ver int64) error {
	if f.softDeleteFn != nil {
		return f.softDeleteFn(ctx, tenantID, userID, ver)
	}
	return nil
}
func (f *ssMembershipRepoDM) CountActive(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (f *ssMembershipRepoDM) ListActiveUserIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

var _ port.MembershipRepository = (*ssMembershipRepoDM)(nil)

// ssRoleRepoDM is a TenantRoleRepository stub for DeleteMember tests.
type ssRoleRepoDM struct {
	listByUserFn           func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error)
	softDeleteAllForUserFn func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error)
}

func (r *ssRoleRepoDM) ListByUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenantRole, error) {
	if r.listByUserFn != nil {
		return r.listByUserFn(ctx, tenantID, userID)
	}
	return nil, nil
}
func (r *ssRoleRepoDM) ListByRole(context.Context, uuid.UUID, domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *ssRoleRepoDM) CountActiveOwners(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (r *ssRoleRepoDM) Grant(context.Context, *domain.TenantRole) (*domain.TenantRole, error) {
	return nil, nil
}
func (r *ssRoleRepoDM) Revoke(context.Context, uuid.UUID, uuid.UUID, domain.TenantRoleCode) (*domain.TenantRole, error) {
	return nil, nil
}
func (r *ssRoleRepoDM) SoftDeleteAllForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenantRole, error) {
	if r.softDeleteAllForUserFn != nil {
		return r.softDeleteAllForUserFn(ctx, tenantID, userID)
	}
	return nil, nil
}

var _ port.TenantRoleRepository = (*ssRoleRepoDM)(nil)

// ssDeptMemRepoDM is a DeptMembershipRepository stub for DeleteMember tests.
type ssDeptMemRepoDM struct {
	softDeleteAllForUserFn func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error)
}

func (f *ssDeptMemRepoDM) ListByUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (f *ssDeptMemRepoDM) ListByDepartment(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (f *ssDeptMemRepoDM) Assign(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, domain.DeptRole, uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
	return nil, nil, nil
}
func (f *ssDeptMemRepoDM) Remove(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.DeptMembership, error) {
	return nil, nil
}
func (f *ssDeptMemRepoDM) SoftDeleteAllForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.DeptMembership, error) {
	if f.softDeleteAllForUserFn != nil {
		return f.softDeleteAllForUserFn(ctx, tenantID, userID)
	}
	return nil, nil
}
func (f *ssDeptMemRepoDM) SoftDeleteAllForDept(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}

var _ port.DeptMembershipRepository = (*ssDeptMemRepoDM)(nil)

// buildDeleteMemberSvc is a helper that wires only the collaborators
// DeleteMember uses, passing a passthroughTxRunner for tx support.
func buildDeleteMemberSvc(
	mem port.MembershipRepository,
	roles port.TenantRoleRepository,
	deptMems port.DeptMembershipRepository,
) *service.ProvisioningService {
	return service.NewProvisioningService(
		nil, mem, roles, deptMems,
		nil, nil, nil, nil,
		&passthroughTxRunner{}, nil, nil,
	)
}

// TestProvisioningService_DeleteMember_AlreadyDeleted_IsIdempotent verifies
// the TM-12 idempotency branch: when FindByUserID returns ErrMemberNotFound
// (the member row has deleted_at IS NOT NULL), DeleteMember returns nil so
// KC webhook retries are safe no-ops.
func TestProvisioningService_DeleteMember_AlreadyDeleted_IsIdempotent(t *testing.T) {
	mem := &ssMembershipRepoDM{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return nil, domain.NewError(domain.ErrMemberNotFound, "already deleted")
		},
	}
	svc := buildDeleteMemberSvc(mem, nil, nil)

	err := svc.DeleteMember(context.Background(), uuid.New(), uuid.New())
	assert.NoError(t, err, "TM-12: already-deleted member must return nil (idempotent KC retry)")
}

// TestProvisioningService_DeleteMember_FindByUserIDError_Propagates verifies
// that a non-ErrMemberNotFound error from FindByUserID propagates out.
func TestProvisioningService_DeleteMember_FindByUserIDError_Propagates(t *testing.T) {
	dbErr := errors.New("db_unavailable")
	mem := &ssMembershipRepoDM{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return nil, dbErr
		},
	}
	svc := buildDeleteMemberSvc(mem, nil, nil)

	err := svc.DeleteMember(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, dbErr)
}

// TestProvisioningService_DeleteMember_ListByUserError_Propagates verifies
// that a roles.ListByUser error during the wasOwner check propagates.
func TestProvisioningService_DeleteMember_ListByUserError_Propagates(t *testing.T) {
	rolesErr := errors.New("roles_db_error")
	mem := &ssMembershipRepoDM{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
	}
	roles := &ssRoleRepoDM{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, rolesErr
		},
	}
	svc := buildDeleteMemberSvc(mem, roles, nil)

	err := svc.DeleteMember(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, rolesErr)
}

// TestProvisioningService_DeleteMember_SoftDeleteAllForUserError_Propagates
// verifies that a roles.SoftDeleteAllForUser error during cascade 1 propagates.
func TestProvisioningService_DeleteMember_SoftDeleteAllForUserError_Propagates(t *testing.T) {
	cascadeErr := errors.New("cascade_role_delete_failed")
	mem := &ssMembershipRepoDM{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
	}
	roles := &ssRoleRepoDM{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil // non-owner, no role grants
		},
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, cascadeErr
		},
	}
	svc := buildDeleteMemberSvc(mem, roles, nil)

	err := svc.DeleteMember(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, cascadeErr)
}

// TestProvisioningService_DeleteMember_DeptSoftDeleteError_Propagates
// verifies that a deptMems.SoftDeleteAllForUser error during cascade 2 propagates.
func TestProvisioningService_DeleteMember_DeptSoftDeleteError_Propagates(t *testing.T) {
	deptErr := errors.New("dept_cascade_delete_failed")
	mem := &ssMembershipRepoDM{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
	}
	roles := &ssRoleRepoDM{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil
		},
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil // cascade 1 succeeds
		},
	}
	deptMems := &ssDeptMemRepoDM{
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			return nil, deptErr
		},
	}
	svc := buildDeleteMemberSvc(mem, roles, deptMems)

	err := svc.DeleteMember(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, deptErr)
}

// ══════════════════════════════════════════════════════════════════════════
// PART B — OperatorService gaps (blackbox via service.NewOperatorService)
// ══════════════════════════════════════════════════════════════════════════

// ── B1–B2: SetFeatureFlags validation (fires before txRunner) ─────────────

// TestOperatorService_SetFeatureFlags_UnknownFlagKey_ReturnsValidationError
// verifies the allow-list guard (LLD O-4) rejects an unknown flag key with
// code "unknown_feature_flag" before any SQL is attempted.
func TestOperatorService_SetFeatureFlags_UnknownFlagKey_ReturnsValidationError(t *testing.T) {
	svc := service.NewOperatorService(nil, nil, nil, nil, nil)

	_, err := svc.SetFeatureFlags(context.Background(), uuid.New(),
		map[string]any{"sso_enable": true}, 1) // typo: should be "sso_enabled"

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
	assert.Equal(t, "unknown_feature_flag", de.Details["code"])
	assert.Equal(t, "sso_enable", de.Details["key"])
}

// TestOperatorService_SetFeatureFlags_NonScalarValue_ReturnsValidationError
// verifies that a nested object value is rejected with "invalid_feature_value"
// (PLAN-6(d) scalar-only guard) before any SQL is attempted.
func TestOperatorService_SetFeatureFlags_NonScalarValue_ReturnsValidationError(t *testing.T) {
	svc := service.NewOperatorService(nil, nil, nil, nil, nil)

	_, err := svc.SetFeatureFlags(context.Background(), uuid.New(),
		map[string]any{"custom_branding": []string{"a", "b"}}, 1) // slice is not a scalar

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
	assert.Equal(t, "invalid_feature_value", de.Details["code"])
}

// ── B3: ReassignOwner — tenant offboarded ────────────────────────────────

// ssTenantRepoOp is a TenantRepository stub for OperatorService tests.
type ssTenantRepoOp struct {
	port.TenantRepositoryNoop
	findByIDFn func(context.Context, uuid.UUID) (*domain.Tenant, error)
}

func (r *ssTenantRepoOp) FindByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	if r.findByIDFn != nil {
		return r.findByIDFn(ctx, id)
	}
	return nil, domain.NewError(domain.ErrTenantNotFound, "not found")
}
func (r *ssTenantRepoOp) FindByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return r.FindByID(ctx, id)
}

var _ port.TenantRepository = (*ssTenantRepoOp)(nil)

// ssMembershipRepoOp is a MembershipRepository stub for OperatorService tests.
type ssMembershipRepoOp struct {
	findByUserIDFn func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error)
}

func (f *ssMembershipRepoOp) List(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
	return nil, nil
}
func (f *ssMembershipRepoOp) FindByUserID(ctx context.Context, tenantID, userID uuid.UUID) (*domain.TenantMembership, error) {
	if f.findByUserIDFn != nil {
		return f.findByUserIDFn(ctx, tenantID, userID)
	}
	return nil, domain.NewError(domain.ErrMemberNotFound, "not found")
}
func (f *ssMembershipRepoOp) Insert(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
	return nil, nil
}
func (f *ssMembershipRepoOp) SetStatus(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
	return nil, nil
}
func (f *ssMembershipRepoOp) SoftDelete(context.Context, uuid.UUID, uuid.UUID, int64) error {
	return nil
}
func (f *ssMembershipRepoOp) CountActive(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (f *ssMembershipRepoOp) ListActiveUserIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

var _ port.MembershipRepository = (*ssMembershipRepoOp)(nil)

// TestOperatorService_ReassignOwner_OffboardedTenant_ReturnsErrTenantOffboarded
// verifies that ReassignOwner returns ErrTenantOffboarded when the tenant's
// subscription status is "offboarded", before any membership lookup.
func TestOperatorService_ReassignOwner_OffboardedTenant_ReturnsErrTenantOffboarded(t *testing.T) {
	tenants := &ssTenantRepoOp{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, Status: domain.StatusOffboarded}, nil
		},
	}
	svc := service.NewOperatorService(tenants, nil, nil, nil, nil)

	_, err := svc.ReassignOwner(context.Background(), uuid.New(), uuid.New(), uuid.New())

	assert.ErrorIs(t, err, domain.ErrTenantOffboarded)
}

// TestOperatorService_ReassignOwner_MemberNotFound_ReturnsErrInvalidOwnerCandidate
// verifies that when FindByUserID returns ErrMemberNotFound (the new owner
// candidate is not in the tenant), ReassignOwner returns ErrInvalidOwnerCandidate.
func TestOperatorService_ReassignOwner_MemberNotFound_ReturnsErrInvalidOwnerCandidate(t *testing.T) {
	tenants := &ssTenantRepoOp{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, Status: domain.StatusActive}, nil
		},
	}
	memberships := &ssMembershipRepoOp{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return nil, domain.NewError(domain.ErrMemberNotFound, "not a member")
		},
	}
	svc := service.NewOperatorService(tenants, nil, memberships, nil, nil)

	_, err := svc.ReassignOwner(context.Background(), uuid.New(), uuid.New(), uuid.New())

	assert.ErrorIs(t, err, domain.ErrInvalidOwnerCandidate)
}

// TestOperatorService_ReassignOwner_InactiveMember_ReturnsErrInvalidOwnerCandidate
// verifies that when the new owner's membership status is not MembershipActive
// (e.g. "suspended"), ReassignOwner returns ErrInvalidOwnerCandidate.
func TestOperatorService_ReassignOwner_InactiveMember_ReturnsErrInvalidOwnerCandidate(t *testing.T) {
	tenants := &ssTenantRepoOp{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, Status: domain.StatusActive}, nil
		},
	}
	memberships := &ssMembershipRepoOp{
		findByUserIDFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
			// Suspended member — not eligible to become owner.
			return &domain.TenantMembership{
				ID: uuid.New(), TenantID: tid, UserID: uid,
				Status: domain.MembershipSuspended,
			}, nil
		},
	}
	svc := service.NewOperatorService(tenants, nil, memberships, nil, nil)

	_, err := svc.ReassignOwner(context.Background(), uuid.New(), uuid.New(), uuid.New())

	assert.ErrorIs(t, err, domain.ErrInvalidOwnerCandidate)
}

// ══════════════════════════════════════════════════════════════════════════
// PART C — TenantService gaps
// ══════════════════════════════════════════════════════════════════════════

// ssTenantRepoIncDel is a TenantRepository that allows separate control of
// FindByID vs FindByIDIncludingDeleted, so GetIncludingOffboarded can be
// tested independently of Get.
type ssTenantRepoIncDel struct {
	port.TenantRepositoryNoop
	findByIDFn                 func(context.Context, uuid.UUID) (*domain.Tenant, error)
	findByIDIncludingDeletedFn func(context.Context, uuid.UUID) (*domain.Tenant, error)
}

func (r *ssTenantRepoIncDel) FindByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	if r.findByIDFn != nil {
		return r.findByIDFn(ctx, id)
	}
	return nil, domain.NewError(domain.ErrTenantNotFound, "not found")
}
func (r *ssTenantRepoIncDel) FindByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	if r.findByIDIncludingDeletedFn != nil {
		return r.findByIDIncludingDeletedFn(ctx, id)
	}
	return nil, domain.NewError(domain.ErrTenantNotFound, "not found")
}

var _ port.TenantRepository = (*ssTenantRepoIncDel)(nil)

// ── C1: GetIncludingOffboarded — delegates to repo.FindByIDIncludingDeleted ─

// TestTenantService_GetIncludingOffboarded_DelegatesToRepo verifies that
// GetIncludingOffboarded returns what the underlying repo returns, including
// soft-deleted tenants (deleted_at IS NOT NULL rows).
func TestTenantService_GetIncludingOffboarded_DelegatesToRepo(t *testing.T) {
	tenantID := uuid.New()
	expected := &domain.Tenant{ID: tenantID, Slug: "offboarded-tenant", Status: domain.StatusOffboarded}
	repo := &ssTenantRepoIncDel{
		findByIDIncludingDeletedFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			assert.Equal(t, tenantID, id)
			return expected, nil
		},
	}
	svc := service.NewTenantService(repo, nil, &tsRP{})

	got, err := svc.GetIncludingOffboarded(context.Background(), tenantID)

	require.NoError(t, err)
	assert.Equal(t, expected, got)
}

// ── C2: GetIncludingOffboarded — propagates error ─────────────────────────

// TestTenantService_GetIncludingOffboarded_PropagatesError verifies that an
// error from FindByIDIncludingDeleted propagates unchanged to the caller.
func TestTenantService_GetIncludingOffboarded_PropagatesError(t *testing.T) {
	repoErr := domain.NewError(domain.ErrTenantNotFound, "tenant not found")
	repo := &ssTenantRepoIncDel{
		findByIDIncludingDeletedFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
			return nil, repoErr
		},
	}
	svc := service.NewTenantService(repo, nil, &tsRP{})

	_, err := svc.GetIncludingOffboarded(context.Background(), uuid.New())

	assert.ErrorIs(t, err, domain.ErrTenantNotFound)
}

// ── C3: Patch — Update repo error propagates ──────────────────────────────

// TestTenantService_Patch_UpdateError_Propagates verifies that when
// tenants.Update returns an error (e.g. optimistic lock conflict, DB down),
// the error propagates from Patch. This covers the Update call path that was
// missing from the existing tests which only covered validation returns.
func TestTenantService_Patch_UpdateError_Propagates(t *testing.T) {
	updateErr := domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict")
	repo := &tsRepo{
		updateFn: func(context.Context, uuid.UUID, *domain.TenantPatch) (*domain.Tenant, error) {
			return nil, updateErr
		},
	}
	svc := service.NewTenantService(repo, nil, &tsRP{})
	name := "New Name"

	_, _, err := svc.Patch(context.Background(), uuid.New(), &domain.TenantPatch{
		Name:          &name,
		RecordVersion: 1,
	})

	assert.ErrorIs(t, err, domain.ErrOptimisticLockConflict)
}

// ── A5: TrialSignup — invalid plan (ErrInvalidPlan branch) ──────────────────

// TestProvisioningService_TrialSignup_InvalidPlan_ReturnsErrInvalidPlan covers
// line 94.10-97.61 in provisioning_service.go: the switch default case when
// req.Plan is not one of {starter, pro, enterprise}.
func TestProvisioningService_TrialSignup_InvalidPlan_ReturnsErrInvalidPlan(t *testing.T) {
	svc := service.NewProvisioningService(
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)

	_, _, err := svc.TrialSignup(context.Background(), service.TrialSignupInput{
		TenantID:    uuid.New(),
		Slug:        "valid-slug",
		Name:        "Test Tenant",
		Plan:        domain.TenantPlan("unknown-plan"), // not in the enum
		OwnerUserID: uuid.New(),
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrInvalidPlan,
		"unknown plan must return ErrInvalidPlan before reaching catalog or DB")
}

// ── C4: Patch — empty patch calls FindByID and propagates its error ───────

// TestTenantService_Patch_EmptyPatch_FindByIDError_Propagates verifies that
// when all patch fields are nil (no-op path), FindByID is called and its
// error propagates (the remaining uncovered branch in the Patch no-op path).
func TestTenantService_Patch_EmptyPatch_FindByIDError_Propagates(t *testing.T) {
	findErr := domain.NewError(domain.ErrTenantNotFound, "tenant not found")
	repo := &fakeTenantRepo{
		findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
			return nil, findErr
		},
	}
	svc := service.NewTenantService(repo, nil, &tsRP{})

	// All-nil patch fields → no-op branch → FindByID called
	_, _, err := svc.Patch(context.Background(), uuid.New(), &domain.TenantPatch{})

	assert.ErrorIs(t, err, domain.ErrTenantNotFound)
}
