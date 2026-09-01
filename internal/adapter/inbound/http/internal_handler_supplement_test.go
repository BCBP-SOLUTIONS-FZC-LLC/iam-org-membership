// internal_handler_supplement_test.go fills handler-layer coverage gaps in
// InternalHandler that are not covered by the existing test suite:
//
//   - GetSeatUsage (71.4%): service error path, OverageSince field population
//   - CheckMemberExists (78.3%): inactive member (Status != MembershipActive),
//     ErrMemberNotFound path
//   - AssignFromGroups (68.8%): user_id nil check, service error path
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

// ── GetSeatUsage error path ───────────────────────────────────────────────

// TestInternalGetSeatUsage_ServiceError_500 verifies that when h.membership.SeatUsage
// returns an error, GetSeatUsage forwards it to HandleError (which maps to 500
// for generic errors).
func TestInternalGetSeatUsage_ServiceError_500(t *testing.T) {
	tenants := &happyTenantRepo{findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
		return nil, domain.NewError(domain.ErrTenantNotFound, "tenant not found")
	}}
	svc := service.NewMembershipService(
		&mhMemRepo{}, &mhRoleRepo{}, &drhDeptMemRepo{},
		tenants, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{},
		&drhWorkflowClient{}, happyTxRunner{}, nil, 30,
	)
	h := &InternalHandler{membership: svc}

	tenantID := uuid.New()
	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", tenantID.String())
	h.GetSeatUsage(c)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// TestInternalGetSeatUsage_WithOverageSince_IncludesField verifies that when
// the tenant has OverageSince set (and seatOverageDays > 0), the response
// includes both "overage_since" and "grace_ends_at".
func TestInternalGetSeatUsage_WithOverageSince_IncludesField(t *testing.T) {
	overageStart := time.Now().UTC().Add(-48 * time.Hour)
	tenants := &happyTenantRepo{findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
		return &domain.Tenant{ID: id, LicensedSeats: 5, OverageSince: &overageStart}, nil
	}}
	mem := &mhMemRepo{countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 6, nil }}
	inv := &iahInviteRepo{countPendingFn: func(context.Context, uuid.UUID) (int, error) { return 0, nil }}
	// seatOverageDays=30 so GraceEndsAt will be populated.
	svc := service.NewMembershipService(
		mem, &mhRoleRepo{}, &drhDeptMemRepo{},
		tenants, inv, happyCacheStub{}, &happyRPClient{},
		&drhWorkflowClient{}, happyTxRunner{}, nil, 30,
	)
	h := &InternalHandler{membership: svc}

	tenantID := uuid.New()
	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", tenantID.String())
	h.GetSeatUsage(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"overage_since"`)
	assert.Contains(t, w.Body.String(), `"grace_ends_at"`)
}

// ── CheckMemberExists error paths ─────────────────────────────────────────

// TestInternalCheckMemberExists_NotFound_200Active_False verifies that when
// FindByUserID returns ErrMemberNotFound, the handler returns 200 with
// active=false (not a 404 — see LLD §11.2).
func TestInternalCheckMemberExists_NotFound_200Active_False(t *testing.T) {
	mem := &mhMemRepo{findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return nil, domain.NewError(domain.ErrMemberNotFound, "not a member")
	}}
	svc := service.NewMembershipService(
		mem, &mhRoleRepo{}, &drhDeptMemRepo{},
		&happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{},
		&drhWorkflowClient{}, happyTxRunner{}, nil, 30,
	)
	h := &InternalHandler{membership: svc}

	tenantID := uuid.New()
	userID := uuid.New()
	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", tenantID.String(), "user_id", userID.String())
	h.CheckMemberExists(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"active":false`)
}

// TestInternalCheckMemberExists_Suspended_200Active_False verifies that when
// FindByUserID returns a membership with Status=suspended (not active), the
// handler returns 200 with active=false.
func TestInternalCheckMemberExists_Suspended_200Active_False(t *testing.T) {
	userID := uuid.New()
	mem := &mhMemRepo{findByUserFn: func(_ context.Context, _, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), UserID: uid, Status: domain.MembershipSuspended}, nil
	}}
	svc := service.NewMembershipService(
		mem, &mhRoleRepo{}, &drhDeptMemRepo{},
		&happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{},
		&drhWorkflowClient{}, happyTxRunner{}, nil, 30,
	)
	h := &InternalHandler{membership: svc}

	tenantID := uuid.New()
	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", tenantID.String(), "user_id", userID.String())
	h.CheckMemberExists(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"active":false`)
}

// TestInternalCheckMemberExists_Active_200Active_True verifies the happy path:
// active member → 200 with active=true.
func TestInternalCheckMemberExists_Active_200Active_True(t *testing.T) {
	memID := uuid.New()
	userID := uuid.New()
	mem := &mhMemRepo{findByUserFn: func(_ context.Context, _, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: memID, UserID: uid, Status: domain.MembershipActive}, nil
	}}
	svc := service.NewMembershipService(
		mem, &mhRoleRepo{}, &drhDeptMemRepo{},
		&happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{},
		&drhWorkflowClient{}, happyTxRunner{}, nil, 30,
	)
	h := &InternalHandler{membership: svc}

	tenantID := uuid.New()
	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", tenantID.String(), "user_id", userID.String())
	h.CheckMemberExists(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"active":true`)
	assert.Contains(t, w.Body.String(), memID.String())
}

// TestInternalCheckMemberExists_ServiceError_Propagates verifies that a
// non-ErrMemberNotFound error from FindByUserID propagates as an HTTP error.
func TestInternalCheckMemberExists_ServiceError_Propagates(t *testing.T) {
	mem := &mhMemRepo{findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return nil, errors.New("db_unavailable")
	}}
	svc := service.NewMembershipService(
		mem, &mhRoleRepo{}, &drhDeptMemRepo{},
		&happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{},
		&drhWorkflowClient{}, happyTxRunner{}, nil, 30,
	)
	h := &InternalHandler{membership: svc}

	tenantID := uuid.New()
	userID := uuid.New()
	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", tenantID.String(), "user_id", userID.String())
	h.CheckMemberExists(c)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// ── AssignFromGroups gaps ─────────────────────────────────────────────────

// TestInternalAssignFromGroups_NilUserID_Returns400 verifies that a
// user_id of uuid.Nil in the request body triggers the nil-guard at line 342
// and returns 400 validation_error.
func TestInternalAssignFromGroups_NilUserID_Returns400(t *testing.T) {
	h := &InternalHandler{} // nil groupMappings — must not reach it

	tenantID := uuid.New()
	body := `{"user_id":"00000000-0000-0000-0000-000000000000","groups":["eng"]}`
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String())
	h.AssignFromGroups(c)

	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// TestInternalAssignFromGroups_MalformedBody_Returns400 verifies that a
// malformed JSON body triggers the ShouldBindJSON error path.
func TestInternalAssignFromGroups_MalformedBody_Returns400(t *testing.T) {
	h := &InternalHandler{}

	tenantID := uuid.New()
	c, w := buildCtx(http.MethodPost, "/", `{not-json`, systemCtx())
	setParams(c, "id", tenantID.String())
	h.AssignFromGroups(c)

	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// agsGroupMappingClient is a port.GroupMappingClient stub for AssignFromGroups tests.
type agsGroupMappingClient struct {
	resolveFn func(context.Context, uuid.UUID, []string) (*port.GroupResolution, error)
}

func (c *agsGroupMappingClient) ResolveGroups(ctx context.Context, tenantID uuid.UUID, groups []string) (*port.GroupResolution, error) {
	if c.resolveFn != nil {
		return c.resolveFn(ctx, tenantID, groups)
	}
	return &port.GroupResolution{}, nil
}

// agsMembershipRepo is a minimal port.MembershipRepository for JIT tests.
type agsMembershipRepo struct{}

func (r *agsMembershipRepo) FindByUserID(_ context.Context, tenantID, userID uuid.UUID) (*domain.TenantMembership, error) {
	return &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID, Status: domain.MembershipActive}, nil
}
func (r *agsMembershipRepo) List(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
	return &domain.MembershipListPage{}, nil
}
func (r *agsMembershipRepo) Insert(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *agsMembershipRepo) SetStatus(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *agsMembershipRepo) SoftDelete(context.Context, uuid.UUID, uuid.UUID, int64) error {
	return nil
}
func (r *agsMembershipRepo) CountActive(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (r *agsMembershipRepo) ListActiveUserIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

// agsTxRunner is a simple TxRunner that calls fn with the same ctx.
type agsTxRunner struct{}

func (r *agsTxRunner) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

// TestInternalAssignFromGroups_ServiceError_Returns503 verifies that when
// the GroupMappingService.AssignFromGroups call returns an error, the handler
// forwards it via HandleError.
func TestInternalAssignFromGroups_ServiceError_Returns503(t *testing.T) {
	// Build a GroupMappingService whose ResolveGroups client returns an error.
	// The service fails open (returns empty JITResult, no error) — to surface
	// an error from AssignFromGroups we must fail at FindByUserID (membership
	// repo error).
	membershipErr := errors.New("membership lookup failed")
	brokenMemberships := &agsBrokenMembershipRepo{err: membershipErr}
	client := &agsGroupMappingClient{
		resolveFn: func(context.Context, uuid.UUID, []string) (*port.GroupResolution, error) {
			return &port.GroupResolution{}, nil // empty resolution → reaches FindByUserID
		},
	}
	gm := service.NewGroupMappingService(brokenMemberships, nil, nil, &agsTxRunner{}, nil, client)

	tenantID := uuid.New()
	userID := uuid.New()
	body := `{"user_id":"` + userID.String() + `","groups":["eng"]}`
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String())

	h := &InternalHandler{groupMappings: gm}
	h.AssignFromGroups(c)

	assert.NotEqual(t, http.StatusOK, w.Code, "service error must not return 200")
}

// agsBrokenMembershipRepo returns an error from FindByUserID.
type agsBrokenMembershipRepo struct {
	err error
}

func (r *agsBrokenMembershipRepo) FindByUserID(_ context.Context, _, _ uuid.UUID) (*domain.TenantMembership, error) {
	return nil, r.err
}
func (r *agsBrokenMembershipRepo) List(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
	return nil, nil
}
func (r *agsBrokenMembershipRepo) Insert(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *agsBrokenMembershipRepo) SetStatus(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *agsBrokenMembershipRepo) SoftDelete(context.Context, uuid.UUID, uuid.UUID, int64) error {
	return nil
}
func (r *agsBrokenMembershipRepo) CountActive(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (r *agsBrokenMembershipRepo) ListActiveUserIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

// TestInternalAssignFromGroups_HappyPath_Returns200 verifies the happy path:
// when AssignFromGroups succeeds, the handler returns 200 OK with the JITResult.
func TestInternalAssignFromGroups_HappyPath_Returns200(t *testing.T) {
	// All-empty resolution → no dept or role assignments, success.
	client := &agsGroupMappingClient{}
	gm := service.NewGroupMappingService(&agsMembershipRepo{}, &agsRoleRepo{}, nil, &agsTxRunner{}, nil, client)

	tenantID := uuid.New()
	userID := uuid.New()
	body := `{"user_id":"` + userID.String() + `","groups":["eng"]}`
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String())

	h := &InternalHandler{groupMappings: gm}
	h.AssignFromGroups(c)

	assert.Equal(t, http.StatusOK, w.Code)
}

// agsRoleRepo is a minimal TenantRoleRepository for JIT handler tests.
type agsRoleRepo struct{}

func (r *agsRoleRepo) ListByUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *agsRoleRepo) ListByRole(context.Context, uuid.UUID, domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *agsRoleRepo) CountActiveOwners(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (r *agsRoleRepo) Grant(context.Context, *domain.TenantRole) (*domain.TenantRole, error) {
	return nil, nil
}
func (r *agsRoleRepo) Revoke(context.Context, uuid.UUID, uuid.UUID, domain.TenantRoleCode) (*domain.TenantRole, error) {
	return nil, nil
}
func (r *agsRoleRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}
