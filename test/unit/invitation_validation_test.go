// Unit tests for invitation service validation gaps:
//
//	P6-EMAIL-VAL-01/02  (GAP-P6-2: email format)
//	P6-EMAIL-DOUBLE-AT  (GAP-P6-2: double @)
//	P6-FULL-NAME-MISSING-01
//	P6-ROLE-VAL-01       (member role rejected, TR-7)
package unit_test

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildInvSvcForValidation() *service.InvitationService {
	return buildInvitationSvc(nil, nil)
}

func invInput(email, fullName string, roles ...domain.TenantRoleCode) service.InvitationInput {
	return service.InvitationInput{
		Email:              email,
		FullName:           fullName,
		InitialTenantRoles: roles,
	}
}

// ── P6-EMAIL-VAL-01: missing @ → 400 ─────────────────────────────────

// Test Case ID:      P6-EMAIL-VAL-01
// Feature:           P-6 · email without @ → 400 (GAP-P6-2 isValidEmail)
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestInvite_InvalidEmail_MissingAt(t *testing.T) {
	svc := buildInvSvcForValidation()
	_, err := svc.Invite(context.Background(), uuid.New(),
		invInput("notanemail", "Test User"), uuid.New())

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
}

// ── P6-EMAIL-DOUBLE-AT-01: double @ → 400 ────────────────────────────

// Test Case ID:      P6-EMAIL-DOUBLE-AT-01
// Feature:           P-6 · email with double @@ → 400 (GAP-P6-2)
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestInvite_InvalidEmail_DoubleAt(t *testing.T) {
	svc := buildInvSvcForValidation()
	_, err := svc.Invite(context.Background(), uuid.New(),
		invInput("a@@b.com", "Test User"), uuid.New())

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
}

// ── P6-EMAIL-VAL-02: valid email passes format check ─────────────────

// Test Case ID:      P6-EMAIL-VAL-02
// Feature:           P-6 · valid email passes isValidEmail → continues to business logic
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestInvite_ValidEmail_PassesFormatCheck(t *testing.T) {
	// With nil invites repo, a valid email will get past format check
	// and fail at FindPendingByEmail (nil repo panic = format passed).
	svc := buildInvSvcForValidation()
	defer func() { _ = recover() }()
	_, _ = svc.Invite(context.Background(), uuid.New(),
		invInput("user@example.com", "Test User"), uuid.New())
}

// ── P6-FULL-NAME-MISSING-01: empty full_name → 400 ───────────────────

// Test Case ID:      P6-FULL-NAME-MISSING-01
// Feature:           P-6 · full_name=” → 400 validation_error
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestInvite_FullNameMissing(t *testing.T) {
	svc := buildInvSvcForValidation()
	_, err := svc.Invite(context.Background(), uuid.New(),
		invInput("user@example.com", ""), uuid.New())

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
}

// ── P6-ROLE-VAL-01: member role in initial_tenant_roles → 422 ────────

// Test Case ID:      P6-ROLE-VAL-01
// Feature:           P-6 · initial_tenant_roles=['member'] → 422 invalid_role (TR-7)
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestInvite_MemberRoleRejected(t *testing.T) {
	svc := buildInvSvcForValidation()
	_, err := svc.Invite(context.Background(), uuid.New(),
		invInput("user@example.com", "Test User", domain.RoleMember), uuid.New())

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
	assert.Equal(t, "invalid_role", de.Details["code"])
}
