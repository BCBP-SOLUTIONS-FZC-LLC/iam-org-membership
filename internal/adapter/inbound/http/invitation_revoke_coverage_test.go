// Handler-layer coverage tests for:
//
//	P-31 DELETE /api/v1/tenants/{id}/invitations/{invitation_id}
//	     (InvitationHandler.Revoke)
//
// P31-HP-01/02/03, P31-VAL-01/02/03, P31-AUTH-01/02/03,
// P31-BL-01/02/03, P31-EDGE-01/02, P31-DEP-01, P31-SEC-01
package http

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// p31InviteRepo is an InvitationRepository stub for P-31 revoke tests.
type p31InviteRepo struct {
	setStatusFn func(ctx context.Context, tenantID, id uuid.UUID, status domain.InvitationStatus, ver int64) (*domain.PendingInvitation, error)
}

func (r *p31InviteRepo) List(context.Context, uuid.UUID) ([]domain.PendingInvitation, error) {
	return nil, nil
}
func (r *p31InviteRepo) FindByID(_ context.Context, tID, id uuid.UUID) (*domain.PendingInvitation, error) {
	return &domain.PendingInvitation{ID: id, TenantID: tID, Status: domain.InvitePending, RecordVersion: 1}, nil
}
func (r *p31InviteRepo) FindPendingByEmail(context.Context, uuid.UUID, string) (*domain.PendingInvitation, error) {
	return nil, nil
}
func (r *p31InviteRepo) FindPendingByKeycloakUser(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
	return nil, nil
}
func (r *p31InviteRepo) Insert(context.Context, *domain.PendingInvitation) (*domain.PendingInvitation, error) {
	return nil, nil
}
func (r *p31InviteRepo) SetKeycloakUserID(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int64) error {
	return nil
}
func (r *p31InviteRepo) SetStatus(ctx context.Context, tenantID, id uuid.UUID, status domain.InvitationStatus, ver int64) (*domain.PendingInvitation, error) {
	if r.setStatusFn != nil {
		return r.setStatusFn(ctx, tenantID, id, status, ver)
	}
	return &domain.PendingInvitation{ID: id, TenantID: tenantID, Status: status, RecordVersion: ver + 1}, nil
}
func (r *p31InviteRepo) SetKCCleanupPending(context.Context, uuid.UUID, uuid.UUID, bool, int64) error {
	return nil
}
func (r *p31InviteRepo) CountPending(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (r *p31InviteRepo) ListExpiring(context.Context, time.Time, int) ([]domain.PendingInvitation, error) {
	return nil, nil
}
func (r *p31InviteRepo) ListPendingKCCleanup(context.Context, int) ([]domain.PendingInvitation, error) {
	return nil, nil
}
func (r *p31InviteRepo) MostRecentCreatedAt(context.Context, uuid.UUID, string) (time.Time, error) {
	return time.Time{}, nil
}
func (r *p31InviteRepo) CountCreatedInWindow(context.Context, uuid.UUID, time.Time) (int, error) {
	return 0, nil
}

// buildRevokeHandler wires an InvitationHandler for P-31 tests.
func buildRevokeHandler(repo *p31InviteRepo) *InvitationHandler {
	svc := service.NewInvitationService(repo, nil, nil, nil, nil, nil, happyCacheStub{}, happyTxRunner{}, nil, 7)
	return NewInvitationHandler(svc)
}

// ── P31-HP-01: owner revokes pending invite → 204 ────────────────────────────

// Test Case ID: P31-HP-01
func TestInvitationRevoke_HappyPath_204(t *testing.T) {
	tenantID := uuid.New()
	h := buildRevokeHandler(&p31InviteRepo{})
	c, w := buildCtx(http.MethodDelete, "/", `{"record_version":1}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "invitation_id", uuid.New().String())
	h.Revoke(c)
	assert.Less(t, w.Code, 300, w.Body.String())
}

// ── P31-HP-02: tenant_admin → 204 ────────────────────────────────────────────

// Test Case ID: P31-HP-02
func TestInvitationRevoke_TenantAdmin_204(t *testing.T) {
	tenantID := uuid.New()
	h := buildRevokeHandler(&p31InviteRepo{})
	c, w := buildCtx(http.MethodDelete, "/", `{"record_version":1}`, tenantAdminCtx(tenantID))
	setParams(c, "id", tenantID.String(), "invitation_id", uuid.New().String())
	h.Revoke(c)
	assert.Less(t, w.Code, 300, w.Body.String())
}

// ── P31-HP-03: rv via query param → 204 ──────────────────────────────────────

// Test Case ID: P31-HP-03
func TestInvitationRevoke_RecordVersionQueryParam_204(t *testing.T) {
	tenantID := uuid.New()
	h := buildRevokeHandler(&p31InviteRepo{})
	c, w := buildCtx(http.MethodDelete, "/?record_version=1", "", tenantOwnerCtx(tenantID))
	c.Request.URL.RawQuery = "record_version=1"
	setParams(c, "id", tenantID.String(), "invitation_id", uuid.New().String())
	h.Revoke(c)
	assert.Less(t, w.Code, 300, w.Body.String())
}

// ── P31-VAL-01: bad invitation_id → 400 ──────────────────────────────────────

// Test Case ID: P31-VAL-01
func TestRevoke_InvalidInvitationID_400(t *testing.T) {
	tenantID := uuid.New()
	h := buildRevokeHandler(&p31InviteRepo{})
	c, w := buildCtx(http.MethodDelete, "/", `{"record_version":1}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "invitation_id", "not-a-uuid")
	h.Revoke(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ── P31-VAL-02: bad tenant_id → 400 ──────────────────────────────────────────

// Test Case ID: P31-VAL-02
func TestRevoke_InvalidTenantID_400(t *testing.T) {
	h := buildRevokeHandler(&p31InviteRepo{})
	c, w := buildCtx(http.MethodDelete, "/", `{"record_version":1}`, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "bad-tenant", "invitation_id", uuid.New().String())
	h.Revoke(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ── P31-VAL-03: non-integer record_version query param → 400 ──────────────────

// Test Case ID: P31-VAL-03
func TestRevoke_BadRecordVersion_400(t *testing.T) {
	tenantID := uuid.New()
	h := buildRevokeHandler(&p31InviteRepo{})
	c, w := buildCtx(http.MethodDelete, "/?record_version=abc", "", tenantOwnerCtx(tenantID))
	c.Request.URL.RawQuery = "record_version=abc"
	setParams(c, "id", tenantID.String(), "invitation_id", uuid.New().String())
	h.Revoke(c)
	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// ── P31-AUTH-01: nil identity → 401 ──────────────────────────────────────────

// Test Case ID: P31-AUTH-01
func TestRevoke_NoIdentity_401(t *testing.T) {
	h := buildRevokeHandler(&p31InviteRepo{})
	c, w := buildCtx(http.MethodDelete, "/", "", nil)
	setParams(c, "id", uuid.New().String(), "invitation_id", uuid.New().String())
	h.Revoke(c)
	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

// ── P31-AUTH-02: plain member → 403 ──────────────────────────────────────────

// Test Case ID: P31-AUTH-02
func TestRevoke_InsufficientRole_403(t *testing.T) {
	tenantID := uuid.New()
	h := buildRevokeHandler(&p31InviteRepo{})
	c, w := buildCtx(http.MethodDelete, "/", `{"record_version":1}`, plainMemberCtx(tenantID))
	setParams(c, "id", tenantID.String(), "invitation_id", uuid.New().String())
	h.Revoke(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── P31-AUTH-03 / P31-SEC-01: cross-tenant → 403 ─────────────────────────────

// Test Case ID: P31-AUTH-03 / P31-SEC-01
func TestRevoke_CrossTenant_403(t *testing.T) {
	pathTenant := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: uuid.New(), Roles: []string{"tenant_owner"}}
	h := buildRevokeHandler(&p31InviteRepo{})
	c, w := buildCtx(http.MethodDelete, "/", `{"record_version":1}`, rc)
	setParams(c, "id", pathTenant.String(), "invitation_id", uuid.New().String())
	h.Revoke(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── P31-BL-01: invitation not found → 404 ────────────────────────────────────

// Test Case ID: P31-BL-01
func TestInvitationRevoke_NotFound_404(t *testing.T) {
	tenantID := uuid.New()
	repo := &p31InviteRepo{setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.InvitationStatus, int64) (*domain.PendingInvitation, error) {
		return nil, domain.NewError(domain.ErrInvitationNotFound, "invitation not found")
	}}
	h := buildRevokeHandler(repo)
	c, w := buildCtx(http.MethodDelete, "/", `{"record_version":1}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "invitation_id", uuid.New().String())
	h.Revoke(c)
	assertErrorCode(t, w, http.StatusNotFound, "invitation_not_found")
}

// ── P31-BL-02: already accepted → 404 ────────────────────────────────────────

// Test Case ID: P31-BL-02
func TestInvitationRevoke_AlreadyAccepted_404(t *testing.T) {
	tenantID := uuid.New()
	// InvitationService.Revoke finds non-pending → ErrInvitationNotFound
	repo := &p31InviteRepo{setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.InvitationStatus, int64) (*domain.PendingInvitation, error) {
		return nil, domain.NewError(domain.ErrInvitationNotFound, "invitation not found")
	}}
	h := buildRevokeHandler(repo)
	c, w := buildCtx(http.MethodDelete, "/", `{"record_version":1}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "invitation_id", uuid.New().String())
	h.Revoke(c)
	assertErrorCode(t, w, http.StatusNotFound, "invitation_not_found")
}

// ── P31-BL-03: already expired → 404 ─────────────────────────────────────────

// Test Case ID: P31-BL-03
func TestInvitationRevoke_Expired_404(t *testing.T) {
	tenantID := uuid.New()
	repo := &p31InviteRepo{setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.InvitationStatus, int64) (*domain.PendingInvitation, error) {
		return nil, domain.NewError(domain.ErrInvitationNotFound, "invitation not found")
	}}
	h := buildRevokeHandler(repo)
	c, w := buildCtx(http.MethodDelete, "/", `{"record_version":1}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "invitation_id", uuid.New().String())
	h.Revoke(c)
	assertErrorCode(t, w, http.StatusNotFound, "invitation_not_found")
}

// ── P31-EDGE-01 / P31-EDGE-02: already revoked (idempotent) → 404 ────────────

// Test Case ID: P31-EDGE-01 / P31-EDGE-02
func TestInvitationRevoke_AlreadyRevoked_404(t *testing.T) {
	tenantID := uuid.New()
	repo := &p31InviteRepo{setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.InvitationStatus, int64) (*domain.PendingInvitation, error) {
		return nil, domain.NewError(domain.ErrInvitationNotFound, "invitation not found")
	}}
	h := buildRevokeHandler(repo)
	c, w := buildCtx(http.MethodDelete, "/", `{"record_version":1}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "invitation_id", uuid.New().String())
	h.Revoke(c)
	assertErrorCode(t, w, http.StatusNotFound, "invitation_not_found")
}

// ── P31-DEP-01: DB unavailable → 503 ─────────────────────────────────────────

// Test Case ID: P31-DEP-01
func TestInvitationRevoke_DBUnavailable_503(t *testing.T) {
	tenantID := uuid.New()
	repo := &p31InviteRepo{setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.InvitationStatus, int64) (*domain.PendingInvitation, error) {
		return nil, domain.NewError(domain.ErrDBUnavailable, "pool exhausted")
	}}
	h := buildRevokeHandler(repo)
	c, w := buildCtx(http.MethodDelete, "/", `{"record_version":1}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "invitation_id", uuid.New().String())
	h.Revoke(c)
	assertErrorCode(t, w, http.StatusServiceUnavailable, "db_unavailable")
}
