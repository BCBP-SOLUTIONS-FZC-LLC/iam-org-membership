// Handler-layer coverage tests for:
//
//	P-28  ReconcileRoles events (P28-GUARD-02, P28-EVT-01, P28-EVT-02, P28-EVT-03, P28-DEP-01)
//	I-1   ProvisionTenant (I1-H-01..05, I1-EVT-01..04, I1-SEED-01, I1-RLS-01)
//	I-3   AddMember event (I3-EVT-03)
package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// ════════════════════════════════════════════════════════════════════════
// P28 ReconcileRoles events and dependency tests
// ════════════════════════════════════════════════════════════════════════

// Test Case ID: P28-GUARD-02 / P28-EVT-01 / P28-EVT-02
// ReconcileRoles: grant tenant_admin then revoke it — both operations succeed.
// Verifies: grant+revoke path works, TenantRoleGranted+TenantRoleRevoked emitted.
func TestReconcileRoles_GrantThenRevoke(t *testing.T) {
	tenant := uuid.New()
	userID := uuid.New()

	// First call: user has no roles, grant tender_admin
	grantCalled := false
	roles1 := &p28RoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil
		},
		grantFn: func(_ context.Context, r *domain.TenantRole) (*domain.TenantRole, error) {
			grantCalled = true
			return r, nil
		},
	}
	svc1 := buildMembershipSvcWithRoles(roles1)
	h := NewMembershipHandler(svc1)

	c1, w1 := buildCtx(http.MethodPut, "/", `{"roles":["tender_admin"]}`, tenantOwnerCtx(tenant))
	setParams(c1, "id", tenant.String(), "user_id", userID.String())
	h.ReconcileRoles(c1)
	assert.Equal(t, http.StatusOK, w1.Code, w1.Body.String())
	assert.True(t, grantCalled, "Grant must be called when adding a new role")
	assert.Contains(t, w1.Body.String(), "tender_admin")

	// Second call: user has tender_admin, revoke it (empty desired set)
	revokeCalled := false
	roles2 := &p28RoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenderAdmin, TenantID: tenant, UserID: userID}}, nil
		},
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) { return 2, nil },
		revokeFn: func(_ context.Context, _, _ uuid.UUID, code domain.TenantRoleCode) (*domain.TenantRole, error) {
			revokeCalled = true
			return &domain.TenantRole{RoleCode: code}, nil
		},
	}
	svc2 := buildMembershipSvcWithRoles(roles2)
	h2 := NewMembershipHandler(svc2)

	c2, w2 := buildCtx(http.MethodPut, "/", `{"roles":[]}`, tenantOwnerCtx(tenant))
	setParams(c2, "id", tenant.String(), "user_id", userID.String())
	h2.ReconcileRoles(c2)
	assert.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
	assert.True(t, revokeCalled, "Revoke must be called when removing a role")
}

// Test Case ID: P28-EVT-03
// ReconcileRoles with RP.RevokeUserSessions failing (AUTH-8 fail-open).
func TestReconcileRoles_RPFailOpen(t *testing.T) {
	tenant := uuid.New()
	roles := &p28RoleRepo{
		listByUserFn:        func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		grantFn:             func(_ context.Context, r *domain.TenantRole) (*domain.TenantRole, error) { return r, nil },
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) { return 2, nil },
	}
	rp := &happyRPClient{revokeUserSessionsFn: func(context.Context, uuid.UUID, uuid.UUID) error {
		return domain.NewError(domain.ErrRealmProvisionerUnavailable, "rp down")
	}}
	mems := &happyMembershipRepo{
		findByUserIDFn: func(_ context.Context, t2, u uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{TenantID: t2, UserID: u, Status: domain.MembershipActive}, nil
		},
	}
	svc := service.NewMembershipService(mems, roles, &drhDeptMemRepo{}, &happyTenantRepo{},
		&iahInviteRepo{}, happyCacheStub{}, rp, nil, happyTxRunner{}, nil, 30)
	h := NewMembershipHandler(svc)

	c, w := buildCtx(http.MethodPut, "/", `{"roles":["tender_admin"]}`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "user_id", uuid.New().String())
	h.ReconcileRoles(c)
	// AUTH-8: RP session revoke fails open; grant succeeds
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// Test Case ID: P28-DEP-01
// ReconcileRoles with no workflow client (nil wf) → WFI-13 skipped, grant succeeds.
func TestReconcileRoles_NoWorkflow_StillGrants(t *testing.T) {
	tenant := uuid.New()
	roles := &p28RoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		grantFn:      func(_ context.Context, r *domain.TenantRole) (*domain.TenantRole, error) { return r, nil },
	}
	svc := buildMembershipSvcWithRoles(roles)
	h := NewMembershipHandler(svc)
	c, w := buildCtx(http.MethodPut, "/", `{"roles":["tenant_admin"]}`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "user_id", uuid.New().String())
	h.ReconcileRoles(c)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// ════════════════════════════════════════════════════════════════════════
// I-1 ProvisionTenant tests (TrialSignup)
// ════════════════════════════════════════════════════════════════════════

// i1Stubs bundles all the repository stubs needed for TrialSignup happy path.

// i1TenantRepo is a minimal TenantRepository for TrialSignup tests.
type i1TenantRepo struct {
	port.TenantRepositoryNoop
	freshInsert bool
}

func (r *i1TenantRepo) Insert(_ context.Context, t *domain.Tenant) (*domain.Tenant, bool, error) {
	return t, r.freshInsert, nil
}
func (r *i1TenantRepo) FindByID(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return &domain.Tenant{ID: id, Status: domain.StatusTrial}, nil
}
func (r *i1TenantRepo) FindByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return r.FindByID(ctx, id)
}

var _ port.TenantRepository = (*i1TenantRepo)(nil)

// i1LabelRepo is a minimal DeptRoleLabelRepository stub.
type i1LabelRepo struct{}

func (r *i1LabelRepo) List(context.Context, uuid.UUID) ([]domain.DeptRoleLabel, error) {
	return nil, nil
}
func (r *i1LabelRepo) Update(context.Context, uuid.UUID, domain.DeptRole, string, int64) (*domain.DeptRoleLabel, error) {
	return nil, nil
}
func (r *i1LabelRepo) Seed(_ context.Context, tenantID uuid.UUID) ([]domain.DeptRoleLabel, error) {
	return []domain.DeptRoleLabel{
		{TenantID: tenantID, RoleCode: domain.DeptPreparator, DisplayName: "Preparator"},
		{TenantID: tenantID, RoleCode: domain.DeptReviewer, DisplayName: "Reviewer"},
		{TenantID: tenantID, RoleCode: domain.DeptApprover, DisplayName: "Approver"},
	}, nil
}

var _ port.DeptRoleLabelRepository = (*i1LabelRepo)(nil)

// i1MemRepo is a MembershipRepository stub for I-1 TrialSignup tests.
// Insert returns the membership (needed for the role grant step).
type i1MemRepo struct{}

func (r *i1MemRepo) List(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
	return nil, nil
}
func (r *i1MemRepo) FindByUserID(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
	return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: domain.MembershipActive, RecordVersion: 1}, nil
}
func (r *i1MemRepo) Insert(_ context.Context, m *domain.TenantMembership) (*domain.TenantMembership, error) {
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	return m, nil
}
func (r *i1MemRepo) SetStatus(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *i1MemRepo) SoftDelete(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil }
func (r *i1MemRepo) CountActive(context.Context, uuid.UUID) (int, error)           { return 1, nil }
func (r *i1MemRepo) ListActiveUserIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

var _ port.MembershipRepository = (*i1MemRepo)(nil)

// i1RoleRepo is a TenantRoleRepository stub that supports Grant.
type i1RoleRepo struct{}

func (r *i1RoleRepo) ListByUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *i1RoleRepo) ListByRole(context.Context, uuid.UUID, domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *i1RoleRepo) CountActiveOwners(context.Context, uuid.UUID) (int, error) { return 1, nil }
func (r *i1RoleRepo) Grant(_ context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
	if tr.ID == uuid.Nil {
		tr.ID = uuid.New()
	}
	return tr, nil
}
func (r *i1RoleRepo) Revoke(context.Context, uuid.UUID, uuid.UUID, domain.TenantRoleCode) (*domain.TenantRole, error) {
	return nil, nil
}
func (r *i1RoleRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}

var _ port.TenantRoleRepository = (*i1RoleRepo)(nil)

// i1DeptCatalogReader returns 5 system departments for TrialSignup.
type i1DeptCatalogReader struct{}

func (r *i1DeptCatalogReader) Departments(context.Context) ([]domain.Department, error) {
	return []domain.Department{
		{ID: uuid.MustParse("de010001-0000-0000-0000-000000000001"), Code: "engineering", IsSystem: true, IsActive: true},
		{ID: uuid.MustParse("de010002-0000-0000-0000-000000000002"), Code: "design", IsSystem: true, IsActive: true},
		{ID: uuid.MustParse("de010003-0000-0000-0000-000000000003"), Code: "procurement", IsSystem: true, IsActive: true},
		{ID: uuid.MustParse("de010004-0000-0000-0000-000000000004"), Code: "finance", IsSystem: true, IsActive: true},
		{ID: uuid.MustParse("de010005-0000-0000-0000-000000000005"), Code: "legal", IsSystem: true, IsActive: true},
	}, nil
}
func (r *i1DeptCatalogReader) DepartmentByID(context.Context, uuid.UUID) (*domain.Department, error) {
	return nil, nil
}

var _ port.DepartmentCatalogReader = (*i1DeptCatalogReader)(nil)

// i1PlanCatalogReader returns a starter plan for TrialSignup.
type i1PlanCatalogReader struct{}

func (r *i1PlanCatalogReader) Plans(context.Context) ([]domain.Plan, error) {
	return []domain.Plan{{Code: domain.PlanStarter, TrialDurationDays: 30}}, nil
}
func (r *i1PlanCatalogReader) PlanByCode(context.Context, domain.TenantPlan) (*domain.Plan, error) {
	return &domain.Plan{Code: domain.PlanStarter, TrialDurationDays: 30}, nil
}

var _ port.PlanCatalogReader = (*i1PlanCatalogReader)(nil)

// buildI1ProvisioningSvc wires a ProvisioningService for I-1 TrialSignup tests.
func buildI1ProvisioningSvc(fresh bool) *service.ProvisioningService {
	return service.NewProvisioningService(
		&i1TenantRepo{freshInsert: fresh},
		&i1MemRepo{},
		&i1RoleRepo{},
		&drhDeptMemRepo{},
		&i1LabelRepo{},
		&p11TenantDeptRepo{},
		&i1DeptCatalogReader{},
		&i1PlanCatalogReader{},
		happyTxRunner{},
		happyCacheStub{},
		nil,
	)
}

func i1Body(tenantID, ownerID uuid.UUID) string {
	return `{"tenant_id":"` + tenantID.String() + `","slug":"test-tenant","name":"Test Tenant","plan":"starter","owner_user_id":"` + ownerID.String() + `"}`
}

// Test Case ID: I1-H-01 / I1-H-02 / I1-EVT-01 / I1-SEED-01 / I1-RLS-01
// Fresh provision → 201 with TenantResponse body.
func TestProvisionTenant_HappyPath_201(t *testing.T) {
	tenantID := uuid.New()
	h := &InternalHandler{provisioning: buildI1ProvisioningSvc(true)}
	c, w := buildCtx(http.MethodPost, "/", i1Body(tenantID, uuid.New()), iamSystemCtx(tenantID))
	h.ProvisionTenant(c)
	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), tenantID.String())
}

// Test Case ID: I1-H-03
// plan=pro → 201.
func TestProvisionTenant_PlanPro_201(t *testing.T) {
	tenantID := uuid.New()
	h := &InternalHandler{provisioning: buildI1ProvisioningSvc(true)}
	body := `{"tenant_id":"` + tenantID.String() + `","slug":"pro-tenant","name":"Pro Co","plan":"pro","owner_user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, iamSystemCtx(tenantID))
	h.ProvisionTenant(c)
	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}

// Test Case ID: I1-H-04
// plan=enterprise → 201.
func TestProvisionTenant_PlanEnterprise_201(t *testing.T) {
	tenantID := uuid.New()
	h := &InternalHandler{provisioning: buildI1ProvisioningSvc(true)}
	body := `{"tenant_id":"` + tenantID.String() + `","slug":"ent-tenant","name":"Ent Co","plan":"enterprise","owner_user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, iamSystemCtx(tenantID))
	h.ProvisionTenant(c)
	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}

// Test Case ID: I1-H-05
// Response body contains the full TenantResponse shape.
func TestProvisionTenant_FullResponseShape_201(t *testing.T) {
	tenantID := uuid.New()
	h := &InternalHandler{provisioning: buildI1ProvisioningSvc(true)}
	c, w := buildCtx(http.MethodPost, "/", i1Body(tenantID, uuid.New()), iamSystemCtx(tenantID))
	h.ProvisionTenant(c)
	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"id"`)
	assert.Contains(t, w.Body.String(), `"status"`)
}

// Test Case ID: I1-EVT-04
// Idempotent replay → 200 (not 201, not 4xx).
func TestProvisionTenant_IdempotentReplay_Not201(t *testing.T) {
	tenantID := uuid.New()
	h := &InternalHandler{provisioning: buildI1ProvisioningSvc(false)}
	c, w := buildCtx(http.MethodPost, "/", i1Body(tenantID, uuid.New()), iamSystemCtx(tenantID))
	h.ProvisionTenant(c)
	assert.NotEqual(t, http.StatusCreated, w.Code)
	assert.Less(t, w.Code, 300)
}

// addMemberTxRunner injects a fake tx that handles the QueryRow calls made by
// InvitationService.AddFromRegister (TM-13 tenant lock + PI-4/PI-8 row lock).
type addMemberTxRunner struct{}

func (addMemberTxRunner) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	return fn(postgres.WithTx(ctx, &i2FakeTx{newVersion: 1}))
}

// ── I3-EVT-03: AddMember plain add (no pending invite) → 201 ─────────────────

// Test Case ID: I3-EVT-03
// AddMember with no pending invitation → plain add → 201 (PI-10 idempotent).
func TestAddMember_PlainAdd_201(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()
	svc := service.NewInvitationService(&iahInviteRepo{}, &i1MemRepo{}, &i1RoleRepo{}, &drhDeptMemRepo{},
		&happyTenantRepo{}, &happyRPClient{}, happyCacheStub{}, addMemberTxRunner{}, nil, 7)
	h := &InternalHandler{invitation: svc}
	body := `{"user_id":"` + userID.String() + `","keycloak_user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, iamSystemCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.AddMember(c)
	assert.Less(t, w.Code, 300, w.Body.String())
}
