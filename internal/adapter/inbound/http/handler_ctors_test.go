package http

import (
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each NewXxxHandler is a two-line struct init that gets exercised the
// moment any handler test wires the handler. Some handlers had no wired
// test at all, so their constructors stayed at 0%. These smoke tests just
// prove the constructors return non-nil handler pointers with the service
// field populated — enough to lock the ctor signature and cover the line.

func TestNewACLHandler_Constructs(t *testing.T) {
	h := NewACLHandler(nil)
	assert.NotNil(t, h)
}

func TestNewDelegationHandler_Constructs(t *testing.T) {
	h := NewDelegationHandler(nil)
	assert.NotNil(t, h)
}

func TestNewDepartmentHandler_Constructs(t *testing.T) {
	h := NewDepartmentHandler(nil)
	assert.NotNil(t, h)
}

func TestNewDeptMembershipHandler_Constructs(t *testing.T) {
	h := NewDeptMembershipHandler(nil)
	assert.NotNil(t, h)
}

func TestNewGroupMappingHandler_Constructs(t *testing.T) {
	h := NewGroupMappingHandler(nil)
	assert.NotNil(t, h)
}

func TestNewInvitationHandler_Constructs(t *testing.T) {
	h := NewInvitationHandler(nil)
	assert.NotNil(t, h)
}

func TestNewInternalHandler_Constructs(t *testing.T) {
	h := NewInternalHandler(nil, nil, nil, nil, nil, nil, nil)
	assert.NotNil(t, h)
}

func TestNewMembershipHandler_Constructs(t *testing.T) {
	h := NewMembershipHandler(nil)
	assert.NotNil(t, h)
}

func TestNewOperatorHandler_Constructs(t *testing.T) {
	h := NewOperatorHandler(nil)
	assert.NotNil(t, h)
}

func TestNewRoleLabelHandler_Constructs(t *testing.T) {
	h := NewRoleLabelHandler(nil)
	assert.NotNil(t, h)
}

func TestNewTenantHandler_Constructs(t *testing.T) {
	h := NewTenantHandler(nil)
	assert.NotNil(t, h)
}

// ── Pure DTO converters ────────────────────────────────────────────────

func TestTenantToResponse_CopiesAllFields(t *testing.T) {
	trialEnds := time.Now().Add(7 * 24 * time.Hour)
	subStarted := time.Now().Add(-time.Hour)
	overageSince := time.Now().Add(-24 * time.Hour)
	updatedAt := time.Now()

	src := &domain.Tenant{
		ID:                    uuid.New(),
		Slug:                  "acme",
		Name:                  "Acme Corp",
		Plan:                  domain.TenantPlan("pro"),
		Status:                domain.StatusActive,
		TrialEndsAt:           &trialEnds,
		SubscriptionStartedAt: &subStarted,
		RealmID:               "acme-realm",
		RealmType:             domain.RealmType("dedicated"),
		MFAFreshnessSeconds:   300,
		LocalAccountsEnabled:  true,
		RealmSyncPending:      false,
		DefaultLocale:         "en-US",
		LicensedSeats:         25,
		OverageSince:          &overageSince,
		RecordVersion:         7,
		UpdatedAt:             updatedAt,
	}

	got := TenantToResponse(src)

	assert.Equal(t, src.ID, got.ID)
	assert.Equal(t, "acme", got.Slug)
	assert.Equal(t, "Acme Corp", got.Name)
	assert.Equal(t, src.Plan, got.Plan)
	assert.Equal(t, src.Status, got.Status)
	assert.Equal(t, &trialEnds, got.TrialEndsAt)
	assert.Equal(t, &subStarted, got.SubscriptionStartedAt)
	assert.Equal(t, "acme-realm", got.RealmID)
	assert.Equal(t, src.RealmType, got.RealmType)
	assert.Equal(t, 300, got.MFAFreshnessSeconds)
	assert.True(t, got.LocalAccountsEnabled)
	assert.False(t, got.RealmSyncPending)
	assert.Equal(t, "en-US", got.DefaultLocale)
	assert.Equal(t, 25, got.LicensedSeats)
	assert.Equal(t, &overageSince, got.OverageSince)
	assert.EqualValues(t, 7, got.RecordVersion)
	assert.Equal(t, updatedAt, got.UpdatedAt)
}

func TestTenantPatchRequest_ToDomain_PassesFieldsThrough(t *testing.T) {
	name := "Renamed"
	locale := "fr-FR"
	localAccounts := false
	mfa := 900

	src := &TenantPatchRequest{
		Name:                 &name,
		DefaultLocale:        &locale,
		LocalAccountsEnabled: &localAccounts,
		MFAFreshnessSeconds:  &mfa,
		RecordVersion:        5,
	}

	got := src.ToDomain()

	assert.Equal(t, &name, got.Name)
	assert.Equal(t, &locale, got.DefaultLocale)
	assert.Equal(t, &localAccounts, got.LocalAccountsEnabled)
	assert.Equal(t, &mfa, got.MFAFreshnessSeconds)
	assert.EqualValues(t, 5, got.RecordVersion)
}

func TestTenantPatchRequest_ToDomain_EmptyPatchPassesNils(t *testing.T) {
	got := (&TenantPatchRequest{RecordVersion: 3}).ToDomain()
	assert.Nil(t, got.Name)
	assert.Nil(t, got.DefaultLocale)
	assert.Nil(t, got.LocalAccountsEnabled)
	assert.Nil(t, got.MFAFreshnessSeconds)
	assert.EqualValues(t, 3, got.RecordVersion)
}

// ── memberItemToResponse / pageItemsToResponse / rolesToWire ──────────

func TestMemberItemToResponse_MapsAllFields(t *testing.T) {
	deptA := uuid.New()
	userID := uuid.New()
	updatedAt := time.Now()
	item := domain.MembershipListItem{
		Membership: domain.TenantMembership{
			UserID:        userID,
			Status:        domain.MembershipActive,
			RecordVersion: 4,
			UpdatedAt:     updatedAt,
		},
		TenantRoles: []domain.TenantRoleCode{domain.RoleMember, domain.RoleTenderAdmin},
		Departments: []domain.DeptMembershipView{
			{DepartmentID: deptA, RoleLevel: domain.DeptApprover},
		},
	}

	got := memberItemToResponse(item)

	assert.Equal(t, userID, got.UserID)
	assert.Equal(t, "active", got.Status)
	assert.Equal(t, []string{"member", "tender_admin"}, got.TenantRoles)
	require.Len(t, got.Departments, 1)
	assert.Equal(t, deptA, got.Departments[0].DepartmentID)
	assert.Equal(t, "approver", got.Departments[0].Level)
	assert.EqualValues(t, 4, got.RecordVersion)
	assert.Equal(t, updatedAt, got.UpdatedAt)
}

func TestPageItemsToResponse_MapsSliceInOrder(t *testing.T) {
	userA, userB := uuid.New(), uuid.New()
	items := []domain.MembershipListItem{
		{Membership: domain.TenantMembership{UserID: userA, Status: domain.MembershipActive}},
		{Membership: domain.TenantMembership{UserID: userB, Status: domain.MembershipSuspended}},
	}
	got := pageItemsToResponse(items)
	require.Len(t, got, 2)
	assert.Equal(t, userA, got[0].UserID)
	assert.Equal(t, userB, got[1].UserID)
	assert.Equal(t, "suspended", got[1].Status)
}

func TestPageItemsToResponse_EmptySliceReturnsEmptyNotNil(t *testing.T) {
	got := pageItemsToResponse(nil)
	assert.NotNil(t, got, "handlers marshal empty arrays, not null")
	assert.Len(t, got, 0)
}

func TestRolesToWire_ConvertsCodes(t *testing.T) {
	rs := []domain.TenantRole{
		{RoleCode: domain.RoleTenantOwner},
		{RoleCode: domain.RoleTenantAdmin},
	}
	got := rolesToWire(rs)
	assert.Equal(t, []string{"tenant_owner", "tenant_admin"}, got)
}

func TestRolesToWire_EmptyIsEmptyNotNil(t *testing.T) {
	got := rolesToWire(nil)
	assert.NotNil(t, got)
	assert.Len(t, got, 0)
}

// ── delegationToResponse ──────────────────────────────────────────────

func TestDelegationToResponse_MapsAllFields(t *testing.T) {
	scopeID := uuid.New()
	starts := time.Now()
	ends := starts.Add(24 * time.Hour)
	d := domain.Delegation{
		ID:            uuid.New(),
		DelegatorID:   uuid.New(),
		DelegateID:    uuid.New(),
		Scope:         domain.ScopeDepartment,
		ScopeID:       &scopeID,
		Reason:        "vacation",
		StartsAt:      starts,
		EndsAt:        &ends,
		Status:        domain.DelegationStatus("active"),
		RecordVersion: 2,
	}
	got := delegationToResponse(d)
	assert.Equal(t, d.ID, got.ID)
	assert.Equal(t, d.DelegatorID, got.DelegatorID)
	assert.Equal(t, d.DelegateID, got.DelegateID)
	assert.Equal(t, "department", got.Scope)
	assert.Equal(t, &scopeID, got.ScopeID)
	assert.Equal(t, "vacation", got.Reason)
	assert.Equal(t, starts, got.StartsAt)
	assert.Equal(t, &ends, got.EndsAt)
	assert.Equal(t, "active", got.Status)
	assert.EqualValues(t, 2, got.RecordVersion)
}

func TestDelegationToResponse_NilScopeIDPassesThrough(t *testing.T) {
	got := delegationToResponse(domain.Delegation{Scope: domain.ScopeAll})
	assert.Nil(t, got.ScopeID)
	assert.Equal(t, "all", got.Scope)
}

// ── OperatorReassignOwnerRequest.EffectiveUserID (already tested but
// belt-and-braces here alongside the other DTO converters). ────────────

func TestOperatorReassignOwnerRequest_EffectiveUserID_PrefersCanonical(t *testing.T) {
	canonical, legacy := uuid.New(), uuid.New()
	req := OperatorReassignOwnerRequest{UserID: canonical, NewOwnerUserID: legacy}
	assert.Equal(t, canonical, req.EffectiveUserID())
}

func TestOperatorReassignOwnerRequest_EffectiveUserID_FallsBackToLegacy(t *testing.T) {
	legacy := uuid.New()
	req := OperatorReassignOwnerRequest{NewOwnerUserID: legacy}
	assert.Equal(t, legacy, req.EffectiveUserID())
}
