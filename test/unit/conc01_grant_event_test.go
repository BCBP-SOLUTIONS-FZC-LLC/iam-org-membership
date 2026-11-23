// NEW-CONC-01 (EVT-10, LLD L2650: "No event is emitted without its DB
// change"): P-28 ReconcileRoles emits TenantRoleGranted only when this call
// really inserted the role row. Grant uses INSERT … ON CONFLICT DO NOTHING
// and returns the EXISTING row when a concurrent identical call won the
// race; that row has a different ID than the one this call asked for.
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

// c1Roles is a TenantRoleRepository for P-28 grant tests. held are the
// roles the user already has; for a code in lost, Grant returns an existing
// row with a different ID (another call inserted it first).
type c1Roles struct {
	held       []domain.TenantRoleCode
	lost       map[domain.TenantRoleCode]bool
	grantCalls int
}

func (r *c1Roles) ListByUser(_ context.Context, tid, uid uuid.UUID) ([]domain.TenantRole, error) {
	out := make([]domain.TenantRole, 0, len(r.held))
	for _, c := range r.held {
		out = append(out, domain.TenantRole{ID: uuid.New(), TenantID: tid, UserID: uid, RoleCode: c})
	}
	return out, nil
}
func (r *c1Roles) ListByUsers(context.Context, uuid.UUID, []uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *c1Roles) ListByRole(context.Context, uuid.UUID, domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *c1Roles) CountActiveOwners(context.Context, uuid.UUID) (int, error)       { return 2, nil }
func (r *c1Roles) CountActiveOwnersActive(context.Context, uuid.UUID) (int, error) { return 2, nil }
func (r *c1Roles) Grant(_ context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
	r.grantCalls++
	out := *tr
	if r.lost[tr.RoleCode] {
		out.ID = uuid.New() // the row a concurrent call inserted
	}
	return &out, nil
}
func (r *c1Roles) Revoke(_ context.Context, tid, uid uuid.UUID, code domain.TenantRoleCode) (*domain.TenantRole, error) {
	return &domain.TenantRole{TenantID: tid, UserID: uid, RoleCode: code}, nil
}
func (r *c1Roles) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}

var _ port.TenantRoleRepository = (*c1Roles)(nil)

// c1Run calls ReconcileRoles and returns the granted rows and the
// TenantRoleGranted events enqueued in the transaction.
func c1Run(t *testing.T, roles *c1Roles, desired ...domain.TenantRoleCode) ([]domain.TenantRole, []*domain.DomainEvent) {
	t.Helper()
	pub := &ruPublisher{}
	m := &b9Members{statuses: b9Statuses(b9Active)}
	svc := service.NewMembershipService(m, roles, b9NoDepts(), &port.TenantRepositoryNoop{}, nil, nil,
		&fakeRPClient{}, nil, &ruTxRunner{pub: pub}, nil)
	granted, _, err := svc.ReconcileRoles(context.Background(), uuid.New(), uuid.New(), desired, uuid.New())
	require.NoError(t, err, "P-28 still succeeds")
	var evts []*domain.DomainEvent
	for _, e := range pub.events {
		if e.Type == domain.EventTenantRoleGranted {
			evts = append(evts, e)
		}
	}
	return granted, evts
}

// RED before the fix: Grant returned the row another call inserted → no event, not in granted.
func TestCONC01_LostRace_NoGrantedEvent(t *testing.T) {
	roles := &c1Roles{lost: map[domain.TenantRoleCode]bool{domain.RoleTenantAdmin: true}}
	granted, evts := c1Run(t, roles, domain.RoleTenantAdmin)
	assert.Equal(t, 1, roles.grantCalls)
	assert.Empty(t, evts, "EVT-10: no TenantRoleGranted without this call's DB change")
	assert.Empty(t, granted, "a role this call did not insert is not reported as granted by it")
}

// Control: Grant inserted the row (same ID) → exactly one event.
func TestCONC01_WonRace_OneGrantedEvent(t *testing.T) {
	roles := &c1Roles{}
	granted, evts := c1Run(t, roles, domain.RoleTenantAdmin)
	require.Len(t, evts, 1)
	assert.Equal(t, domain.RoleTenantAdmin, evts[0].Data.(domain.TenantRoleGrantedPayload).RoleCode)
	assert.Len(t, granted, 1)
}

// Control: the role is already held → no Grant call, no event.
func TestCONC01_AlreadyHeld_NoGrantNoEvent(t *testing.T) {
	roles := &c1Roles{held: []domain.TenantRoleCode{domain.RoleTenantAdmin}}
	granted, evts := c1Run(t, roles, domain.RoleTenantAdmin)
	assert.Zero(t, roles.grantCalls)
	assert.Empty(t, evts)
	assert.Empty(t, granted)
}

// RED before the fix: two new roles, one lost the race → exactly one event, for the other role.
func TestCONC01_TwoRolesOneLost_ExactlyOneEvent(t *testing.T) {
	roles := &c1Roles{lost: map[domain.TenantRoleCode]bool{domain.RoleTenantAdmin: true}}
	granted, evts := c1Run(t, roles, domain.RoleTenantAdmin, domain.RoleTenderAdmin)
	assert.Equal(t, 2, roles.grantCalls)
	require.Len(t, evts, 1)
	assert.Equal(t, domain.RoleTenderAdmin, evts[0].Data.(domain.TenantRoleGrantedPayload).RoleCode)
	require.Len(t, granted, 1)
	assert.Equal(t, domain.RoleTenderAdmin, granted[0].RoleCode)
}
