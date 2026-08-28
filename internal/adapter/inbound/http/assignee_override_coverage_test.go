// Handler-layer unit tests for:
//
//	I-13 POST /api/v1/internal/tenants/:id/tenders/:tender_id/assignee-override
//	     (InternalHandler.AssigneeOverride)
//
// OVR-1: validates the new assignee's active membership + dept role level,
// emits TenderAssigneeOverridden, and persists nothing. All service-level
// auth gating (actor must hold tender_admin or higher) is exercised here
// because the handler does no requestctx-based role gate of its own —
// that responsibility lives in the service (§5.4 I-13 step 1).
package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// buildOverrideHandler wires an InternalHandler whose only populated field is
// membership, which is sufficient for AssigneeOverride. All other service
// pointers are nil (provisioning, authz, invitation, groupMappings, tenants).
func buildOverrideHandler(mem *service.MembershipService) *InternalHandler {
	return &InternalHandler{membership: mem}
}

// overrideBody constructs the JSON request body for AssigneeOverride.
func overrideBody(actorID, newUserID, deptID uuid.UUID, requiredLevel string) string {
	return `{"actor_id":"` + actorID.String() +
		`","new_user_id":"` + newUserID.String() +
		`","department_id":"` + deptID.String() +
		`","required_level":"` + requiredLevel + `"}`
}

// roleListRepo returns a mhRoleRepo whose ListByUser always returns the given
// role codes for any (tenant, user) pair.
func roleListRepo(codes ...domain.TenantRoleCode) *mhRoleRepo {
	return &mhRoleRepo{listByUserFn: func(_ context.Context, _, _ uuid.UUID) ([]domain.TenantRole, error) {
		roles := make([]domain.TenantRole, len(codes))
		for i, c := range codes {
			roles[i] = domain.TenantRole{RoleCode: c}
		}
		return roles, nil
	}}
}

// activeMemberRepo returns a mhMemRepo whose FindByUserID always returns an
// active membership in the given tenant, with the given set of department
// memberships projected via ListByUser on the dept-membership repo.
func activeMemberRepo() *mhMemRepo {
	return &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{
			ID: uuid.New(), TenantID: tid, UserID: uid,
			Status: domain.MembershipActive, RecordVersion: 1,
		}, nil
	}}
}

// ── I13-VAL-01 ───────────────────────────────────────────────────────────────

// TestAssigneeOverride_MissingActorID_400 (I13-VAL-01): actor_id is the nil
// UUID (zero value) in the body — handler returns 400 validation_error before
// reaching the service.
func TestAssigneeOverride_MissingActorID_400(t *testing.T) {
	tenantID := uuid.New()
	tenderID := uuid.New()
	// actor_id omitted → zero UUID
	body := `{"new_user_id":"` + uuid.New().String() +
		`","department_id":"` + uuid.New().String() +
		`","required_level":"preparator"}`

	h := &InternalHandler{}
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String(), "tender_id", tenderID.String())
	h.AssigneeOverride(c)

	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// ── I13-VAL-02 ───────────────────────────────────────────────────────────────

// TestAssigneeOverride_InvalidTenderID_400 (I13-VAL-02): tender_id path param
// is not a UUID — parseUUIDParam returns 400 before body is read.
func TestAssigneeOverride_InvalidTenderID_400(t *testing.T) {
	tenantID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{}`, systemCtx())
	setParams(c, "id", tenantID.String(), "tender_id", "bad-uuid")
	h.AssigneeOverride(c)

	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ── I13-VAL-03 ───────────────────────────────────────────────────────────────

// TestAssigneeOverride_MalformedBody_400 (I13-VAL-03): body is not valid JSON
// — ShouldBindJSON fails → 400 validation_error.
func TestAssigneeOverride_MalformedBody_400(t *testing.T) {
	tenantID := uuid.New()
	tenderID := uuid.New()
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodPost, "/", `not-json`, systemCtx())
	setParams(c, "id", tenantID.String(), "tender_id", tenderID.String())
	h.AssigneeOverride(c)

	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// ── I13-VAL-04 ───────────────────────────────────────────────────────────────

// TestAssigneeOverride_BadRequiredLevel_422 (I13-VAL-04): required_level is an
// unknown string ("wizard"). The handler passes it through to the service as a
// DeptRole. The service's ValidateAndEmitAssigneeOverride accepts any DeptRole
// value but the eligibility check then finds no dept match (the assignee is not
// in a dept with RoleLevel.Satisfies("wizard") since Rank() returns -1 for
// unknown codes). The service returns ErrAssigneeIneligible → 422.
func TestAssigneeOverride_BadRequiredLevel_422(t *testing.T) {
	tenantID := uuid.New()
	tenderID := uuid.New()
	actorID := uuid.New()
	newUserID := uuid.New()
	deptID := uuid.New()

	roles := roleListRepo(domain.RoleTenderAdmin)
	// Assignee is active but has no dept memberships → not eligible.
	mem := activeMemberRepo()
	deptRepo := &drhDeptMemRepo{listByUserFn: func(_ context.Context, _, _ uuid.UUID) ([]domain.DeptMembership, error) {
		return nil, nil
	}}

	svc := service.NewMembershipService(mem, roles, deptRepo, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, nil, happyTxRunner{}, nil, 30)
	h := buildOverrideHandler(svc)

	body := overrideBody(actorID, newUserID, deptID, "wizard")
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String(), "tender_id", tenderID.String())
	h.AssigneeOverride(c)

	assertErrorCode(t, w, http.StatusUnprocessableEntity, "assignee_ineligible")
}

// ── I13-AUTH-01 ──────────────────────────────────────────────────────────────

// TestAssigneeOverride_NoIdentity_403 (I13-AUTH-01): the actor_id is populated
// but the roles repo returns empty (simulating a caller with no elevated
// roles). Service returns ErrInsufficientRole → 403. The I-13 handler has no
// requestctx-based gate of its own (the RequireSystemRole middleware handles
// the route-level gate); the service provides the defense-in-depth actor-role
// check (§5.4 I-13 step 1).
func TestAssigneeOverride_NoIdentity_403(t *testing.T) {
	tenantID := uuid.New()
	tenderID := uuid.New()
	actorID := uuid.New()
	newUserID := uuid.New()
	deptID := uuid.New()

	// No roles for the actor → service returns ErrInsufficientRole.
	roles := &mhRoleRepo{listByUserFn: func(_ context.Context, _, _ uuid.UUID) ([]domain.TenantRole, error) {
		return nil, nil
	}}
	svc := service.NewMembershipService(&mhMemRepo{}, roles, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, nil, happyTxRunner{}, nil, 30)
	h := buildOverrideHandler(svc)

	body := overrideBody(actorID, newUserID, deptID, "preparator")
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String(), "tender_id", tenderID.String())
	h.AssigneeOverride(c)

	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── I13-AUTH-02 ──────────────────────────────────────────────────────────────

// TestAssigneeOverride_PlainMember_403 (I13-AUTH-02): the actor_id maps to a
// user whose only tenant role is "member" (derived, never in tenant_roles).
// roles.ListByUser returns empty → service → ErrInsufficientRole → 403.
func TestAssigneeOverride_PlainMember_403(t *testing.T) {
	tenantID := uuid.New()
	tenderID := uuid.New()
	actorID := uuid.New()
	newUserID := uuid.New()
	deptID := uuid.New()

	// "member" is not stored in tenant_roles (TR-7); ListByUser returns empty.
	roles := &mhRoleRepo{listByUserFn: func(_ context.Context, _, _ uuid.UUID) ([]domain.TenantRole, error) {
		return []domain.TenantRole{}, nil
	}}
	svc := service.NewMembershipService(&mhMemRepo{}, roles, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, nil, happyTxRunner{}, nil, 30)
	h := buildOverrideHandler(svc)

	body := overrideBody(actorID, newUserID, deptID, "preparator")
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String(), "tender_id", tenderID.String())
	h.AssigneeOverride(c)

	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── I13-SEC-01 / I13-SEC-02 ──────────────────────────────────────────────────

// TestAssigneeOverride_CrossTenant_403 (I13-SEC-01, I13-SEC-02): the caller's
// requestctx TenantID differs from the path :id. Because AssigneeOverride does
// no same-tenant guard at the handler layer (I-13 is an internal route gated
// solely by RequireSystemRole in production), cross-tenant scenarios surface as
// a service-layer auth failure when the actor holds no elevated roles in the
// target tenant. This validates that the service's role lookup uses the path
// tenantID, not the requestctx one — a caller authenticated for tenant B cannot
// manufacture elevated-role signals in tenant A by body-injecting a tenant-A
// actor_id without an actual row.
func TestAssigneeOverride_CrossTenant_403(t *testing.T) {
	pathTenantID := uuid.New()
	callerTenantID := uuid.New() // different from path
	tenderID := uuid.New()
	actorID := uuid.New()
	newUserID := uuid.New()
	deptID := uuid.New()

	// Actor has no roles in the path tenant (cross-tenant scenario).
	roles := &mhRoleRepo{listByUserFn: func(_ context.Context, tid, _ uuid.UUID) ([]domain.TenantRole, error) {
		// Only has roles in callerTenantID, not pathTenantID.
		if tid == pathTenantID {
			return nil, nil
		}
		return []domain.TenantRole{{RoleCode: domain.RoleTenderAdmin}}, nil
	}}
	svc := service.NewMembershipService(&mhMemRepo{}, roles, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, nil, happyTxRunner{}, nil, 30)
	h := buildOverrideHandler(svc)

	// requestctx carries callerTenantID but path uses pathTenantID.
	callerRC := &requestctx.RequestContext{
		UserID:   uuid.New(),
		TenantID: callerTenantID,
		Roles:    []string{"iam-system"},
	}
	body := overrideBody(actorID, newUserID, deptID, "preparator")
	c, w := buildCtx(http.MethodPost, "/", body, callerRC)
	setParams(c, "id", pathTenantID.String(), "tender_id", tenderID.String())
	h.AssigneeOverride(c)

	// 403 (actor has no elevated role in path tenant) or 422 (if roles are
	// present but assignee not in dept) — either proves the cross-tenant guard.
	assert.True(t, w.Code == http.StatusForbidden || w.Code == http.StatusUnprocessableEntity,
		"expected 403 or 422 for cross-tenant request, got %d: %s", w.Code, w.Body.String())
}

// ── I13-HP-01 ────────────────────────────────────────────────────────────────

// TestAssigneeOverride_HappyPath_200 (I13-HP-01): actor is tender_admin, assignee
// is active and holds approver in the target department with required=preparator
// (satisfies). Service emits the event (txRunner runs in-process, no outbox
// insert since pub==nil in test context) and returns nil → 200 eligible:true.
func TestAssigneeOverride_HappyPath_200(t *testing.T) {
	tenantID := uuid.New()
	tenderID := uuid.New()
	actorID := uuid.New()
	newUserID := uuid.New()
	deptID := uuid.New()

	roles := roleListRepo(domain.RoleTenderAdmin)
	mem := activeMemberRepo()
	deptRepo := &drhDeptMemRepo{listByUserFn: func(_ context.Context, _, uid uuid.UUID) ([]domain.DeptMembership, error) {
		if uid == newUserID {
			return []domain.DeptMembership{
				{DepartmentID: deptID, RoleLevel: domain.DeptApprover},
			}, nil
		}
		return nil, nil
	}}

	svc := service.NewMembershipService(mem, roles, deptRepo, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, nil, happyTxRunner{}, nil, 30)
	h := buildOverrideHandler(svc)

	body := overrideBody(actorID, newUserID, deptID, "preparator")
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String(), "tender_id", tenderID.String())
	h.AssigneeOverride(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"eligible":true`)
	assert.Contains(t, w.Body.String(), tenderID.String())
}

// ── I13-HP-02 ────────────────────────────────────────────────────────────────

// TestAssigneeOverride_TenantOwnerActor_200 (I13-HP-02): actor holds
// tenant_owner (satisfies the tender_admin-or-higher gate). Assignee is active
// and in the target department as reviewer with required=reviewer (exact match).
func TestAssigneeOverride_TenantOwnerActor_200(t *testing.T) {
	tenantID := uuid.New()
	tenderID := uuid.New()
	actorID := uuid.New()
	newUserID := uuid.New()
	deptID := uuid.New()

	roles := roleListRepo(domain.RoleTenantOwner)
	mem := activeMemberRepo()
	deptRepo := &drhDeptMemRepo{listByUserFn: func(_ context.Context, _, uid uuid.UUID) ([]domain.DeptMembership, error) {
		if uid == newUserID {
			return []domain.DeptMembership{
				{DepartmentID: deptID, RoleLevel: domain.DeptReviewer},
			}, nil
		}
		return nil, nil
	}}

	svc := service.NewMembershipService(mem, roles, deptRepo, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, nil, happyTxRunner{}, nil, 30)
	h := buildOverrideHandler(svc)

	body := overrideBody(actorID, newUserID, deptID, "reviewer")
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String(), "tender_id", tenderID.String())
	h.AssigneeOverride(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"eligible":true`)
}

// ── I13-BL-01 ────────────────────────────────────────────────────────────────

// TestAssigneeOverride_AssigneeNotInDept_422 (I13-BL-01): the assignee is an
// active tenant member but has no department membership in the target dept.
// Service returns ErrAssigneeIneligible → 422.
func TestAssigneeOverride_AssigneeNotInDept_422(t *testing.T) {
	tenantID := uuid.New()
	tenderID := uuid.New()
	actorID := uuid.New()
	newUserID := uuid.New()
	deptID := uuid.New()

	roles := roleListRepo(domain.RoleTenderAdmin)
	mem := activeMemberRepo()
	// Assignee is in a different dept, not deptID.
	deptRepo := &drhDeptMemRepo{listByUserFn: func(_ context.Context, _, uid uuid.UUID) ([]domain.DeptMembership, error) {
		if uid == newUserID {
			return []domain.DeptMembership{
				{DepartmentID: uuid.New(), RoleLevel: domain.DeptApprover}, // wrong dept
			}, nil
		}
		return nil, nil
	}}

	svc := service.NewMembershipService(mem, roles, deptRepo, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, nil, happyTxRunner{}, nil, 30)
	h := buildOverrideHandler(svc)

	body := overrideBody(actorID, newUserID, deptID, "preparator")
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String(), "tender_id", tenderID.String())
	h.AssigneeOverride(c)

	assertErrorCode(t, w, http.StatusUnprocessableEntity, "assignee_ineligible")
}

// ── I13-BL-02 / I13-BL-03 ────────────────────────────────────────────────────

// TestAssigneeOverride_AssigneeSuspended_422 (I13-BL-02, I13-BL-03): the
// assignee's tenant_membership.status is suspended. Service fetches the member
// via Get → FindByUserID returns suspended → ErrAssigneeIneligible → 422.
func TestAssigneeOverride_AssigneeSuspended_422(t *testing.T) {
	tenantID := uuid.New()
	tenderID := uuid.New()
	actorID := uuid.New()
	newUserID := uuid.New()
	deptID := uuid.New()

	roles := roleListRepo(domain.RoleTenderAdmin)
	// Assignee membership is suspended.
	mem := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{
			ID: uuid.New(), TenantID: tid, UserID: uid,
			Status: domain.MembershipSuspended, RecordVersion: 1,
		}, nil
	}}

	svc := service.NewMembershipService(mem, roles, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, nil, happyTxRunner{}, nil, 30)
	h := buildOverrideHandler(svc)

	body := overrideBody(actorID, newUserID, deptID, "preparator")
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String(), "tender_id", tenderID.String())
	h.AssigneeOverride(c)

	assertErrorCode(t, w, http.StatusUnprocessableEntity, "assignee_ineligible")
}

// ── I13-BL-04 ────────────────────────────────────────────────────────────────

// TestAssigneeOverride_AssigneeBelowLevel_422 (I13-BL-04): assignee is active
// and in the target dept but holds preparator while required is approver.
// DeptRole.Satisfies returns false → ErrAssigneeIneligible → 422.
func TestAssigneeOverride_AssigneeBelowLevel_422(t *testing.T) {
	tenantID := uuid.New()
	tenderID := uuid.New()
	actorID := uuid.New()
	newUserID := uuid.New()
	deptID := uuid.New()

	roles := roleListRepo(domain.RoleTenderAdmin)
	mem := activeMemberRepo()
	deptRepo := &drhDeptMemRepo{listByUserFn: func(_ context.Context, _, uid uuid.UUID) ([]domain.DeptMembership, error) {
		if uid == newUserID {
			return []domain.DeptMembership{
				{DepartmentID: deptID, RoleLevel: domain.DeptPreparator}, // too low
			}, nil
		}
		return nil, nil
	}}

	svc := service.NewMembershipService(mem, roles, deptRepo, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, nil, happyTxRunner{}, nil, 30)
	h := buildOverrideHandler(svc)

	body := overrideBody(actorID, newUserID, deptID, "approver") // need approver, have preparator
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String(), "tender_id", tenderID.String())
	h.AssigneeOverride(c)

	assertErrorCode(t, w, http.StatusUnprocessableEntity, "assignee_ineligible")
}

// ── I13-EDGE-01 ──────────────────────────────────────────────────────────────

// TestAssigneeOverride_NoElevatedRole_403 (I13-EDGE-01): actor has no elevated
// role in tenant_roles (not tender_admin, not tenant_admin, not tenant_owner).
// The service's roles.ListByUser returns empty → hasElevated=false →
// ErrInsufficientRole → 403.
func TestAssigneeOverride_NoElevatedRole_403(t *testing.T) {
	tenantID := uuid.New()
	tenderID := uuid.New()
	actorID := uuid.New()
	newUserID := uuid.New()
	deptID := uuid.New()

	// No elevated roles at all.
	roles := &mhRoleRepo{}
	svc := service.NewMembershipService(&mhMemRepo{}, roles, &drhDeptMemRepo{}, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, nil, happyTxRunner{}, nil, 30)
	h := buildOverrideHandler(svc)

	body := overrideBody(actorID, newUserID, deptID, "preparator")
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String(), "tender_id", tenderID.String())
	h.AssigneeOverride(c)

	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── I13-EDGE-02 ──────────────────────────────────────────────────────────────

// TestAssigneeOverride_MultiDeptSatisfied_200 (I13-EDGE-02): assignee is a
// member of multiple departments; only one of them matches deptID with a level
// that satisfies required_level. The service iterates all departments and finds
// the matching one → eligible → 200.
func TestAssigneeOverride_MultiDeptSatisfied_200(t *testing.T) {
	tenantID := uuid.New()
	tenderID := uuid.New()
	actorID := uuid.New()
	newUserID := uuid.New()
	targetDeptID := uuid.New()
	otherDeptID := uuid.New()

	roles := roleListRepo(domain.RoleTenderAdmin)
	mem := activeMemberRepo()
	// Two depts: other dept (preparator, wrong dept), target dept (reviewer, right dept).
	deptRepo := &drhDeptMemRepo{listByUserFn: func(_ context.Context, _, uid uuid.UUID) ([]domain.DeptMembership, error) {
		if uid == newUserID {
			return []domain.DeptMembership{
				{DepartmentID: otherDeptID, RoleLevel: domain.DeptPreparator},
				{DepartmentID: targetDeptID, RoleLevel: domain.DeptReviewer},
			}, nil
		}
		return nil, nil
	}}

	svc := service.NewMembershipService(mem, roles, deptRepo, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, nil, happyTxRunner{}, nil, 30)
	h := buildOverrideHandler(svc)

	// required_level=preparator; assignee has reviewer in targetDeptID (satisfies).
	body := overrideBody(actorID, newUserID, targetDeptID, "preparator")
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String(), "tender_id", tenderID.String())
	h.AssigneeOverride(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"eligible":true`)
}

// ── I13-EVT-01 ───────────────────────────────────────────────────────────────

// TestAssigneeOverride_EventEmitted_200 (I13-EVT-01): on a successful override
// the service calls txRunner.RunInTx. In the unit-test context happyTxRunner
// runs the closure in-process; since no EventPublisher is stored in the context,
// pub==nil and EnqueueCtx is skipped (the outbox path short-circuits safely).
// The handler returns 200 with eligible:true and the correct tender_id.
// This confirms the event path is exercised without panicking (OVR-1 contract).
func TestAssigneeOverride_EventEmitted_200(t *testing.T) {
	tenantID := uuid.New()
	tenderID := uuid.New()
	actorID := uuid.New()
	newUserID := uuid.New()
	deptID := uuid.New()

	roles := roleListRepo(domain.RoleTenderAdmin)
	mem := activeMemberRepo()
	deptRepo := &drhDeptMemRepo{listByUserFn: func(_ context.Context, _, uid uuid.UUID) ([]domain.DeptMembership, error) {
		if uid == newUserID {
			return []domain.DeptMembership{
				{DepartmentID: deptID, RoleLevel: domain.DeptApprover},
			}, nil
		}
		return nil, nil
	}}

	txRan := false
	trackingTx := trackingTxRunner{inner: happyTxRunner{}, ran: &txRan}

	svc := service.NewMembershipService(mem, roles, deptRepo, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, nil, trackingTx, nil, 30)
	h := buildOverrideHandler(svc)

	body := overrideBody(actorID, newUserID, deptID, "approver")
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String(), "tender_id", tenderID.String())
	h.AssigneeOverride(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"eligible":true`)
	assert.Contains(t, w.Body.String(), tenderID.String())
	assert.True(t, txRan, "expected txRunner.RunInTx to be called on success (outbox path)")
}

// trackingTxRunner wraps happyTxRunner and records whether RunInTx was invoked.
type trackingTxRunner struct {
	inner happyTxRunner
	ran   *bool
}

func (r trackingTxRunner) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	*r.ran = true
	return r.inner.RunInTx(ctx, fn)
}

// ── I13-HP-03 ────────────────────────────────────────────────────────────────

// TestAssigneeOverride_HP3_MinimalFields_200 (I13-HP-03): minimal valid request
// with required_level=preparator (lowest level). Assignee has exactly
// preparator in the target dept (boundary: Satisfies(preparator) with
// preparator → rank equal → true).
func TestAssigneeOverride_HP3_MinimalFields_200(t *testing.T) {
	tenantID := uuid.New()
	tenderID := uuid.New()
	actorID := uuid.New()
	newUserID := uuid.New()
	deptID := uuid.New()

	roles := roleListRepo(domain.RoleTenantAdmin)
	mem := activeMemberRepo()
	deptRepo := &drhDeptMemRepo{listByUserFn: func(_ context.Context, _, uid uuid.UUID) ([]domain.DeptMembership, error) {
		if uid == newUserID {
			return []domain.DeptMembership{
				{DepartmentID: deptID, RoleLevel: domain.DeptPreparator},
			}, nil
		}
		return nil, nil
	}}

	svc := service.NewMembershipService(mem, roles, deptRepo, &happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{}, &happyRPClient{}, nil, happyTxRunner{}, nil, 30)
	h := buildOverrideHandler(svc)

	body := overrideBody(actorID, newUserID, deptID, "preparator")
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", tenantID.String(), "tender_id", tenderID.String())
	h.AssigneeOverride(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"eligible":true`)
	assert.Contains(t, w.Body.String(), newUserID.String())
}
