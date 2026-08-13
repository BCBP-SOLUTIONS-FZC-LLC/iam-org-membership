// Phase 19 — Handler happy-path coverage for
// InvitationHandler, ACLHandler, GroupMappingHandler.
package http

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// ── LOCAL fakes (prefix "iah") ────────────────────────────────────────

type iahInviteRepo struct {
	listFn                      func(context.Context, uuid.UUID) ([]domain.PendingInvitation, error)
	findByIDFn                  func(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error)
	findPendingByEmailFn        func(context.Context, uuid.UUID, string) (*domain.PendingInvitation, error)
	findPendingByKeycloakUserFn func(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error)
	insertFn                    func(context.Context, *domain.PendingInvitation) (*domain.PendingInvitation, error)
	setKeycloakUserIDFn         func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int64) error
	setStatusFn                 func(context.Context, uuid.UUID, uuid.UUID, domain.InvitationStatus, int64) (*domain.PendingInvitation, error)
	setKCCleanupPendingFn       func(context.Context, uuid.UUID, uuid.UUID, bool, int64) error
	countPendingFn              func(context.Context, uuid.UUID) (int, error)
	listExpiringFn              func(context.Context, time.Time, int) ([]domain.PendingInvitation, error)
	listPendingKCCleanupFn      func(context.Context, int) ([]domain.PendingInvitation, error)
}

func (f *iahInviteRepo) List(ctx context.Context, tid uuid.UUID) ([]domain.PendingInvitation, error) {
	if f.listFn != nil {
		return f.listFn(ctx, tid)
	}
	return nil, nil
}
func (f *iahInviteRepo) FindByID(ctx context.Context, tid, id uuid.UUID) (*domain.PendingInvitation, error) {
	if f.findByIDFn != nil {
		return f.findByIDFn(ctx, tid, id)
	}
	return nil, nil
}
func (f *iahInviteRepo) FindPendingByEmail(ctx context.Context, tid uuid.UUID, email string) (*domain.PendingInvitation, error) {
	if f.findPendingByEmailFn != nil {
		return f.findPendingByEmailFn(ctx, tid, email)
	}
	return nil, nil
}
func (f *iahInviteRepo) FindPendingByKeycloakUser(ctx context.Context, tid, kcID uuid.UUID) (*domain.PendingInvitation, error) {
	if f.findPendingByKeycloakUserFn != nil {
		return f.findPendingByKeycloakUserFn(ctx, tid, kcID)
	}
	return nil, nil
}
func (f *iahInviteRepo) Insert(ctx context.Context, inv *domain.PendingInvitation) (*domain.PendingInvitation, error) {
	if f.insertFn != nil {
		return f.insertFn(ctx, inv)
	}
	inv.ID = uuid.New()
	return inv, nil
}
func (f *iahInviteRepo) SetKeycloakUserID(ctx context.Context, tid, id, kcID uuid.UUID, ver int64) error {
	if f.setKeycloakUserIDFn != nil {
		return f.setKeycloakUserIDFn(ctx, tid, id, kcID, ver)
	}
	return nil
}
func (f *iahInviteRepo) SetStatus(ctx context.Context, tid, id uuid.UUID, s domain.InvitationStatus, ver int64) (*domain.PendingInvitation, error) {
	if f.setStatusFn != nil {
		return f.setStatusFn(ctx, tid, id, s, ver)
	}
	return &domain.PendingInvitation{ID: id, TenantID: tid, Status: s, RecordVersion: ver + 1}, nil
}
func (f *iahInviteRepo) SetKCCleanupPending(ctx context.Context, tid, id uuid.UUID, p bool, ver int64) error {
	if f.setKCCleanupPendingFn != nil {
		return f.setKCCleanupPendingFn(ctx, tid, id, p, ver)
	}
	return nil
}
func (f *iahInviteRepo) CountPending(ctx context.Context, tid uuid.UUID) (int, error) {
	if f.countPendingFn != nil {
		return f.countPendingFn(ctx, tid)
	}
	return 0, nil
}
func (f *iahInviteRepo) ListExpiring(ctx context.Context, t time.Time, l int) ([]domain.PendingInvitation, error) {
	if f.listExpiringFn != nil {
		return f.listExpiringFn(ctx, t, l)
	}
	return nil, nil
}
func (f *iahInviteRepo) ListPendingKCCleanup(ctx context.Context, l int) ([]domain.PendingInvitation, error) {
	if f.listPendingKCCleanupFn != nil {
		return f.listPendingKCCleanupFn(ctx, l)
	}
	return nil, nil
}
func (f *iahInviteRepo) MostRecentCreatedAt(context.Context, uuid.UUID, string) (time.Time, error) {
	return time.Time{}, nil // no prior invite — cooldown passes in tests
}
func (f *iahInviteRepo) CountCreatedInWindow(context.Context, uuid.UUID, time.Time) (int, error) {
	return 0, nil // no invites in window — rate limit passes in tests
}

var _ port.InvitationRepository = (*iahInviteRepo)(nil)

type iahACLRepo struct {
	listByTenderFn      func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenderACLEntry, error)
	findActiveForUserFn func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.TenderACLEntry, error)
	grantFn             func(context.Context, *domain.TenderACLEntry) (*domain.TenderACLEntry, error)
	revokeFn            func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.TenderACLEntry, error)
	softDeleteForUserFn func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenderACLEntry, error)
}

func (f *iahACLRepo) ListByTender(ctx context.Context, tid, teID uuid.UUID) ([]domain.TenderACLEntry, error) {
	if f.listByTenderFn != nil {
		return f.listByTenderFn(ctx, tid, teID)
	}
	return nil, nil
}
func (f *iahACLRepo) FindActiveForUser(ctx context.Context, tid, teID, uid uuid.UUID) (*domain.TenderACLEntry, error) {
	if f.findActiveForUserFn != nil {
		return f.findActiveForUserFn(ctx, tid, teID, uid)
	}
	return nil, nil
}
func (f *iahACLRepo) Grant(ctx context.Context, e *domain.TenderACLEntry) (*domain.TenderACLEntry, error) {
	if f.grantFn != nil {
		return f.grantFn(ctx, e)
	}
	e.ID = uuid.New()
	return e, nil
}
func (f *iahACLRepo) Revoke(ctx context.Context, tid, teID, uid uuid.UUID) (*domain.TenderACLEntry, error) {
	if f.revokeFn != nil {
		return f.revokeFn(ctx, tid, teID, uid)
	}
	return &domain.TenderACLEntry{TenantID: tid, TenderID: teID, UserID: uid}, nil
}
func (f *iahACLRepo) SoftDeleteForUser(ctx context.Context, tid, uid uuid.UUID) ([]domain.TenderACLEntry, error) {
	if f.softDeleteForUserFn != nil {
		return f.softDeleteForUserFn(ctx, tid, uid)
	}
	return nil, nil
}

var _ port.TenderACLRepository = (*iahACLRepo)(nil)

type iahGMRepo struct {
	listDeptRoleFn    func(context.Context, uuid.UUID) ([]domain.GroupDeptRoleMapping, error)
	replaceDeptRoleFn func(context.Context, uuid.UUID, []domain.GroupDeptRoleMapping) ([]domain.GroupDeptRoleMapping, error)

	listTRFn    func(context.Context, uuid.UUID) ([]domain.GroupTenantRoleMapping, error)
	replaceTRFn func(context.Context, uuid.UUID, []domain.GroupTenantRoleMapping) ([]domain.GroupTenantRoleMapping, error)

	listDeptFn    func(context.Context, uuid.UUID) ([]domain.GroupDeptMapping, error)
	replaceDeptFn func(context.Context, uuid.UUID, []domain.GroupDeptMapping) ([]domain.GroupDeptMapping, error)
}

func (f *iahGMRepo) ListDeptRoleMappings(ctx context.Context, tid uuid.UUID) ([]domain.GroupDeptRoleMapping, error) {
	if f.listDeptRoleFn != nil {
		return f.listDeptRoleFn(ctx, tid)
	}
	return nil, nil
}
func (f *iahGMRepo) ReplaceDeptRoleMappings(ctx context.Context, tid uuid.UUID, d []domain.GroupDeptRoleMapping) ([]domain.GroupDeptRoleMapping, error) {
	if f.replaceDeptRoleFn != nil {
		return f.replaceDeptRoleFn(ctx, tid, d)
	}
	return d, nil
}
func (f *iahGMRepo) ListTenantRoleMappings(ctx context.Context, tid uuid.UUID) ([]domain.GroupTenantRoleMapping, error) {
	if f.listTRFn != nil {
		return f.listTRFn(ctx, tid)
	}
	return nil, nil
}
func (f *iahGMRepo) ReplaceTenantRoleMappings(ctx context.Context, tid uuid.UUID, d []domain.GroupTenantRoleMapping) ([]domain.GroupTenantRoleMapping, error) {
	if f.replaceTRFn != nil {
		return f.replaceTRFn(ctx, tid, d)
	}
	return d, nil
}
func (f *iahGMRepo) ListDeptMappings(ctx context.Context, tid uuid.UUID) ([]domain.GroupDeptMapping, error) {
	if f.listDeptFn != nil {
		return f.listDeptFn(ctx, tid)
	}
	return nil, nil
}
func (f *iahGMRepo) ReplaceDeptMappings(ctx context.Context, tid uuid.UUID, d []domain.GroupDeptMapping) ([]domain.GroupDeptMapping, error) {
	if f.replaceDeptFn != nil {
		return f.replaceDeptFn(ctx, tid, d)
	}
	return d, nil
}

var _ port.GroupMappingRepository = (*iahGMRepo)(nil)

// The invitation service also needs a TenantRepo, RP, TxRunner, and members repo.
// We re-use the fakes defined in tenant_delegation_happy_test.go for tenant + RP + membership,
// as they live in the same package.

// ═════════════════════════════════════════════════════════════════════════
// P-30 · InvitationHandler.List
// ═════════════════════════════════════════════════════════════════════════

func TestInvitationList_Success_200(t *testing.T) {
	tenantID := uuid.New()
	repo := &iahInviteRepo{listFn: func(_ context.Context, tid uuid.UUID) ([]domain.PendingInvitation, error) {
		return []domain.PendingInvitation{
			{ID: uuid.New(), TenantID: tid, Email: "a@x.com", FullName: "A", Status: domain.InvitePending},
		}, nil
	}}
	svc := service.NewInvitationService(repo, &happyMembershipRepo{}, nil, nil, nil, &happyRPClient{}, happyCacheStub{}, happyTxRunner{}, nil, 7)
	h := &InvitationHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.List(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "a@x.com")
}

func TestInvitationList_RepoError(t *testing.T) {
	tenantID := uuid.New()
	repo := &iahInviteRepo{listFn: func(context.Context, uuid.UUID) ([]domain.PendingInvitation, error) {
		return nil, errors.New("db down")
	}}
	svc := service.NewInvitationService(repo, &happyMembershipRepo{}, nil, nil, nil, &happyRPClient{}, happyCacheStub{}, happyTxRunner{}, nil, 7)
	h := &InvitationHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.List(c)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// ═════════════════════════════════════════════════════════════════════════
// P-6 · InvitationHandler.Invite — validation path (early return)
// ═════════════════════════════════════════════════════════════════════════

func TestInvitationInvite_EmptyEmail(t *testing.T) {
	tenantID := uuid.New()
	svc := service.NewInvitationService(&iahInviteRepo{}, &happyMembershipRepo{}, nil, nil, &happyTenantRepo{}, &happyRPClient{}, happyCacheStub{}, happyTxRunner{}, nil, 7)
	h := &InvitationHandler{svc: svc}

	body := `{"email":"","full_name":"A","initial_tenant_roles":["tenant_admin"]}`
	c, w := buildCtx(http.MethodPost, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.Invite(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestInvitationInvite_DuplicateEmail(t *testing.T) {
	tenantID := uuid.New()
	repo := &iahInviteRepo{findPendingByEmailFn: func(_ context.Context, tid uuid.UUID, e string) (*domain.PendingInvitation, error) {
		return &domain.PendingInvitation{ID: uuid.New(), TenantID: tid, Email: e, Status: domain.InvitePending}, nil
	}}
	svc := service.NewInvitationService(repo, &happyMembershipRepo{}, nil, nil, &happyTenantRepo{}, &happyRPClient{}, happyCacheStub{}, happyTxRunner{}, nil, 7)
	h := &InvitationHandler{svc: svc}

	body := `{"email":"dup@x.com","full_name":"A","initial_tenant_roles":["tenant_admin"]}`
	c, w := buildCtx(http.MethodPost, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.Invite(c)

	assert.Equal(t, http.StatusConflict, w.Code)
}

// ═════════════════════════════════════════════════════════════════════════
// P-31 · InvitationHandler.Revoke
// ═════════════════════════════════════════════════════════════════════════

func TestInvitationRevoke_Success_204(t *testing.T) {
	tenantID := uuid.New()
	invID := uuid.New()
	svc := service.NewInvitationService(&iahInviteRepo{}, &happyMembershipRepo{}, nil, nil, nil, &happyRPClient{}, happyCacheStub{}, happyTxRunner{}, nil, 7)
	h := &InvitationHandler{svc: svc}

	body := `{"record_version":1}`
	c, w := buildCtx(http.MethodDelete, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "invitation_id", invID.String())
	h.Revoke(c)

	// Gin's c.Status(204) writes to c.Writer.Status(); httptest.ResponseRecorder
	// only sees the code once the header is flushed.
	assert.Equal(t, http.StatusNoContent, c.Writer.Status(), w.Body.String())
}

func TestInvitationRevoke_LegacyQueryStringVersion_204(t *testing.T) {
	tenantID := uuid.New()
	invID := uuid.New()
	svc := service.NewInvitationService(&iahInviteRepo{}, &happyMembershipRepo{}, nil, nil, nil, &happyRPClient{}, happyCacheStub{}, happyTxRunner{}, nil, 7)
	h := &InvitationHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "invitation_id", invID.String())
	c.Request.URL.RawQuery = "record_version=1"
	h.Revoke(c)

	assert.Equal(t, http.StatusNoContent, c.Writer.Status(), w.Body.String())
}

func TestInvitationRevoke_OptimisticLock(t *testing.T) {
	tenantID := uuid.New()
	invID := uuid.New()
	repo := &iahInviteRepo{setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.InvitationStatus, int64) (*domain.PendingInvitation, error) {
		return nil, domain.NewError(domain.ErrOptimisticLockConflict, "record_version mismatch")
	}}
	svc := service.NewInvitationService(repo, &happyMembershipRepo{}, nil, nil, nil, &happyRPClient{}, happyCacheStub{}, happyTxRunner{}, nil, 7)
	h := &InvitationHandler{svc: svc}

	body := `{"record_version":99}`
	c, w := buildCtx(http.MethodDelete, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "invitation_id", invID.String())
	h.Revoke(c)

	assert.Equal(t, http.StatusConflict, w.Code)
}

// ═════════════════════════════════════════════════════════════════════════
// P-21 · ACLHandler.List
// ═════════════════════════════════════════════════════════════════════════

func TestACLList_Success_200(t *testing.T) {
	tenantID := uuid.New()
	tenderID := uuid.New()
	userA := uuid.New()
	repo := &iahACLRepo{listByTenderFn: func(_ context.Context, tid, teID uuid.UUID) ([]domain.TenderACLEntry, error) {
		return []domain.TenderACLEntry{
			{TenantID: tid, TenderID: teID, UserID: userA, AccessLevel: domain.ACLView, GrantedBy: uuid.New()},
		}, nil
	}}
	svc := service.NewTenderACLService(repo, &happyMembershipRepo{})
	h := &ACLHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "tender_id", tenderID.String())
	h.List(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "view")
}

// ═════════════════════════════════════════════════════════════════════════
// P-22 · ACLHandler.Grant
// ═════════════════════════════════════════════════════════════════════════

func TestACLGrant_Success_201(t *testing.T) {
	tenantID := uuid.New()
	tenderID := uuid.New()
	userID := uuid.New()
	repo := &iahACLRepo{grantFn: func(_ context.Context, e *domain.TenderACLEntry) (*domain.TenderACLEntry, error) {
		e.ID = uuid.New()
		return e, nil
	}}
	m := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid, Status: domain.MembershipActive}, nil
	}}
	svc := service.NewTenderACLService(repo, m)
	h := &ACLHandler{svc: svc}

	body := `{"user_id":"` + userID.String() + `","access_level":"view","reason":"stakeholder"}`
	c, w := buildCtx(http.MethodPost, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "tender_id", tenderID.String())
	h.Grant(c)

	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "view")
}

func TestACLGrant_InvalidLevel(t *testing.T) {
	tenantID := uuid.New()
	svc := service.NewTenderACLService(&iahACLRepo{}, &happyMembershipRepo{})
	h := &ACLHandler{svc: svc}

	body := `{"user_id":"` + uuid.New().String() + `","access_level":"emperor"}`
	c, w := buildCtx(http.MethodPost, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "tender_id", uuid.New().String())
	h.Grant(c)

	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestACLGrant_ExpiresInPast(t *testing.T) {
	tenantID := uuid.New()
	svc := service.NewTenderACLService(&iahACLRepo{}, &happyMembershipRepo{})
	h := &ACLHandler{svc: svc}

	past := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	body := `{"user_id":"` + uuid.New().String() + `","access_level":"view","expires_at":"` + past + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "tender_id", uuid.New().String())
	h.Grant(c)

	// ErrInvalidExpiresAt maps to 422 per §17 error taxonomy.
	assert.Contains(t, []int{http.StatusUnprocessableEntity, http.StatusBadRequest}, w.Code, w.Body.String())
}

// ═════════════════════════════════════════════════════════════════════════
// P-23 · ACLHandler.Revoke
// ═════════════════════════════════════════════════════════════════════════

func TestACLRevoke_Success_200(t *testing.T) {
	tenantID := uuid.New()
	tenderID := uuid.New()
	userID := uuid.New()
	repo := &iahACLRepo{revokeFn: func(_ context.Context, tid, teID, uid uuid.UUID) (*domain.TenderACLEntry, error) {
		return &domain.TenderACLEntry{TenantID: tid, TenderID: teID, UserID: uid}, nil
	}}
	svc := service.NewTenderACLService(repo, &happyMembershipRepo{})
	h := &ACLHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "tender_id", tenderID.String(), "user_id", userID.String())
	h.Revoke(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"revoked":true`)
}

// ═════════════════════════════════════════════════════════════════════════
// P-14 · GroupMappingHandler.ListDeptRole
// ═════════════════════════════════════════════════════════════════════════

func TestGM_ListDeptRole_Success_200(t *testing.T) {
	tenantID := uuid.New()
	repo := &iahGMRepo{listDeptRoleFn: func(_ context.Context, tid uuid.UUID) ([]domain.GroupDeptRoleMapping, error) {
		return []domain.GroupDeptRoleMapping{
			{TenantID: tid, KeycloakGroupName: "engineers", RoleCode: domain.DeptApprover},
		}, nil
	}}
	svc := service.NewGroupMappingService(repo, &happyMembershipRepo{}, nil, nil, nil, happyTxRunner{}, happyCacheStub{})
	h := &GroupMappingHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.ListDeptRole(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "engineers")
}

// ═════════════════════════════════════════════════════════════════════════
// P-15 · GroupMappingHandler.PutDeptRole
// ═════════════════════════════════════════════════════════════════════════

func TestGM_PutDeptRole_Success_200(t *testing.T) {
	tenantID := uuid.New()
	repo := &iahGMRepo{replaceDeptRoleFn: func(_ context.Context, tid uuid.UUID, d []domain.GroupDeptRoleMapping) ([]domain.GroupDeptRoleMapping, error) {
		return d, nil
	}}
	svc := service.NewGroupMappingService(repo, &happyMembershipRepo{}, nil, nil, nil, happyTxRunner{}, happyCacheStub{})
	h := &GroupMappingHandler{svc: svc}

	body := `{"mappings":[{"keycloak_group_name":"eng","role_code":"approver"}]}`
	c, w := buildCtx(http.MethodPut, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.PutDeptRole(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "approver")
}

// ═════════════════════════════════════════════════════════════════════════
// P-16 · GroupMappingHandler.ListDept
// ═════════════════════════════════════════════════════════════════════════

func TestGM_ListDept_Success_200(t *testing.T) {
	tenantID := uuid.New()
	deptA := uuid.New()
	repo := &iahGMRepo{listDeptFn: func(_ context.Context, tid uuid.UUID) ([]domain.GroupDeptMapping, error) {
		return []domain.GroupDeptMapping{
			{TenantID: tid, KeycloakGroupName: "engineers", DepartmentID: deptA},
		}, nil
	}}
	svc := service.NewGroupMappingService(repo, &happyMembershipRepo{}, nil, nil, nil, happyTxRunner{}, happyCacheStub{})
	h := &GroupMappingHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.ListDept(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "engineers")
}

// ═════════════════════════════════════════════════════════════════════════
// P-17 · GroupMappingHandler.PutDept
// ═════════════════════════════════════════════════════════════════════════

func TestGM_PutDept_Success_200(t *testing.T) {
	tenantID := uuid.New()
	deptID := uuid.New()
	repo := &iahGMRepo{replaceDeptFn: func(_ context.Context, tid uuid.UUID, d []domain.GroupDeptMapping) ([]domain.GroupDeptMapping, error) {
		return d, nil
	}}
	svc := service.NewGroupMappingService(repo, &happyMembershipRepo{}, nil, nil, nil, happyTxRunner{}, happyCacheStub{})
	h := &GroupMappingHandler{svc: svc}

	body := `{"mappings":[{"keycloak_group_name":"eng","department_id":"` + deptID.String() + `"}]}`
	c, w := buildCtx(http.MethodPut, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.PutDept(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// ═════════════════════════════════════════════════════════════════════════
// P-29 · GroupMappingHandler.PutTenantRole (§16 A25)
// ═════════════════════════════════════════════════════════════════════════

func TestGM_PutTenantRole_Success_200(t *testing.T) {
	tenantID := uuid.New()
	repo := &iahGMRepo{replaceTRFn: func(_ context.Context, tid uuid.UUID, d []domain.GroupTenantRoleMapping) ([]domain.GroupTenantRoleMapping, error) {
		return d, nil
	}}
	svc := service.NewGroupMappingService(repo, &happyMembershipRepo{}, nil, nil, nil, happyTxRunner{}, happyCacheStub{})
	h := &GroupMappingHandler{svc: svc}

	body := `{"mappings":[{"keycloak_group_name":"admins","role_code":"tenant_admin"}]}`
	c, w := buildCtx(http.MethodPut, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.PutTenantRole(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "tenant_admin")
}
