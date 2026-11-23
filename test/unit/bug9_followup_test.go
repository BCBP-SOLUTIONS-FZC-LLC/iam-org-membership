// BUG-9 follow-up (TM-8 L881): the owner guards on P-8, P-28 and P-7 must
// count only owners whose membership is active (the I-5 escalation tests
// are in bug9_followup_i5_test.go). The fakes here keep the two owner counts apart:
// CountActiveOwners (owner role rows, suspended owners included) and
// CountActiveOwnersActive (the TM-8 definition), so a suspended co-owner
// is visible to the test.
package unit_test

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// b9Roles is a TenantRoleRepository whose two owner counts are separate.
// active is returned call by call (the last value repeats).
type b9Roles struct {
	targetIsOwner bool
	total         int
	active        []int
	activeCalls   int
	softDeleted   bool
	revoked       []domain.TenantRoleCode
}

func (r *b9Roles) ListByUser(_ context.Context, tid, uid uuid.UUID) ([]domain.TenantRole, error) {
	if !r.targetIsOwner {
		return nil, nil
	}
	return []domain.TenantRole{{TenantID: tid, UserID: uid, RoleCode: domain.RoleTenantOwner}}, nil
}
func (r *b9Roles) ListByUsers(context.Context, uuid.UUID, []uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *b9Roles) ListByRole(context.Context, uuid.UUID, domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *b9Roles) CountActiveOwners(context.Context, uuid.UUID) (int, error) { return r.total, nil }
func (r *b9Roles) CountActiveOwnersActive(context.Context, uuid.UUID) (int, error) {
	i := r.activeCalls
	if i >= len(r.active) {
		i = len(r.active) - 1
	}
	r.activeCalls++
	return r.active[i], nil
}
func (r *b9Roles) Grant(_ context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
	out := *tr
	return &out, nil
}
func (r *b9Roles) Revoke(_ context.Context, tid, uid uuid.UUID, code domain.TenantRoleCode) (*domain.TenantRole, error) {
	r.revoked = append(r.revoked, code)
	return &domain.TenantRole{TenantID: tid, UserID: uid, RoleCode: code}, nil
}
func (r *b9Roles) SoftDeleteAllForUser(_ context.Context, tid, uid uuid.UUID) ([]domain.TenantRole, error) {
	r.softDeleted = true
	if !r.targetIsOwner {
		return nil, nil
	}
	return []domain.TenantRole{{TenantID: tid, UserID: uid, RoleCode: domain.RoleTenantOwner}}, nil
}

var _ port.TenantRoleRepository = (*b9Roles)(nil)

// b9Members is a MembershipRepository whose FindByUserID returns the
// given statuses call by call (the last value repeats); notFound makes it
// return ErrMemberNotFound (an I-5 replay after the first removal).
type b9Members struct {
	statuses       []domain.MembershipStatus
	findCalls      int
	notFound       bool
	setStatusCalls int
	softDeleted    bool
}

func (m *b9Members) List(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
	return &domain.MembershipListPage{}, nil
}
func (m *b9Members) FindByUserID(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
	if m.notFound {
		return nil, domain.NewError(domain.ErrMemberNotFound, "member not found")
	}
	i := m.findCalls
	if i >= len(m.statuses) {
		i = len(m.statuses) - 1
	}
	m.findCalls++
	return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid, Status: m.statuses[i], RecordVersion: 1}, nil
}
func (m *b9Members) Insert(_ context.Context, tm *domain.TenantMembership) (*domain.TenantMembership, error) {
	return tm, nil
}
func (m *b9Members) SetStatus(_ context.Context, tid, uid uuid.UUID, st domain.MembershipStatus, v int64) (*domain.TenantMembership, error) {
	m.setStatusCalls++
	return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: st, RecordVersion: v + 1}, nil
}
func (m *b9Members) SoftDelete(context.Context, uuid.UUID, uuid.UUID, int64) error {
	m.softDeleted = true
	return nil
}
func (m *b9Members) CountActive(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (m *b9Members) ListActiveUserIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

var _ port.MembershipRepository = (*b9Members)(nil)

func b9NoDepts() *ruDeptMemRepo {
	return &ruDeptMemRepo{softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
		return nil, nil
	}}
}

func b9MembershipSvc(m port.MembershipRepository, r port.TenantRoleRepository) *service.MembershipService {
	return service.NewMembershipService(m, r, b9NoDepts(), &port.TenantRepositoryNoop{}, nil, nil,
		&fakeRPClient{}, nil, &ruTxRunner{}, nil)
}

func b9Statuses(s ...domain.MembershipStatus) []domain.MembershipStatus { return s }

const (
	b9Active    = domain.MembershipActive
	b9Suspended = domain.MembershipSuspended
)

// ── P-8 RemoveUser ──────────────────────────────────────────────────────

// RED before the fix: owner A (active) is removed while the only other owner B is suspended.
func TestBUG9F_P8_RemoveActiveOwner_OtherSuspended_Refused(t *testing.T) {
	m := &b9Members{statuses: b9Statuses(b9Active)}
	r := &b9Roles{targetIsOwner: true, total: 2, active: []int{1}}
	err := b9MembershipSvc(m, r).RemoveUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrLastOwnerRemoval, "TM-8: A is the last ACTIVE owner")
	assert.False(t, r.softDeleted, "refused before the cascade")
	assert.Zero(t, m.setStatusCalls, "membership not changed")
}

// Control: the suspended owner B is removed while A is active.
func TestBUG9F_P8_RemoveSuspendedOwner_OtherActive_Succeeds(t *testing.T) {
	m := &b9Members{statuses: b9Statuses(b9Suspended)}
	r := &b9Roles{targetIsOwner: true, total: 2, active: []int{1}}
	err := b9MembershipSvc(m, r).RemoveUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.True(t, r.softDeleted)
}

// Control: two active owners, one is removed.
func TestBUG9F_P8_RemoveOneOfTwoActiveOwners_Succeeds(t *testing.T) {
	m := &b9Members{statuses: b9Statuses(b9Active)}
	r := &b9Roles{targetIsOwner: true, total: 2, active: []int{2}}
	require.NoError(t, b9MembershipSvc(m, r).RemoveUser(context.Background(), uuid.New(), uuid.New(), uuid.New()))
	assert.True(t, r.softDeleted)
}

// Control (unchanged rule): the only owner is suspended and is removed → still refused.
func TestBUG9F_P8_RemoveOnlyOwnerSuspended_StillRefused(t *testing.T) {
	m := &b9Members{statuses: b9Statuses(b9Suspended)}
	r := &b9Roles{targetIsOwner: true, total: 1, active: []int{0}}
	err := b9MembershipSvc(m, r).RemoveUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrLastOwnerRemoval)
	assert.False(t, r.softDeleted)
}

// ── P-28 ReconcileRoles (strip tenant_owner) ────────────────────────────

// RED before the fix: strip owner from A (active) while the only other owner B is suspended.
func TestBUG9F_P28_StripActiveOwner_OtherSuspended_Refused(t *testing.T) {
	m := &b9Members{statuses: b9Statuses(b9Active)}
	r := &b9Roles{targetIsOwner: true, total: 2, active: []int{1}}
	_, _, err := b9MembershipSvc(m, r).ReconcileRoles(context.Background(), uuid.New(), uuid.New(), nil, uuid.New())
	assert.ErrorIs(t, err, domain.ErrLastOwnerRemoval, "TM-8: A is the last ACTIVE owner")
	assert.Empty(t, r.revoked, "nothing revoked")
}

// Control: strip owner from the suspended owner B while A is active.
func TestBUG9F_P28_StripSuspendedOwner_OtherActive_Succeeds(t *testing.T) {
	m := &b9Members{statuses: b9Statuses(b9Suspended)}
	r := &b9Roles{targetIsOwner: true, total: 2, active: []int{1}}
	_, revoked, err := b9MembershipSvc(m, r).ReconcileRoles(context.Background(), uuid.New(), uuid.New(), nil, uuid.New())
	require.NoError(t, err)
	require.Len(t, revoked, 1)
	assert.Equal(t, domain.RoleTenantOwner, revoked[0].RoleCode)
}

// Control (guards the in-lock re-read): the target looked active before the
// lock (2 active owners then) but was suspended before the tx took it; inside
// the lock the target is suspended and 1 active owner is left → allowed.
func TestBUG9F_P28_TargetSuspendedInsideTx_ActiveOutside_Succeeds(t *testing.T) {
	m := &b9Members{statuses: b9Statuses(b9Active, b9Suspended)}
	r := &b9Roles{targetIsOwner: true, total: 2, active: []int{2, 1}}
	_, revoked, err := b9MembershipSvc(m, r).ReconcileRoles(context.Background(), uuid.New(), uuid.New(), nil, uuid.New())
	require.NoError(t, err)
	assert.Len(t, revoked, 1)
}

// Control: two active owners, owner stripped from one.
func TestBUG9F_P28_StripOneOfTwoActiveOwners_Succeeds(t *testing.T) {
	m := &b9Members{statuses: b9Statuses(b9Active)}
	r := &b9Roles{targetIsOwner: true, total: 2, active: []int{2}}
	_, revoked, err := b9MembershipSvc(m, r).ReconcileRoles(context.Background(), uuid.New(), uuid.New(), nil, uuid.New())
	require.NoError(t, err)
	assert.Len(t, revoked, 1)
}

// ── P-7 SetStatus (suspend) ─────────────────────────────────────────────

// RED before the fix: B is already suspended and is suspended again while A is the only active owner.
func TestBUG9F_P7_ResuspendSuspendedOwner_NotRefused(t *testing.T) {
	m := &b9Members{statuses: b9Statuses(b9Suspended)}
	r := &b9Roles{targetIsOwner: true, total: 2, active: []int{1}}
	res, err := b9MembershipSvc(m, r).SetStatus(context.Background(), uuid.New(), uuid.New(), b9Suspended, 3)
	require.NoError(t, err, "the active-owner count does not change, TM-8 does not apply")
	assert.Equal(t, b9Suspended, res.Membership.Status)
	assert.EqualValues(t, 4, res.Membership.RecordVersion, "same repo call as the non-owner path")
	assert.Equal(t, 1, m.setStatusCalls)
}

// Control (BUG-9): active A is suspended while the only other owner B is suspended → 422.
func TestBUG9F_P7_SuspendActiveOwner_OtherSuspended_Refused(t *testing.T) {
	m := &b9Members{statuses: b9Statuses(b9Active)}
	r := &b9Roles{targetIsOwner: true, total: 2, active: []int{1}}
	_, err := b9MembershipSvc(m, r).SetStatus(context.Background(), uuid.New(), uuid.New(), b9Suspended, 3)
	assert.ErrorIs(t, err, domain.ErrLastOwnerRemoval)
	assert.Zero(t, m.setStatusCalls)
}

// Control: active A is suspended while another owner is active.
func TestBUG9F_P7_SuspendActiveOwner_OtherActive_Succeeds(t *testing.T) {
	m := &b9Members{statuses: b9Statuses(b9Active)}
	r := &b9Roles{targetIsOwner: true, total: 2, active: []int{2}}
	_, err := b9MembershipSvc(m, r).SetStatus(context.Background(), uuid.New(), uuid.New(), b9Suspended, 3)
	require.NoError(t, err)
	assert.Equal(t, 1, m.setStatusCalls)
}
