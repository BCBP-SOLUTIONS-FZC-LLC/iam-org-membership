// Phase 19 — Handler happy-path coverage for InvitationHandler. (ACLHandler
// coverage retired ADR-0007 Wave 3 Phase 6.)
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
	port.InvitationRepositoryNoop
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

// P-21/P-22/P-23 (ACLHandler) — retired ADR-0007 Wave 3 Phase 6, moved to
// iam-tender-acl's TAC-1/2/3. IDs never reused.

// P-14/P-15/P-16/P-17/P-29 (GroupMappingHandler) — retired, moved to Group
// Mapping Service. IDs never reused.
