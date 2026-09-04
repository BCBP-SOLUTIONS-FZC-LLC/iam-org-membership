// Handler-layer coverage tests for InternalHandler (I-1/I-2/I-4/I-5/I-8/
// I-9/I-10/I-11/I-13/I-14/I-15). Complements memberships_coverage_test.go
// (which uses the nil-service + absorbPanic pattern to prove validation
// runs before any service call) with tests that wire REAL services so the
// handler's actual success/error-mapping lines — post-service-call JSON
// construction and HandleError branches — are exercised too.
package http

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// ── shared local fakes (prefix "ih") ────────────────────────────────────

// ihMembershipInsertRepo overrides Insert on top of happyMembershipRepo
// (whose default returns nil, nil — unusable wherever the caller
// dereferences the returned row, e.g. TrialSignup's mem.ID / AddFromRegister).
type ihMembershipInsertRepo struct {
	happyMembershipRepo
	insertFn func(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error)
}

func (r *ihMembershipInsertRepo) Insert(ctx context.Context, m *domain.TenantMembership) (*domain.TenantMembership, error) {
	if r.insertFn != nil {
		return r.insertFn(ctx, m)
	}
	m.ID = uuid.New()
	return m, nil
}

// ihPlanCatalogReader is a minimal port.PlanCatalogReader fake.
type ihPlanCatalogReader struct {
	planByCodeFn func(context.Context, domain.TenantPlan) (*domain.Plan, error)
}

func (f *ihPlanCatalogReader) Plans(context.Context) ([]domain.Plan, error) { return nil, nil }
func (f *ihPlanCatalogReader) PlanByCode(ctx context.Context, code domain.TenantPlan) (*domain.Plan, error) {
	if f.planByCodeFn != nil {
		return f.planByCodeFn(ctx, code)
	}
	return &domain.Plan{Code: code, TrialDurationDays: 14}, nil
}

var _ port.PlanCatalogReader = (*ihPlanCatalogReader)(nil)

// ihSetRealmFieldsErrRepo overrides SetRealmFields on top of happyTenantRepo
// (whose TenantRepositoryNoop default always succeeds).
type ihSetRealmFieldsErrRepo struct {
	happyTenantRepo
	err error
}

func (r *ihSetRealmFieldsErrRepo) SetRealmFields(context.Context, uuid.UUID, string, domain.RealmType, string, int64) error {
	return r.err
}

// buildFullProvisioningSvc wires every ProvisioningService collaborator
// with a working default so I-1/I-2/I-4/I-5 all reach their real success
// path without a nil-pointer panic. insertFn customizes the tenants.Insert
// outcome (fresh-create vs idempotent-replay) for I-1 tests.
func buildFullProvisioningSvc(tenants port.TenantRepository, mems port.MembershipRepository) *service.ProvisioningService {
	if mems == nil {
		mems = &ihMembershipInsertRepo{}
	}
	return service.NewProvisioningService(
		tenants, mems, &p28RoleRepo{}, &drhDeptMemRepo{}, &drhLabelRepo{},
		&drhTenantDeptRepo{}, &drhDeptRepo{}, &ihPlanCatalogReader{},
		happyTxRunner{}, happyCacheStub{}, &happyRPClient{},
	)
}

// ═════════════════════════════════════════════════════════════════════════
// I-1 · ProvisionTenant
// ═════════════════════════════════════════════════════════════════════════

// I1-ERR-01: service-layer validation error (slug too short) → 400,
// propagated through the handler's HandleError branch (no repo touched).
func TestProvisionTenant_ServiceValidationError_400(t *testing.T) {
	svc := buildFullProvisioningSvc(&happyTenantRepo{}, nil)
	h := &InternalHandler{provisioning: svc}

	tenantID, ownerID := uuid.New(), uuid.New()
	body := fmt.Sprintf(`{"tenant_id":"%s","slug":"ab","name":"Acme","plan":"pro","owner_user_id":"%s"}`, tenantID, ownerID)
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	h.ProvisionTenant(c)

	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// I1-H-01: fresh create → 201.
func TestProvisionTenant_FreshCreate_201(t *testing.T) {
	tenants := &happyTenantRepo{insertFn: func(_ context.Context, t *domain.Tenant) (*domain.Tenant, bool, error) {
		return t, true, nil
	}}
	svc := buildFullProvisioningSvc(tenants, nil)
	h := &InternalHandler{provisioning: svc}

	tenantID, ownerID := uuid.New(), uuid.New()
	body := fmt.Sprintf(`{"tenant_id":"%s","slug":"acme-co","name":"Acme","plan":"pro","owner_user_id":"%s"}`, tenantID, ownerID)
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	h.ProvisionTenant(c)

	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), tenantID.String())
}

// I1-H-02: idempotent replay (row already existed) → 200, not 201.
func TestProvisionTenant_IdempotentReplay_200(t *testing.T) {
	tenants := &happyTenantRepo{insertFn: func(_ context.Context, t *domain.Tenant) (*domain.Tenant, bool, error) {
		return t, false, nil
	}}
	svc := buildFullProvisioningSvc(tenants, nil)
	h := &InternalHandler{provisioning: svc}

	tenantID, ownerID := uuid.New(), uuid.New()
	body := fmt.Sprintf(`{"tenant_id":"%s","slug":"acme-co","name":"Acme","plan":"pro","owner_user_id":"%s"}`, tenantID, ownerID)
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	h.ProvisionTenant(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// ═════════════════════════════════════════════════════════════════════════
// I-2 · PatchTenantRealm
// ═════════════════════════════════════════════════════════════════════════

// I2-H-01: full happy path → 200.
func TestPatchTenantRealm_Success_200(t *testing.T) {
	svc := buildFullProvisioningSvc(&happyTenantRepo{}, nil)
	h := &InternalHandler{provisioning: svc}

	tenantID := uuid.New()
	body := `{"realm_id":"realm-1","realm_type":"dedicated","keycloak_shard":"shard-1","record_version":1}`
	c, w := buildCtx(http.MethodPatch, "/", body, systemCtx())
	setParams(c, "id", tenantID.String())
	h.PatchTenantRealm(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "realm-1")
}

// I2-ERR-01: repo returns optimistic-lock conflict → 409.
func TestPatchTenantRealm_ServiceError_409(t *testing.T) {
	tenants := &ihSetRealmFieldsErrRepo{err: domain.NewError(domain.ErrOptimisticLockConflict, "record_version mismatch")}
	svc := buildFullProvisioningSvc(tenants, nil)
	h := &InternalHandler{provisioning: svc}

	tenantID := uuid.New()
	body := `{"realm_id":"realm-1","realm_type":"dedicated","keycloak_shard":"shard-1","record_version":1}`
	c, w := buildCtx(http.MethodPatch, "/", body, systemCtx())
	setParams(c, "id", tenantID.String())
	h.PatchTenantRealm(c)

	assertErrorCode(t, w, http.StatusConflict, "optimistic_lock_conflict")
}

// ═════════════════════════════════════════════════════════════════════════
// I-4 · PatchMemberLifecycle
// ═════════════════════════════════════════════════════════════════════════

// I4-M-01: malformed tenant id path param → 400 invalid_uuid.
func TestPatchMemberLifecycle_InvalidTenantID_400(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"status":"active","record_version":1}`, systemCtx())
	setParams(c, "id", "not-a-uuid", "user_id", uuid.New().String())
	h.PatchMemberLifecycle(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// I4-M-02: malformed user_id path param → 400 invalid_uuid.
func TestPatchMemberLifecycle_InvalidUserID_400(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"status":"active","record_version":1}`, systemCtx())
	setParams(c, "id", uuid.New().String(), "user_id", "not-a-uuid")
	h.PatchMemberLifecycle(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// I4-M-03: malformed JSON body → 400 validation_error.
func TestPatchMemberLifecycle_MalformedBody_400(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"status":`, systemCtx())
	setParams(c, "id", uuid.New().String(), "user_id", uuid.New().String())
	h.PatchMemberLifecycle(c)
	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// I4-H-01: full happy path → 200.
func TestPatchMemberLifecycle_Success_200(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	mems := &mhMemRepo{setStatusFn: func(_ context.Context, tid, uid uuid.UUID, s domain.MembershipStatus, ver int64) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid, Status: s, RecordVersion: ver + 1}, nil
	}}
	svc := buildFullProvisioningSvc(&happyTenantRepo{}, mems)
	h := &InternalHandler{provisioning: svc}

	c, w := buildCtx(http.MethodPatch, "/", `{"status":"suspended","record_version":1}`, systemCtx())
	setParams(c, "id", tenantID.String(), "user_id", userID.String())
	h.PatchMemberLifecycle(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"status":"suspended"`)
}

// I4-ERR-01: repo returns optimistic-lock conflict → 409.
func TestPatchMemberLifecycle_ServiceError_409(t *testing.T) {
	mems := &mhMemRepo{setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
		return nil, domain.NewError(domain.ErrOptimisticLockConflict, "record_version mismatch")
	}}
	svc := buildFullProvisioningSvc(&happyTenantRepo{}, mems)
	h := &InternalHandler{provisioning: svc}

	c, w := buildCtx(http.MethodPatch, "/", `{"status":"active","record_version":9}`, systemCtx())
	setParams(c, "id", uuid.New().String(), "user_id", uuid.New().String())
	h.PatchMemberLifecycle(c)

	assertErrorCode(t, w, http.StatusConflict, "optimistic_lock_conflict")
}

// ═════════════════════════════════════════════════════════════════════════
// I-3 · AddMember (plain-add branch — full success, and the
// keycloak_user_id-defaults-to-user_id branch)
// ═════════════════════════════════════════════════════════════════════════

// I3-H-01: plain add (no matching pending invite) with keycloak_user_id
// omitted → defaults to user_id, and the handler renders 201.
func TestAddMember_PlainAdd_KeycloakIDDefaults_201(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	mems := &ihMembershipInsertRepo{}
	repo := &iahInviteRepo{}
	svc := service.NewInvitationService(repo, mems, nil, nil, &happyTenantRepo{}, &happyRPClient{}, happyCacheStub{}, happyTxRunner{}, nil, 7)
	h := &InternalHandler{invitation: svc}

	body := fmt.Sprintf(`{"user_id":"%s"}`, userID)
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String())
	h.AddMember(c)

	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), userID.String())
	assert.Contains(t, w.Body.String(), `"status":"active"`)
}

// ═════════════════════════════════════════════════════════════════════════
// I-10 · AssignFromGroups
// ═════════════════════════════════════════════════════════════════════════

// I10-H-01: empty groups → immediate no-op JITResult, 200.
func TestAssignFromGroups_EmptyGroups_200(t *testing.T) {
	svc := service.NewGroupMappingService(&mhMemRepo{}, &mhRoleRepo{}, &drhDeptMemRepo{}, happyTxRunner{}, happyCacheStub{}, nil)
	h := &InternalHandler{groupMappings: svc}

	tenantID := uuid.New()
	body := fmt.Sprintf(`{"user_id":"%s","groups":[]}`, uuid.New())
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String())
	h.AssignFromGroups(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// I10-H-02: non-empty groups with no configured GroupMappingClient (fails
// open, ADR-0007 Action Item 4) still reaches a successful empty-resolution
// 200 — exercises the full service-call + JSON-render lines.
func TestAssignFromGroups_NoClientConfigured_FailsOpen_200(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	mems := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid, Status: domain.MembershipActive}, nil
	}}
	svc := service.NewGroupMappingService(mems, &mhRoleRepo{}, &drhDeptMemRepo{}, happyTxRunner{}, happyCacheStub{}, nil)
	h := &InternalHandler{groupMappings: svc}

	body := fmt.Sprintf(`{"user_id":"%s","groups":["eng-leads"]}`, userID)
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String())
	h.AssignFromGroups(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// I10-ERR-01: user is not a member of the tenant → service error propagates.
func TestAssignFromGroups_MembershipLookupError(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	mems := &mhMemRepo{findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return nil, domain.NewError(domain.ErrMemberNotFound, "member not found")
	}}
	svc := service.NewGroupMappingService(mems, &mhRoleRepo{}, &drhDeptMemRepo{}, happyTxRunner{}, happyCacheStub{}, nil)
	h := &InternalHandler{groupMappings: svc}

	body := fmt.Sprintf(`{"user_id":"%s","groups":["eng-leads"]}`, userID)
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String())
	h.AssignFromGroups(c)

	assertErrorCode(t, w, http.StatusNotFound, "member_not_found")
}

// ═════════════════════════════════════════════════════════════════════════
// I-5 · DeleteMember
// ═════════════════════════════════════════════════════════════════════════

// I5-H-01: full happy path → 200 removed:true.
func TestDeleteMember_Success_200(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	mems := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid, Status: domain.MembershipActive, RecordVersion: 1}, nil
	}}
	svc := buildFullProvisioningSvc(&happyTenantRepo{}, mems)
	h := &InternalHandler{provisioning: svc}

	c, w := buildCtx(http.MethodDelete, "/", ``, systemCtx())
	setParams(c, "id", tenantID.String(), "user_id", userID.String())
	h.DeleteMember(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"removed":true`)
}

// I5-ERR-01: repo error (not ErrMemberNotFound) propagates.
func TestDeleteMember_ServiceError(t *testing.T) {
	mems := &mhMemRepo{findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return nil, domain.NewError(domain.ErrDBUnavailable, "db down")
	}}
	svc := buildFullProvisioningSvc(&happyTenantRepo{}, mems)
	h := &InternalHandler{provisioning: svc}

	c, w := buildCtx(http.MethodDelete, "/", ``, systemCtx())
	setParams(c, "id", uuid.New().String(), "user_id", uuid.New().String())
	h.DeleteMember(c)

	assertErrorCode(t, w, http.StatusServiceUnavailable, "db_unavailable")
}

// ═════════════════════════════════════════════════════════════════════════
// I-8 · GetMemberships (full service-call happy/error paths)
// ═════════════════════════════════════════════════════════════════════════

// ihAuthZRepo is a minimal port.AuthZRepository fake.
type ihAuthZRepo struct {
	findFn func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error)
}

func (f *ihAuthZRepo) FindMembershipProjection(ctx context.Context, tenantID, userID uuid.UUID) (*port.MembershipProjectionRow, error) {
	if f.findFn != nil {
		return f.findFn(ctx, tenantID, userID)
	}
	return nil, nil
}

// I8-H-FULL-01: full happy path → 200 with a real projection.
func TestGetMemberships_FullHappyPath_200(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	repo := &ihAuthZRepo{findFn: func(_ context.Context, tid, uid uuid.UUID) (*port.MembershipProjectionRow, error) {
		return &port.MembershipProjectionRow{
			MembershipStatus:   domain.MembershipActive,
			TenantPlan:         domain.PlanPro,
			SubscriptionStatus: domain.StatusActive,
			Locale:             "en-US",
		}, nil
	}}
	svc := service.NewAuthZService(repo, &ihPlanCatalogReader{}, nil, happyCacheStub{})
	h := &InternalHandler{authz: svc}

	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	h.GetMemberships(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "en-US")
}

// I8-ERR-FULL-01: no membership row → 404 member_not_found.
func TestGetMemberships_FullNotFound_404(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	repo := &ihAuthZRepo{}
	svc := service.NewAuthZService(repo, &ihPlanCatalogReader{}, nil, happyCacheStub{})
	h := &InternalHandler{authz: svc}

	c, w := buildCtx(http.MethodGet, "/?tenant_id="+tenantID.String(), "", systemCtx())
	setParams(c, "id", userID.String())
	h.GetMemberships(c)

	assertErrorCode(t, w, http.StatusNotFound, "member_not_found")
}

// ═════════════════════════════════════════════════════════════════════════
// I-9 · GetLocale
// ═════════════════════════════════════════════════════════════════════════

func TestGetLocale_Success_200(t *testing.T) {
	tenantID := uuid.New()
	tenants := &happyTenantRepo{findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
		return &domain.Tenant{ID: id, DefaultLocale: "en-US"}, nil
	}}
	svc := service.NewTenantService(tenants, happyCacheStub{}, &happyRPClient{})
	h := &InternalHandler{tenants: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", tenantID.String())
	h.GetLocale(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "en-US")
}

func TestGetLocale_NotFound_404(t *testing.T) {
	tenants := &happyTenantRepo{findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
		return nil, domain.NewError(domain.ErrTenantNotFound, "tenant not found")
	}}
	svc := service.NewTenantService(tenants, happyCacheStub{}, &happyRPClient{})
	h := &InternalHandler{tenants: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", uuid.New().String())
	h.GetLocale(c)

	assertErrorCode(t, w, http.StatusNotFound, "tenant_not_found")
}

// ═════════════════════════════════════════════════════════════════════════
// I-14 · GetMFAFreshness (0% baseline — fully untested)
// ═════════════════════════════════════════════════════════════════════════

// I14-M-01: malformed tenant id path param → 400 invalid_uuid.
func TestGetMFAFreshness_InvalidTenantID_400(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", "not-a-uuid")
	h.GetMFAFreshness(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// I14-H-01: full happy path → 200 with mfa_freshness_seconds.
func TestGetMFAFreshness_Success_200(t *testing.T) {
	tenantID := uuid.New()
	tenants := &happyTenantRepo{findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
		return &domain.Tenant{ID: id, MFAFreshnessSeconds: 300}, nil
	}}
	svc := service.NewTenantService(tenants, happyCacheStub{}, &happyRPClient{})
	h := &InternalHandler{tenants: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", tenantID.String())
	h.GetMFAFreshness(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"mfa_freshness_seconds":300`)
}

// I14-ERR-01: tenant not found → 404.
func TestGetMFAFreshness_NotFound_404(t *testing.T) {
	tenants := &happyTenantRepo{findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
		return nil, domain.NewError(domain.ErrTenantNotFound, "tenant not found")
	}}
	svc := service.NewTenantService(tenants, happyCacheStub{}, &happyRPClient{})
	h := &InternalHandler{tenants: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", uuid.New().String())
	h.GetMFAFreshness(c)

	assertErrorCode(t, w, http.StatusNotFound, "tenant_not_found")
}

// ═════════════════════════════════════════════════════════════════════════
// I-11 · GetSeatUsage
// ═════════════════════════════════════════════════════════════════════════

// I11-M-01: malformed tenant id path param → 400 invalid_uuid.
func TestGetSeatUsageInternal_InvalidTenantID_400(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", "not-a-uuid")
	h.GetSeatUsage(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// I11-H-01: full happy path → 200.
func TestGetSeatUsageInternal_Success_200(t *testing.T) {
	tenantID := uuid.New()
	overageSince := time.Now().UTC().Add(-24 * time.Hour)
	tenants := &happyTenantRepo{findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
		return &domain.Tenant{ID: id, LicensedSeats: 10, OverageSince: &overageSince}, nil
	}}
	mems := &mhMemRepo{countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 3, nil }}
	invs := &iahInviteRepo{countPendingFn: func(context.Context, uuid.UUID) (int, error) { return 1, nil }}
	svc := service.NewMembershipService(mems, &mhRoleRepo{}, &drhDeptMemRepo{}, tenants, invs, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &InternalHandler{membership: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", tenantID.String())
	h.GetSeatUsage(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"active_users":3`)
	assert.Contains(t, w.Body.String(), "overage_since")
	assert.Contains(t, w.Body.String(), "grace_ends_at")
}

// I11-ERR-01: tenant not found → 404.
func TestGetSeatUsageInternal_NotFound_404(t *testing.T) {
	tenants := &happyTenantRepo{findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
		return nil, domain.NewError(domain.ErrTenantNotFound, "tenant not found")
	}}
	svc := service.NewMembershipService(&mhMemRepo{}, &mhRoleRepo{}, &drhDeptMemRepo{}, tenants, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &InternalHandler{membership: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", uuid.New().String())
	h.GetSeatUsage(c)

	assertErrorCode(t, w, http.StatusNotFound, "tenant_not_found")
}

// ═════════════════════════════════════════════════════════════════════════
// I-15 · CheckMemberExists
// ═════════════════════════════════════════════════════════════════════════

// I15-H-01: no membership row (ErrMemberNotFound) → 200 active:false, never 404.
func TestCheckMemberExists_NotFound_ActiveFalse(t *testing.T) {
	mems := &mhMemRepo{findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return nil, domain.NewError(domain.ErrMemberNotFound, "member not found")
	}}
	svc := service.NewMembershipService(mems, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &InternalHandler{membership: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", uuid.New().String(), "user_id", uuid.New().String())
	h.CheckMemberExists(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"active":false`)
}

// I15-H-02: membership exists but suspended → 200 active:false.
func TestCheckMemberExists_Suspended_ActiveFalse(t *testing.T) {
	mems := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid, Status: domain.MembershipSuspended}, nil
	}}
	svc := service.NewMembershipService(mems, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &InternalHandler{membership: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", uuid.New().String(), "user_id", uuid.New().String())
	h.CheckMemberExists(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"active":false`)
}

// I15-H-03: active membership → 200 active:true with tenant_membership_id.
func TestCheckMemberExists_Active_ActiveTrue(t *testing.T) {
	membershipID := uuid.New()
	mems := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: membershipID, TenantID: tid, UserID: uid, Status: domain.MembershipActive}, nil
	}}
	svc := service.NewMembershipService(mems, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &InternalHandler{membership: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", uuid.New().String(), "user_id", uuid.New().String())
	h.CheckMemberExists(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"active":true`)
	assert.Contains(t, w.Body.String(), membershipID.String())
}

// I15-ERR-01: a genuine (non-not-found) repo error propagates via HandleError.
func TestCheckMemberExists_GenericError(t *testing.T) {
	mems := &mhMemRepo{findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return nil, domain.NewError(domain.ErrDBUnavailable, "db down")
	}}
	svc := service.NewMembershipService(mems, &mhRoleRepo{}, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &InternalHandler{membership: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", uuid.New().String(), "user_id", uuid.New().String())
	h.CheckMemberExists(c)

	assertErrorCode(t, w, http.StatusServiceUnavailable, "db_unavailable")
}

// ═════════════════════════════════════════════════════════════════════════
// I-13 · AssigneeOverride
// ═════════════════════════════════════════════════════════════════════════

// I13-M-00: malformed tenant id path param → 400 invalid_uuid.
func TestAssigneeOverride_InvalidTenantID_400(t *testing.T) {
	h := &InternalHandler{}
	body := fmt.Sprintf(`{"new_user_id":"%s","department_id":"%s","required_level":"approver","actor_id":"%s"}`,
		uuid.New(), uuid.New(), uuid.New())
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", "not-a-uuid", "tender_id", uuid.New().String())
	h.AssigneeOverride(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// I13-M-01: malformed tender_id path param → 400 invalid_uuid.
func TestAssigneeOverride_InvalidTenderID_400(t *testing.T) {
	h := &InternalHandler{}
	body := fmt.Sprintf(`{"new_user_id":"%s","department_id":"%s","required_level":"approver","actor_id":"%s"}`,
		uuid.New(), uuid.New(), uuid.New())
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", uuid.New().String(), "tender_id", "not-a-uuid")
	h.AssigneeOverride(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// I13-M-02: missing required field (new_user_id) → 400 validation_error.
func TestAssigneeOverride_MissingFields_400(t *testing.T) {
	h := &InternalHandler{}
	body := fmt.Sprintf(`{"department_id":"%s","required_level":"approver","actor_id":"%s"}`, uuid.New(), uuid.New())
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", uuid.New().String(), "tender_id", uuid.New().String())
	h.AssigneeOverride(c)
	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// I13-H-01: full happy path → 200 eligible:true.
func TestAssigneeOverride_Success_200(t *testing.T) {
	tenantID, tenderID, newUserID, deptID, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mems := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid, Status: domain.MembershipActive}, nil
	}}
	roles := &mhRoleRepo{listByUserFn: func(_ context.Context, tid, uid uuid.UUID) ([]domain.TenantRole, error) {
		if uid == actorID {
			return []domain.TenantRole{{TenantID: tid, UserID: uid, RoleCode: domain.RoleTenantAdmin}}, nil
		}
		return nil, nil
	}}
	deptMems := &drhDeptMemRepo{listByUserFn: func(_ context.Context, tid, uid uuid.UUID) ([]domain.DeptMembership, error) {
		return []domain.DeptMembership{{TenantID: tid, UserID: uid, DepartmentID: deptID, RoleLevel: domain.DeptApprover}}, nil
	}}
	svc := service.NewMembershipService(mems, roles, deptMems, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &InternalHandler{membership: svc}

	body := fmt.Sprintf(`{"new_user_id":"%s","department_id":"%s","required_level":"approver","actor_id":"%s"}`,
		newUserID, deptID, actorID)
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String(), "tender_id", tenderID.String())
	h.AssigneeOverride(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"eligible":true`)
}

// I13-ERR-01: actor lacks an elevated role → 403 insufficient_role.
func TestAssigneeOverride_ActorNotElevated_403(t *testing.T) {
	tenantID, tenderID, newUserID, deptID, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	roles := &mhRoleRepo{listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
		return nil, nil // no elevated roles for anyone
	}}
	svc := service.NewMembershipService(&mhMemRepo{}, roles, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30)
	h := &InternalHandler{membership: svc}

	body := fmt.Sprintf(`{"new_user_id":"%s","department_id":"%s","required_level":"approver","actor_id":"%s"}`,
		newUserID, deptID, actorID)
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String(), "tender_id", tenderID.String())
	h.AssigneeOverride(c)

	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}
