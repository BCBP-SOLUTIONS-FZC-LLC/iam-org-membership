//go:build integration

// F5 (RP↔O&M alignment review) — RP-5 applies O&M's required_actions
// verbatim, but O&M's CreateInvitedUser call previously sent only
// {email, full_name}. This tests the fix: InvitationService.Invite now
// computes required_actions from the invite's initial_tenant_roles /
// initial_dept_mappings before calling RP (HLD §8.2.2 step 3).
package postgres_test

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

func TestInvite_RequiredActions_BaseCase_NoElevatedGrant(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, actorID := seedTenantWithOwner(t, ctx, fx, "p6-reqact-base")

	_, err := fx.Invitation.Invite(withSystemAndTenant(ctx, tenantID), tenantID, service.InvitationInput{
		Email:    "plain@example.com",
		FullName: "Plain Member",
	}, actorID)
	require.NoError(t, err)

	require.Len(t, fx.RP.CreateInvitedUserCalls, 1)
	assert.ElementsMatch(t, []string{port.RequiredActionVerifyEmail, port.RequiredActionUpdatePassword},
		fx.RP.CreateInvitedUserCalls[0].RequiredActions,
		"no elevated role/dept mapping → base actions only, no CONFIGURE_TOTP")
}

func TestInvite_RequiredActions_TenantAdminGrant_AddsConfigureTOTP(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, actorID := seedTenantWithOwner(t, ctx, fx, "p6-reqact-admin")

	_, err := fx.Invitation.Invite(withSystemAndTenant(ctx, tenantID), tenantID, service.InvitationInput{
		Email:              "admin@example.com",
		FullName:           "New Admin",
		InitialTenantRoles: []domain.TenantRoleCode{domain.RoleTenantAdmin},
	}, actorID)
	require.NoError(t, err)

	require.Len(t, fx.RP.CreateInvitedUserCalls, 1)
	assert.ElementsMatch(t,
		[]string{port.RequiredActionVerifyEmail, port.RequiredActionUpdatePassword, port.RequiredActionConfigureTOTP},
		fx.RP.CreateInvitedUserCalls[0].RequiredActions,
		"tenant_admin initial role → CONFIGURE_TOTP added (HLD §8.2.2 step 3)")
}

func TestInvite_RequiredActions_ApproverDeptMapping_AddsConfigureTOTP(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, actorID := seedTenantWithOwner(t, ctx, fx, "p6-reqact-approver")
	deptID := uuid.New()

	_, err := fx.Invitation.Invite(withSystemAndTenant(ctx, tenantID), tenantID, service.InvitationInput{
		Email:    "approver@example.com",
		FullName: "New Approver",
		InitialDeptMappings: []domain.InvitationDeptMapping{
			{DepartmentID: deptID, Level: domain.DeptApprover},
		},
	}, actorID)
	require.NoError(t, err)

	require.Len(t, fx.RP.CreateInvitedUserCalls, 1)
	assert.Contains(t, fx.RP.CreateInvitedUserCalls[0].RequiredActions, port.RequiredActionConfigureTOTP,
		"Approver-level initial dept mapping → CONFIGURE_TOTP added (HLD §8.2.2 step 3)")
}

func TestInvite_RequiredActions_ReviewerDeptMapping_NoConfigureTOTP(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, actorID := seedTenantWithOwner(t, ctx, fx, "p6-reqact-reviewer")
	deptID := uuid.New()

	_, err := fx.Invitation.Invite(withSystemAndTenant(ctx, tenantID), tenantID, service.InvitationInput{
		Email:    "reviewer@example.com",
		FullName: "New Reviewer",
		InitialDeptMappings: []domain.InvitationDeptMapping{
			{DepartmentID: deptID, Level: domain.DeptReviewer},
		},
	}, actorID)
	require.NoError(t, err)

	require.Len(t, fx.RP.CreateInvitedUserCalls, 1)
	assert.NotContains(t, fx.RP.CreateInvitedUserCalls[0].RequiredActions, port.RequiredActionConfigureTOTP,
		"reviewer (non-Approver) dept mapping must NOT trigger CONFIGURE_TOTP")
}
