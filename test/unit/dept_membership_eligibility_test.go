// Unit tests for DeptMembershipService.CheckEligibility (I-19, the Workflow
// Services' assignee eligibility read): every not-eligible reason, the full
// required_level × held-level matrix (preparator < reviewer < approver),
// required_level validation, and repository-error propagation.
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

// The el* fakes embed the port interface and override only what
// CheckEligibility calls; any other method panics on the nil embed.

type elMemRepo struct {
	port.MembershipRepository
	m   *domain.TenantMembership
	err error
}

func (r *elMemRepo) FindByUserID(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
	return r.m, r.err
}

type elTenantDeptRepo struct {
	port.TenantDepartmentRepository
	td  *domain.TenantDepartment
	err error
}

func (r *elTenantDeptRepo) Find(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
	return r.td, r.err
}

type elDeptMemRepo struct {
	port.DeptMembershipRepository
	dms []domain.DeptMembership
	err error
}

func (r *elDeptMemRepo) ListByUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return r.dms, r.err
}

type eligibilityFixture struct {
	mem *elMemRepo
	td  *elTenantDeptRepo
	dm  *elDeptMemRepo
}

// newEligibilityFixture starts from the eligible case: active member, active
// department, holding `held` in deptID.
func newEligibilityFixture(deptID uuid.UUID, held domain.DeptRole) *eligibilityFixture {
	return &eligibilityFixture{
		mem: &elMemRepo{m: &domain.TenantMembership{ID: uuid.New(), Status: domain.MembershipActive}},
		td:  &elTenantDeptRepo{td: &domain.TenantDepartment{DepartmentID: deptID, IsActive: true}},
		dm:  &elDeptMemRepo{dms: []domain.DeptMembership{{DepartmentID: deptID, RoleLevel: held}}},
	}
}

func (f *eligibilityFixture) svc() *service.DeptMembershipService {
	return service.NewDeptMembershipService(f.dm, f.mem, f.td, nil, nil, nil, nil, nil)
}

func (f *eligibilityFixture) check(t *testing.T, deptID uuid.UUID, required domain.DeptRole) *domain.AssigneeEligibility {
	t.Helper()
	res, err := f.svc().CheckEligibility(context.Background(), uuid.New(), uuid.New(), deptID, required)
	require.NoError(t, err)
	require.NotNil(t, res)
	return res
}

func TestCheckEligibility_ActiveMemberAtRequiredLevel_Eligible(t *testing.T) {
	deptID := uuid.New()
	res := newEligibilityFixture(deptID, domain.DeptReviewer).check(t, deptID, domain.DeptReviewer)
	assert.True(t, res.Eligible)
	assert.Empty(t, res.Reason)
	assert.Equal(t, domain.DeptReviewer, res.RoleLevel)
}

// The level order is owned here: a held level satisfies every required level
// at or below it, and none above it.
func TestCheckEligibility_LevelOrderMatrix(t *testing.T) {
	levels := []domain.DeptRole{domain.DeptPreparator, domain.DeptReviewer, domain.DeptApprover}
	for hi, held := range levels {
		for ri, required := range levels {
			t.Run(string(held)+"_for_"+string(required), func(t *testing.T) {
				deptID := uuid.New()
				res := newEligibilityFixture(deptID, held).check(t, deptID, required)
				assert.Equal(t, hi >= ri, res.Eligible)
				assert.Equal(t, held, res.RoleLevel)
				if hi < ri {
					assert.Equal(t, domain.EligibilityLevelInsufficient, res.Reason)
				}
			})
		}
	}
}

func TestCheckEligibility_NotMember(t *testing.T) {
	deptID := uuid.New()
	f := newEligibilityFixture(deptID, domain.DeptApprover)
	f.mem.m, f.mem.err = nil, domain.NewError(domain.ErrMemberNotFound, "no membership")
	res := f.check(t, deptID, domain.DeptPreparator)
	assert.False(t, res.Eligible)
	assert.Equal(t, domain.EligibilityNotMember, res.Reason)
}

func TestCheckEligibility_SuspendedOrLeftMember_NotActive(t *testing.T) {
	for _, st := range []domain.MembershipStatus{domain.MembershipSuspended, domain.MembershipLeft} {
		t.Run(string(st), func(t *testing.T) {
			deptID := uuid.New()
			f := newEligibilityFixture(deptID, domain.DeptApprover)
			f.mem.m.Status = st
			res := f.check(t, deptID, domain.DeptPreparator)
			assert.False(t, res.Eligible)
			assert.Equal(t, domain.EligibilityMemberNotActive, res.Reason)
		})
	}
}

// A deactivated department keeps its dept_memberships rows, so this must be
// checked explicitly — the user still "holds" approver there.
func TestCheckEligibility_DepartmentDeactivated_NotActive(t *testing.T) {
	deptID := uuid.New()
	f := newEligibilityFixture(deptID, domain.DeptApprover)
	f.td.td.IsActive = false
	res := f.check(t, deptID, domain.DeptPreparator)
	assert.False(t, res.Eligible)
	assert.Equal(t, domain.EligibilityDepartmentNotActive, res.Reason)
}

func TestCheckEligibility_DepartmentNeverActivated_NotActive(t *testing.T) {
	deptID := uuid.New()
	f := newEligibilityFixture(deptID, domain.DeptApprover)
	f.td.td, f.td.err = nil, domain.NewError(domain.ErrDepartmentNotFound, "not activated")
	res := f.check(t, deptID, domain.DeptPreparator)
	assert.False(t, res.Eligible)
	assert.Equal(t, domain.EligibilityDepartmentNotActive, res.Reason)
}

func TestCheckEligibility_MemberOfOtherDepartmentOnly_NotInDepartment(t *testing.T) {
	deptID := uuid.New()
	f := newEligibilityFixture(uuid.New(), domain.DeptApprover) // approver elsewhere
	f.td.td.DepartmentID = deptID
	res := f.check(t, deptID, domain.DeptPreparator)
	assert.False(t, res.Eligible)
	assert.Equal(t, domain.EligibilityNotInDepartment, res.Reason)
	assert.Empty(t, res.RoleLevel)
}

func TestCheckEligibility_InvalidRequiredLevel_ValidationError(t *testing.T) {
	for _, lvl := range []domain.DeptRole{"", "admin", "APPROVER"} {
		t.Run(string(lvl), func(t *testing.T) {
			deptID := uuid.New()
			_, err := newEligibilityFixture(deptID, domain.DeptApprover).svc().
				CheckEligibility(context.Background(), uuid.New(), uuid.New(), deptID, lvl)
			require.Error(t, err)
			assert.ErrorIs(t, err, domain.ErrValidation)
		})
	}
}

func TestCheckEligibility_RepoErrorsPropagate(t *testing.T) {
	boom := errors.New("db down")
	cases := map[string]func(f *eligibilityFixture){
		"membership":      func(f *eligibilityFixture) { f.mem.m, f.mem.err = nil, boom },
		"tenant_dept":     func(f *eligibilityFixture) { f.td.td, f.td.err = nil, boom },
		"dept_membership": func(f *eligibilityFixture) { f.dm.dms, f.dm.err = nil, boom },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			deptID := uuid.New()
			f := newEligibilityFixture(deptID, domain.DeptApprover)
			breakIt(f)
			res, err := f.svc().CheckEligibility(context.Background(), uuid.New(), uuid.New(), deptID, domain.DeptReviewer)
			assert.ErrorIs(t, err, boom, "a dependency failure must surface, never read as not-eligible")
			assert.Nil(t, res)
		})
	}
}
