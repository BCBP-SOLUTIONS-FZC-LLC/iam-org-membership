// Handler-layer coverage tests for InvitationHandler (P-6/P-30/P-31):
// the full Invite happy path (202, previously untested because the shared
// happyTenantRepo doesn't override LicensedSeatsForUpdate — SEAT-1's
// transactional recheck always saw 0 licensed seats and fell into the
// seat-limit branch) plus Revoke's malformed-input branches.
package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// ihSeatTenantRepo overrides LicensedSeatsForUpdate on top of happyTenantRepo
// so the SEAT-1 transactional recheck inside Invite's RunInTx sees a real
// seat count instead of the TenantRepositoryNoop default of 0.
type ihSeatTenantRepo struct {
	happyTenantRepo
	seats int
}

func (r *ihSeatTenantRepo) LicensedSeatsForUpdate(context.Context, uuid.UUID) (int, error) {
	return r.seats, nil
}

// P6-H-05: full happy path — SEAT-1 passes under FOR UPDATE, RP creates the
// KC user, the pending row is inserted, and the handler returns 202 with
// the invitation body (dto.go InvitationResponse shape).
func TestInvite_FullHappyPath_202(t *testing.T) {
	tenant := uuid.New()
	tenants := &ihSeatTenantRepo{
		happyTenantRepo: happyTenantRepo{findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: tenant, LicensedSeats: 10}, nil
		}},
		seats: 10,
	}
	repo := &iahInviteRepo{findPendingByEmailFn: func(context.Context, uuid.UUID, string) (*domain.PendingInvitation, error) {
		return nil, nil
	}}
	svc := service.NewInvitationService(repo, &happyMembershipRepo{}, nil, nil, tenants, &happyRPClient{}, happyCacheStub{}, happyTxRunner{}, nil, 7)
	h := NewInvitationHandler(svc)

	body := `{"email":"new@acme.com","full_name":"New User"}`
	c, w := buildCtx(http.MethodPost, "/", body, tenantAdminCtx(tenant))
	setParams(c, "id", tenant.String())
	h.Invite(c)

	assert.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "new@acme.com")
	assert.Contains(t, w.Body.String(), `"status":"pending"`)
}

// P31-M-02: malformed tenant id path param on Revoke → 400 invalid_uuid.
func TestInvitationRevoke_InvalidTenantID_400(t *testing.T) {
	h := &InvitationHandler{}

	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "not-a-uuid", "invitation_id", uuid.New().String())
	h.Revoke(c)

	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// P31-M-03: malformed invitation_id path param → 400 invalid_uuid.
func TestInvitationRevoke_InvalidInvitationID_400(t *testing.T) {
	tenant := uuid.New()
	h := &InvitationHandler{}

	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "invitation_id", "not-a-uuid")
	h.Revoke(c)

	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// P31-M-04: malformed JSON body (Content-Length > 0) → 400 validation_error.
func TestInvitationRevoke_MalformedBody_400(t *testing.T) {
	tenant, invID := uuid.New(), uuid.New()
	h := &InvitationHandler{}

	c, w := buildCtx(http.MethodDelete, "/", `{"record_version":`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "invitation_id", invID.String())
	h.Revoke(c)

	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// P31-M-05: legacy query-string record_version is not a valid integer → 400.
func TestInvitationRevoke_InvalidQueryVersion_400(t *testing.T) {
	tenant, invID := uuid.New(), uuid.New()
	svc := service.NewInvitationService(&iahInviteRepo{}, &happyMembershipRepo{}, nil, nil, nil, &happyRPClient{}, happyCacheStub{}, happyTxRunner{}, nil, 7)
	h := &InvitationHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "invitation_id", invID.String())
	c.Request.URL.RawQuery = "record_version=not-a-number"
	h.Revoke(c)

	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}
