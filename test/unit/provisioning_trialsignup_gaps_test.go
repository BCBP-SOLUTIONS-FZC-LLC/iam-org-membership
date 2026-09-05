// Additional unit tests closing remaining branch gaps in
// internal/core/service/provisioning_service.go TrialSignup (I-1): the
// plan-whitelist validation error, the empty-DefaultLocale default-to-
// en-US branch, every tx-step error propagation (tenant Insert, dept
// Activate, label Seed, membership Insert, role Grant), and the
// pub!=nil event-emission branch (TenantCreated + TrialStarted +
// TenantRoleGranted), none of which buildTrialProvisioningSvc's
// happy-path fakes or the department-filter-focused tests in
// provisioning_trial_departments_test.go exercise (that file's TxRunner
// never injects an EventPublisher, and none of its fakes can be told to
// fail).
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

// ── configurable fakes (ts = TrialSignup) ───────────────────────────────

type tsTenantRepo struct {
	port.TenantRepositoryNoop
	insertFn func(ctx context.Context, t *domain.Tenant) (*domain.Tenant, bool, error)
}

func (r *tsTenantRepo) Insert(ctx context.Context, t *domain.Tenant) (*domain.Tenant, bool, error) {
	if r.insertFn != nil {
		return r.insertFn(ctx, t)
	}
	return t, true, nil
}

var _ port.TenantRepository = (*tsTenantRepo)(nil)

type tsTenantDeptRepo struct {
	activateFn func(ctx context.Context, tenantID, deptID uuid.UUID) (*domain.TenantDepartment, error)
}

func (r *tsTenantDeptRepo) List(context.Context, uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *tsTenantDeptRepo) ListActive(context.Context, uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *tsTenantDeptRepo) Find(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
	return nil, nil
}
func (r *tsTenantDeptRepo) Activate(ctx context.Context, tenantID, deptID uuid.UUID) (*domain.TenantDepartment, error) {
	if r.activateFn != nil {
		return r.activateFn(ctx, tenantID, deptID)
	}
	return &domain.TenantDepartment{TenantID: tenantID, DepartmentID: deptID, IsActive: true, RecordVersion: 1}, nil
}
func (r *tsTenantDeptRepo) SetActive(context.Context, uuid.UUID, uuid.UUID, bool, int64) (*domain.TenantDepartment, error) {
	return nil, nil
}

var _ port.TenantDepartmentRepository = (*tsTenantDeptRepo)(nil)

type tsLabelRepo struct {
	seedFn func(ctx context.Context, tenantID uuid.UUID) ([]domain.DeptRoleLabel, error)
}

func (r *tsLabelRepo) List(context.Context, uuid.UUID) ([]domain.DeptRoleLabel, error) {
	return nil, nil
}
func (r *tsLabelRepo) Update(context.Context, uuid.UUID, domain.DeptRole, string, int64) (*domain.DeptRoleLabel, error) {
	return nil, nil
}
func (r *tsLabelRepo) Seed(ctx context.Context, tenantID uuid.UUID) ([]domain.DeptRoleLabel, error) {
	if r.seedFn != nil {
		return r.seedFn(ctx, tenantID)
	}
	return nil, nil
}

var _ port.DeptRoleLabelRepository = (*tsLabelRepo)(nil)

type tsMembershipRepo struct {
	insertFn func(ctx context.Context, tm *domain.TenantMembership) (*domain.TenantMembership, error)
}

func (r *tsMembershipRepo) List(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
	return nil, nil
}
func (r *tsMembershipRepo) FindByUserID(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *tsMembershipRepo) Insert(ctx context.Context, tm *domain.TenantMembership) (*domain.TenantMembership, error) {
	if r.insertFn != nil {
		return r.insertFn(ctx, tm)
	}
	tm.ID = uuid.New()
	return tm, nil
}
func (r *tsMembershipRepo) SetStatus(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *tsMembershipRepo) SoftDelete(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil }
func (r *tsMembershipRepo) CountActive(context.Context, uuid.UUID) (int, error)           { return 0, nil }
func (r *tsMembershipRepo) ListActiveUserIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

var _ port.MembershipRepository = (*tsMembershipRepo)(nil)

type tsRoleRepo struct {
	grantFn func(ctx context.Context, tr *domain.TenantRole) (*domain.TenantRole, error)
}

func (r *tsRoleRepo) ListByUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *tsRoleRepo) ListByRole(context.Context, uuid.UUID, domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *tsRoleRepo) CountActiveOwners(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (r *tsRoleRepo) Grant(ctx context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
	if r.grantFn != nil {
		return r.grantFn(ctx, tr)
	}
	return tr, nil
}
func (r *tsRoleRepo) Revoke(context.Context, uuid.UUID, uuid.UUID, domain.TenantRoleCode) (*domain.TenantRole, error) {
	return nil, nil
}
func (r *tsRoleRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}

var _ port.TenantRoleRepository = (*tsRoleRepo)(nil)

// buildFullTrialSvc wires every collaborator TrialSignup touches, each
// independently overridable, with a happy-path default identical to
// buildTrialProvisioningSvc's. txRunner defaults to &arTxRunner{} (no
// publisher) unless overridden.
func buildFullTrialSvc(tenants port.TenantRepository, tenantDepts port.TenantDepartmentRepository,
	labels port.DeptRoleLabelRepository, mem port.MembershipRepository, roles port.TenantRoleRepository,
	catalog port.DepartmentCatalogReader, plans port.PlanCatalogReader, txRunner port.TxRunner,
) *service.ProvisioningService {
	return service.NewProvisioningService(
		tenants, mem, roles, nil, labels, tenantDepts, catalog, plans,
		txRunner, nil, nil,
	)
}

func onlyEngineeringCatalog() port.DepartmentCatalogReader {
	return &ptdCatalogDepts{departmentsFn: func(context.Context) ([]domain.Department, error) {
		return []domain.Department{sysDept("ENGINEERING", true)}, nil
	}}
}

// ── invalid plan validation ─────────────────────────────────────────────

func TestTrialSignup_InvalidPlan_ValidationError(t *testing.T) {
	svc := buildFullTrialSvc(&tsTenantRepo{}, &tsTenantDeptRepo{}, &tsLabelRepo{}, &tsMembershipRepo{}, &tsRoleRepo{},
		onlyEngineeringCatalog(), &ptdCatalogPlans{}, &arTxRunner{})
	in := trialSignupInput()
	in.Plan = domain.TenantPlan("bogus")

	_, _, err := svc.TrialSignup(context.Background(), in)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.ErrorIs(t, err, domain.ErrInvalidPlan)
}

// ── empty DefaultLocale defaults to en-US ───────────────────────────────

func TestTrialSignup_EmptyDefaultLocale_DefaultsToEnUS(t *testing.T) {
	var gotLocale string
	tenants := &tsTenantRepo{insertFn: func(_ context.Context, tn *domain.Tenant) (*domain.Tenant, bool, error) {
		gotLocale = tn.DefaultLocale
		return tn, true, nil
	}}
	svc := buildFullTrialSvc(tenants, &tsTenantDeptRepo{}, &tsLabelRepo{}, &tsMembershipRepo{}, &tsRoleRepo{},
		onlyEngineeringCatalog(), &ptdCatalogPlans{}, &arTxRunner{})
	in := trialSignupInput()
	in.DefaultLocale = ""

	_, _, err := svc.TrialSignup(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, "en-US", gotLocale)
}

// ── tx-step error propagation ───────────────────────────────────────────

func TestTrialSignup_TenantInsertErrorPropagates(t *testing.T) {
	insertErr := errors.New("uq_tenants_slug violation")
	tenants := &tsTenantRepo{insertFn: func(context.Context, *domain.Tenant) (*domain.Tenant, bool, error) {
		return nil, false, insertErr
	}}
	svc := buildFullTrialSvc(tenants, &tsTenantDeptRepo{}, &tsLabelRepo{}, &tsMembershipRepo{}, &tsRoleRepo{},
		onlyEngineeringCatalog(), &ptdCatalogPlans{}, &arTxRunner{})

	_, _, err := svc.TrialSignup(context.Background(), trialSignupInput())
	assert.ErrorIs(t, err, insertErr)
}

func TestTrialSignup_TenantDeptActivateErrorPropagates(t *testing.T) {
	activateErr := errors.New("fk_td_tenant violation")
	tenantDepts := &tsTenantDeptRepo{activateFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
		return nil, activateErr
	}}
	svc := buildFullTrialSvc(&tsTenantRepo{}, tenantDepts, &tsLabelRepo{}, &tsMembershipRepo{}, &tsRoleRepo{},
		onlyEngineeringCatalog(), &ptdCatalogPlans{}, &arTxRunner{})

	_, _, err := svc.TrialSignup(context.Background(), trialSignupInput())
	assert.ErrorIs(t, err, activateErr)
}

func TestTrialSignup_LabelsSeedErrorPropagates(t *testing.T) {
	seedErr := errors.New("db down")
	labels := &tsLabelRepo{seedFn: func(context.Context, uuid.UUID) ([]domain.DeptRoleLabel, error) {
		return nil, seedErr
	}}
	svc := buildFullTrialSvc(&tsTenantRepo{}, &tsTenantDeptRepo{}, labels, &tsMembershipRepo{}, &tsRoleRepo{},
		onlyEngineeringCatalog(), &ptdCatalogPlans{}, &arTxRunner{})

	_, _, err := svc.TrialSignup(context.Background(), trialSignupInput())
	assert.ErrorIs(t, err, seedErr)
}

func TestTrialSignup_MembershipInsertErrorPropagates(t *testing.T) {
	insertErr := errors.New("uq_tm_active_user violation")
	mem := &tsMembershipRepo{insertFn: func(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
		return nil, insertErr
	}}
	svc := buildFullTrialSvc(&tsTenantRepo{}, &tsTenantDeptRepo{}, &tsLabelRepo{}, mem, &tsRoleRepo{},
		onlyEngineeringCatalog(), &ptdCatalogPlans{}, &arTxRunner{})

	_, _, err := svc.TrialSignup(context.Background(), trialSignupInput())
	assert.ErrorIs(t, err, insertErr)
}

func TestTrialSignup_RoleGrantErrorPropagates(t *testing.T) {
	grantErr := errors.New("grant failed")
	roles := &tsRoleRepo{grantFn: func(context.Context, *domain.TenantRole) (*domain.TenantRole, error) {
		return nil, grantErr
	}}
	svc := buildFullTrialSvc(&tsTenantRepo{}, &tsTenantDeptRepo{}, &tsLabelRepo{}, &tsMembershipRepo{}, roles,
		onlyEngineeringCatalog(), &ptdCatalogPlans{}, &arTxRunner{})

	_, _, err := svc.TrialSignup(context.Background(), trialSignupInput())
	assert.ErrorIs(t, err, grantErr)
}

// ── event emission: live publisher carries all three events ────────────

func TestTrialSignup_Success_EmitsTenantCreatedTrialStartedAndRoleGranted(t *testing.T) {
	tenantID, ownerID := uuid.New(), uuid.New()
	pub := &arPub{}
	svc := buildFullTrialSvc(&tsTenantRepo{}, &tsTenantDeptRepo{}, &tsLabelRepo{}, &tsMembershipRepo{}, &tsRoleRepo{},
		onlyEngineeringCatalog(), &ptdCatalogPlans{}, &arTxRunner{pub: pub})
	in := trialSignupInput()
	in.TenantID = tenantID
	in.OwnerUserID = ownerID

	got, wasCreated, err := svc.TrialSignup(context.Background(), in)
	require.NoError(t, err)
	assert.True(t, wasCreated)
	require.NotNil(t, got)

	require.Len(t, pub.events, 3)
	assert.Equal(t, domain.EventTenantCreated, pub.events[0].Type)
	assert.Equal(t, domain.EventTrialStarted, pub.events[1].Type)
	assert.Equal(t, domain.EventTenantRoleGranted, pub.events[2].Type)

	rolePayload, ok := pub.events[2].Data.(domain.TenantRoleGrantedPayload)
	require.True(t, ok)
	assert.Equal(t, ownerID, rolePayload.UserID)
	assert.Equal(t, ownerID, rolePayload.ActorID, "LLD I-1: granted_by = owner_user_id itself")
	assert.Equal(t, domain.RoleTenantOwner, rolePayload.RoleCode)
}

// ── no publisher in context: TrialSignup still succeeds, no emission ───

func TestTrialSignup_NoEventPublisherInContext_SkipsEmissionButStillSucceeds(t *testing.T) {
	svc := buildFullTrialSvc(&tsTenantRepo{}, &tsTenantDeptRepo{}, &tsLabelRepo{}, &tsMembershipRepo{}, &tsRoleRepo{},
		onlyEngineeringCatalog(), &ptdCatalogPlans{}, &arTxRunner{})

	got, wasCreated, err := svc.TrialSignup(context.Background(), trialSignupInput())
	require.NoError(t, err)
	assert.True(t, wasCreated)
	require.NotNil(t, got)
}

// ── tx-runner-level failure propagates ──────────────────────────────────

func TestTrialSignup_TxRunnerErrorPropagates(t *testing.T) {
	txErr := errors.New("tx aborted")
	svc := buildFullTrialSvc(&tsTenantRepo{}, &tsTenantDeptRepo{}, &tsLabelRepo{}, &tsMembershipRepo{}, &tsRoleRepo{},
		onlyEngineeringCatalog(), &ptdCatalogPlans{}, &passthroughTxRunner{runErr: txErr})

	_, _, err := svc.TrialSignup(context.Background(), trialSignupInput())
	assert.ErrorIs(t, err, txErr)
}
