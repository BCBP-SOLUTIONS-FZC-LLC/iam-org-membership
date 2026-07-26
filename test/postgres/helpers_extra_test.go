//go:build integration

// Phase 4 service-integration test helpers.
//
// Provides testFixtures that wires the full service graph (repos + TxRunner
// + outbox publisher) against the testcontainers Postgres from setupTestDB.
// Individual tests just call fx.Provisioning.TrialSignup(...) etc, then
// query outbox_events / dept_memberships / whatever to assert observable
// side-effects.
//
// Design note: we wire the REAL outbox publisher (via eventbusadapter.New
// with NoopCodec) so event enqueues actually land in outbox_events. We do
// NOT wire the outbox RUNNER (the goroutine that drains outbox → SNS) —
// that would introduce non-determinism. Tests inspect outbox_events
// directly to verify what a downstream SNS subscriber would eventually see.

package postgres_test

import (
	"context"
	"errors"
	"testing"

	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/eventbus"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// testFixtures bundles a fully wired service graph plus the two pools so
// tests can seed data via rawPool (BYPASSRLS) and exercise services via
// the RLS-enforcing appPool.
type testFixtures struct {
	appPool *pgcommon.Pool
	rawPool *pgxpool.Pool

	// Repos (all directly usable in assertions).
	Tenants     port.TenantRepository
	Memberships port.MembershipRepository
	Roles       port.TenantRoleRepository
	DeptMems    port.DeptMembershipRepository
	Labels      port.DeptRoleLabelRepository
	TenantDepts port.TenantDepartmentRepository
	Depts       port.DepartmentRepository
	Delegations port.DelegationRepository
	ACLs        port.TenderACLRepository
	Plans       port.PlanRepository
	GroupMaps   port.GroupMappingRepository
	Invitations port.InvitationRepository

	// Services under test.
	Provisioning   *service.ProvisioningService
	Membership     *service.MembershipService
	DeptMembership *service.DeptMembershipService
	Operator       *service.OperatorService
	GroupMapping   *service.GroupMappingService
	Invitation     *service.InvitationService
	AuthZ          *service.AuthZService
	Delegation     *service.DelegationService
	Tenant         *service.TenantService
	Department     *service.DepartmentService
	RoleLabel      *service.RoleLabelService

	// Overridable stubs — tests can reach in and swap behaviour (e.g.
	// fx.UP.FailNext = true to simulate a UP outage for CONS-2 tests).
	UP *fakeUserProfile
	RP *fakeRealmProvisioner
}

// buildTestFixtures wires everything on top of the containers Postgres.
// Fake outbound clients are used for RP/UP/Workflow — they always return
// no-op results so the service flows don't fail on external dependencies.
func buildTestFixtures(t testing.TB) *testFixtures {
	t.Helper()
	appPool, rawPool := setupTestDB(t)

	tenants := pgadapter.NewTenantRepository(appPool)
	memberships := pgadapter.NewMembershipRepository(appPool)
	roles := pgadapter.NewTenantRoleRepository(appPool)
	deptMems := pgadapter.NewDeptMembershipRepository(appPool)
	labels := pgadapter.NewDeptRoleLabelRepository(appPool)
	tenantDepts := pgadapter.NewTenantDepartmentRepository(appPool)
	depts := pgadapter.NewDepartmentRepository(appPool)
	delegations := pgadapter.NewDelegationRepository(appPool)
	acls := pgadapter.NewTenderACLRepository(appPool)
	plans := pgadapter.NewPlanRepository(appPool)
	groupMaps := pgadapter.NewGroupMappingRepository(appPool)
	invitations := pgadapter.NewInvitationRepository(appPool)

	// Outbox publisher — writes to outbox_events table on EnqueueCtx.
	// NoopCodec passes payloads through as JSON bytes.
	codec, err := eventbusadapter.NewValidatingCodec(eventbusadapter.NoopCodec{})
	require.NoError(t, err)
	outboxPub := eventbusadapter.New("iam-org-membership-test", codec)

	// TxRunner injects a tx-bound event publisher into ctx.
	txRunner := pgadapter.NewTxRunner(appPool, outboxPub)

	fx := &testFixtures{
		appPool:     appPool,
		rawPool:     rawPool,
		Tenants:     tenants,
		Memberships: memberships,
		Roles:       roles,
		DeptMems:    deptMems,
		Labels:      labels,
		TenantDepts: tenantDepts,
		Depts:       depts,
		Delegations: delegations,
		ACLs:        acls,
		Plans:       plans,
		GroupMaps:   groupMaps,
		Invitations: invitations,
	}

	fx.Provisioning = service.NewProvisioningService(
		appPool, tenants, memberships, roles, deptMems, labels,
		tenantDepts, depts, delegations, acls, plans,
		txRunner, nil, // cache=nil (advisory)
		&fakeRealmProvisioner{}, // no-op RP client
	)
	fx.DeptMembership = service.NewDeptMembershipService(
		deptMems, memberships, tenantDepts, delegations,
		&fakeWorkflow{}, nil, txRunner,
	)
	fx.GroupMapping = service.NewGroupMappingService(
		groupMaps, memberships, roles, deptMems, txRunner, nil,
	)
	fx.Membership = service.NewMembershipService(
		memberships, roles, deptMems, delegations, acls,
		tenants, invitations, nil, // cache
		&fakeRealmProvisioner{}, &fakeWorkflow{},
		txRunner, nil, 30, // seatOverageDays
	)
	fx.Operator = service.NewOperatorService(
		appPool, plans, depts, tenants, roles, memberships, nil, txRunner,
	)
	fx.Invitation = service.NewInvitationService(
		invitations, memberships, roles, deptMems, tenants,
		&fakeRealmProvisioner{}, nil, txRunner, nil, 7,
	)
	fx.AuthZ = service.NewAuthZService(appPool, plans, nil)
	// UP + RP clients are injected via fx.UP / fx.RP so individual tests can
	// override behaviour (fx.UP.FailNext = true / fx.RP.PatchRealmConfigFailNext = true).
	fx.UP = &fakeUserProfile{}
	fx.RP = &fakeRealmProvisioner{}
	fx.Delegation = service.NewDelegationService(
		delegations, memberships, fx.UP, txRunner,
	)
	fx.Tenant = service.NewTenantService(tenants, nil, fx.RP)
	fx.Department = service.NewDepartmentService(depts, tenantDepts, nil)
	fx.RoleLabel = service.NewRoleLabelService(labels, nil)

	return fx
}

// countOutboxEvents returns the number of rows currently in outbox_events
// for the given tenant. Uses rawPool (BYPASSRLS) so cross-tenant assertions
// remain possible.
func (fx *testFixtures) countOutboxEvents(t *testing.T, ctx context.Context, tenantID uuid.UUID) int {
	t.Helper()
	var n int
	err := fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE tenant_id = $1`,
		tenantID.String()).Scan(&n)
	require.NoError(t, err)
	return n
}

// outboxEventTypesForTenant returns every event_type row for the tenant,
// in insertion order. Duplicates preserved so per-delta counts hold.
func (fx *testFixtures) outboxEventTypesForTenant(t *testing.T, ctx context.Context, tenantID uuid.UUID) []string {
	t.Helper()
	rows, err := fx.rawPool.Query(ctx,
		`SELECT event_type FROM outbox_events
		 WHERE tenant_id = $1
		 ORDER BY created_at ASC, id ASC`,
		tenantID.String())
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var et string
		require.NoError(t, rows.Scan(&et))
		out = append(out, et)
	}
	require.NoError(t, rows.Err())
	return out
}

// ── fake outbound clients ──────────────────────────────────────────────

// fakeRealmProvisioner is a stub for tests that don't exercise the RP
// integration surface. Individual methods can be configured to fail via
// the *FailNext booleans (one-shot) so tests can exercise the T-15
// deferred-sync branch, PI-9 cleanup errors, etc.
type fakeRealmProvisioner struct {
	PatchRealmConfigFailNext bool
	CreateInvitedUserFailNext bool
	DeleteUserFailNext        bool
	RevokeUserSessionsFailNext bool

	PatchRealmConfigCalls []port.RealmConfigPatch
}

func (f *fakeRealmProvisioner) CreateInvitedUser(_ context.Context, req port.CreateInvitedUserRequest) (*port.CreateInvitedUserResponse, error) {
	if f.CreateInvitedUserFailNext {
		f.CreateInvitedUserFailNext = false
		return nil, errors.New("fake RP CreateInvitedUser outage")
	}
	return &port.CreateInvitedUserResponse{KeycloakUserID: uuid.New()}, nil
}
func (f *fakeRealmProvisioner) DeleteUser(_ context.Context, _, _ uuid.UUID) error {
	if f.DeleteUserFailNext {
		f.DeleteUserFailNext = false
		return errors.New("fake RP DeleteUser outage")
	}
	return nil
}
func (f *fakeRealmProvisioner) PatchRealmConfig(_ context.Context, _ uuid.UUID, p port.RealmConfigPatch) error {
	f.PatchRealmConfigCalls = append(f.PatchRealmConfigCalls, p)
	if f.PatchRealmConfigFailNext {
		f.PatchRealmConfigFailNext = false
		return errors.New("fake RP PatchRealmConfig outage")
	}
	return nil
}
func (f *fakeRealmProvisioner) RevokeUserSessions(_ context.Context, _, _ uuid.UUID) error {
	if f.RevokeUserSessionsFailNext {
		f.RevokeUserSessionsFailNext = false
		return errors.New("fake RP RevokeUserSessions outage")
	}
	return nil
}

// fakeUserProfile is a configurable stub for the UserProfileClient port.
// Default behaviour: SetAvailability succeeds. Tests can flip FailNext or
// set an ErrToReturn to simulate UP outages / non-2xx responses.
type fakeUserProfile struct {
	FailNext    bool
	ErrToReturn error
	Calls       []port.SetAvailabilityRequest
}

func (f *fakeUserProfile) SetAvailability(_ context.Context, req port.SetAvailabilityRequest) error {
	f.Calls = append(f.Calls, req)
	if f.FailNext {
		f.FailNext = false
		if f.ErrToReturn != nil {
			return f.ErrToReturn
		}
		return errors.New("fake UP outage")
	}
	return nil
}

// fakeWorkflow returns zero impact — matches the WFI-13 fail-open path
// so removal/dept-remove flows don't block on Workflow.
type fakeWorkflow struct{}

func (f *fakeWorkflow) GetDelegateImpact(_ context.Context, _, _ uuid.UUID, _ *uuid.UUID) (*port.DelegateImpact, error) {
	return &port.DelegateImpact{ActiveWorkflows: 0}, nil
}
func (f *fakeWorkflow) ReassignDelegate(_ context.Context, _, _, _ uuid.UUID, _ *uuid.UUID) error {
	return nil
}
func (f *fakeWorkflow) CancelByDelegate(_ context.Context, _, _ uuid.UUID, _ *uuid.UUID) error {
	return nil
}
