// Unit tests added after manual testing session (2026-07-31).
// Covers scenarios found during P-22/P-23 manual testing that had
// coverage=no in the Excel tracker.
package unit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helper: build a TenderACLService with the given stubs.
func buildACLSvc(acl *fakeACLRepo, mem *fakeMembershipRepo) *service.TenderACLService {
	return service.NewTenderACLService(acl, mem)
}

// ── P22-HAPPY-05 ──────────────────────────────────────────────────────────

// Test Case ID:      P22-HAPPY-05
// Feature:           P-22 · reason stored in returned entry
// Scenario:          Grant with reason → 201, reason present in response body
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestTenderACLService_Grant_WithReason(t *testing.T) {
	tenantID, tenderID, userID, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID, Status: domain.MembershipActive}
	reason := "Temporary approval rights for Q3 tender review"

	acl := &fakeACLRepo{
		grantFn: func(_ context.Context, e *domain.TenderACLEntry) (*domain.TenderACLEntry, error) {
			assert.Equal(t, reason, e.Reason, "reason must be passed through to repository")
			e.ID = uuid.New()
			return e, nil
		},
	}
	mr := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return mem, nil
		},
	}
	svc := buildACLSvc(acl, mr)

	got, err := svc.Grant(context.Background(), tenantID, tenderID, userID,
		domain.ACLApprove, actorID, reason, nil)
	require.NoError(t, err)
	assert.Equal(t, reason, got.Reason)
}

// ── P22-VAL-02 ────────────────────────────────────────────────────────────

// Test Case ID:      P22-VAL-02
// Feature:           P-22 · empty access_level string → 400 invalid_access_level
// Scenario:          access_level=” (empty string) → validation_error
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestACLGrant_EmptyAccessLevel(t *testing.T) {
	svc := buildACLSvc(&fakeACLRepo{}, &fakeMembershipRepo{})

	_, err := svc.Grant(context.Background(), uuid.New(), uuid.New(), uuid.New(),
		domain.TenderACLLevel(""), uuid.New(), "", nil)

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
	assert.Equal(t, "invalid_access_level", de.Details["code"])
}

// ── P22-VAL-05 ────────────────────────────────────────────────────────────

// Test Case ID:      P22-VAL-05
// Feature:           P-22 · missing access_level field → 400 (zero-value empty string)
// Scenario:          Missing access_level → Go zero-value "" → same 400 path as VAL-02
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestACLGrant_MissingAccessLevel(t *testing.T) {
	svc := buildACLSvc(&fakeACLRepo{}, &fakeMembershipRepo{})

	// zero value of domain.TenderACLLevel is "" — same as omitting the field in JSON
	_, err := svc.Grant(context.Background(), uuid.New(), uuid.New(), uuid.New(),
		domain.TenderACLLevel(""), uuid.New(), "", nil)

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
	assert.Equal(t, "invalid_access_level", de.Details["code"])
}

// ── P22-DUP-01 / Bug B-ACL-01 ────────────────────────────────────────────

// Test Case ID:      P22-DUP-01
// Feature:           P-22 · duplicate active grant → 409 acl_already_exists (Bug B-ACL-01 fix)
// Scenario:          Repo returns ErrACLAlreadyExists (23505 uq_tae_active_entry) → service propagates → 409
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestACLGrant_DuplicateConflict(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID, Status: domain.MembershipActive}

	acl := &fakeACLRepo{
		grantFn: func(context.Context, *domain.TenderACLEntry) (*domain.TenderACLEntry, error) {
			return nil, domain.NewError(domain.ErrACLAlreadyExists,
				"active ACL grant already exists for this user on this tender")
		},
	}
	mr := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return mem, nil
		},
	}
	svc := buildACLSvc(acl, mr)

	_, err := svc.Grant(context.Background(), tenantID, uuid.New(), userID,
		domain.ACLView, uuid.New(), "", nil)

	assert.ErrorIs(t, err, domain.ErrACLAlreadyExists)
}

// ── P23-NOT-FOUND-01 ──────────────────────────────────────────────────────

// Test Case ID:      P23-NOT-FOUND-01
// Feature:           P-23 · no active grant → 404 member_not_found
// Scenario:          Revoke on user with no active grant — repo returns ErrMemberNotFound
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestACLRevoke_NotFound(t *testing.T) {
	acl := &fakeACLRepo{
		revokeFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.TenderACLEntry, error) {
			return nil, domain.NewError(domain.ErrMemberNotFound, "acl entry not found")
		},
	}
	svc := buildACLSvc(acl, &fakeMembershipRepo{})

	_, err := svc.Revoke(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrMemberNotFound)
}

// ── P23-DOUBLE-REVOKE-01 ─────────────────────────────────────────────────

// Test Case ID:      P23-DOUBLE-REVOKE-01
// Feature:           P-23 · revoke already-revoked → 404 (not idempotent)
// Scenario:          Soft-deleted row excluded by WHERE deleted_at IS NULL → 0 rows → 404
// Priority: P2 · Severity: Major · Automation Status: Automated
func TestACLRevoke_AlreadyRevoked(t *testing.T) {
	// Repository returns not-found when deleted_at IS NULL filter excludes the row.
	// Service behaviour is identical to TestACLRevoke_NotFound — both propagate ErrMemberNotFound.
	acl := &fakeACLRepo{
		revokeFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.TenderACLEntry, error) {
			return nil, domain.NewError(domain.ErrMemberNotFound, "acl entry not found")
		},
	}
	svc := buildACLSvc(acl, &fakeMembershipRepo{})

	_, err := svc.Revoke(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrMemberNotFound)
}

// ── P23-CROSS-TENDER-01 ───────────────────────────────────────────────────

// Test Case ID:      P23-CROSS-TENDER-01
// Feature:           P-23 · revoke on wrong tender → 404 (tender isolation)
// Scenario:          User has grant on tender-A, revoke targets tender-B — WHERE tender_id=$2 → 0 rows → 404
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestACLRevoke_WrongTender(t *testing.T) {
	tenderA := uuid.New()
	tenderB := uuid.New()

	acl := &fakeACLRepo{
		revokeFn: func(_ context.Context, _, tenderID, _ uuid.UUID) (*domain.TenderACLEntry, error) {
			// Simulate: user has a grant on tenderA, but we're revoking tenderB
			if tenderID == tenderB {
				return nil, domain.NewError(domain.ErrMemberNotFound, "acl entry not found")
			}
			return &domain.TenderACLEntry{ID: uuid.New()}, nil
		},
	}
	svc := buildACLSvc(acl, &fakeMembershipRepo{})

	// Revoke on the wrong tender → 404
	_, err := svc.Revoke(context.Background(), uuid.New(), tenderB, uuid.New())
	assert.ErrorIs(t, err, domain.ErrMemberNotFound)

	// Revoke on the correct tender → succeeds (sanity)
	_, err = svc.Revoke(context.Background(), uuid.New(), tenderA, uuid.New())
	require.NoError(t, err)
}

// ── P22-NO-SQS-01 / P23-NO-SQS-01 ───────────────────────────────────────

// Test Case ID:      P22-NO-SQS-01 / P23-NO-SQS-01
// Feature:           P-22/P-23 · no SQS events emitted on ACL grant or revoke
// Scenario:          TenderACLService has no EventPublisher field — ACL changes are silent per §16 A32(c)
// Priority: P2 · Severity: Minor · Automation Status: Automated
func TestACLService_NoPublisher_SilentOnGrantAndRevoke(t *testing.T) {
	// TenderACLService is constructed with only (acls, memberships) — no publisher.
	// This test proves the design: there is no publisher wired to the ACL service.
	tenantID, userID := uuid.New(), uuid.New()
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID, Status: domain.MembershipActive}
	future := time.Now().Add(24 * time.Hour)

	grantCalled, revokeCalled := false, false
	acl := &fakeACLRepo{
		grantFn: func(_ context.Context, e *domain.TenderACLEntry) (*domain.TenderACLEntry, error) {
			grantCalled = true
			e.ID = uuid.New()
			return e, nil
		},
		revokeFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.TenderACLEntry, error) {
			revokeCalled = true
			return &domain.TenderACLEntry{ID: uuid.New()}, nil
		},
	}
	mr := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return mem, nil
		},
	}
	svc := service.NewTenderACLService(acl, mr)

	_, err := svc.Grant(context.Background(), tenantID, uuid.New(), userID,
		domain.ACLView, uuid.New(), "", &future)
	require.NoError(t, err)
	assert.True(t, grantCalled, "repo.Grant must be called")

	_, err = svc.Revoke(context.Background(), tenantID, uuid.New(), userID)
	require.NoError(t, err)
	assert.True(t, revokeCalled, "repo.Revoke must be called")

	// No assertions on event emission — the service has no publisher.
	// If a publisher were accidentally added, tests in other packages that
	// check event counts would catch the regression.
}

// ── B-TAE-01: TAE-5 suspended member rejected ─────────────────────────────

// Test Case ID:      B-TAE-01
// Feature:           P-22 · Grant to suspended member → 422 member_not_active (TAE-5)
// Scenario:          Grantee membership status=suspended → service rejects before repo call
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestACLGrant_SuspendedMember_Rejected(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	suspended := &domain.TenantMembership{
		ID: uuid.New(), TenantID: tenantID, UserID: userID,
		Status: domain.MembershipSuspended,
	}
	mr := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return suspended, nil
		},
	}
	svc := buildACLSvc(&fakeACLRepo{}, mr)

	_, err := svc.Grant(context.Background(), tenantID, uuid.New(), userID,
		domain.ACLView, uuid.New(), "", nil)

	assert.ErrorIs(t, err, domain.ErrMemberNotActive)
}

// Test Case ID:      B-TAE-01 extension
// Feature:           P-22 · Active member still allowed after TAE-5 guard added
// Scenario:          Status=active passes TAE-5 and reaches repo
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestACLGrant_ActiveMember_PassesTAE5(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	active := &domain.TenantMembership{
		ID: uuid.New(), TenantID: tenantID, UserID: userID,
		Status: domain.MembershipActive,
	}
	grantCalled := false
	acl := &fakeACLRepo{
		grantFn: func(_ context.Context, e *domain.TenderACLEntry) (*domain.TenderACLEntry, error) {
			grantCalled = true
			e.ID = uuid.New()
			return e, nil
		},
	}
	mr := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return active, nil
		},
	}
	svc := buildACLSvc(acl, mr)

	_, err := svc.Grant(context.Background(), tenantID, uuid.New(), userID,
		domain.ACLView, uuid.New(), "", nil)
	require.NoError(t, err)
	assert.True(t, grantCalled, "repo.Grant must be reached for active members")
}

// ── B-TAE-02: reason > 500 chars rejected ────────────────────────────────

// Test Case ID:      B-TAE-02
// Feature:           P-22 · reason > 500 chars → 400 reason_too_long (§16 A27)
// Scenario:          501-char reason → validation_error before membership/repo call
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestACLGrant_ReasonTooLong_Rejected(t *testing.T) {
	reason501 := string(make([]byte, 501))
	for i := range reason501 {
		reason501 = reason501[:i] + "a" + reason501[i+1:]
	}
	svc := buildACLSvc(&fakeACLRepo{}, &fakeMembershipRepo{})

	_, err := svc.Grant(context.Background(), uuid.New(), uuid.New(), uuid.New(),
		domain.ACLView, uuid.New(), reason501, nil)

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
	assert.Equal(t, "reason_too_long", de.Details["code"])
}

// Test Case ID:      B-TAE-02 boundary
// Feature:           P-22 · reason exactly 500 chars → 201 (boundary allowed)
// Priority: P2 · Severity: Minor · Automation Status: Automated
func TestACLGrant_Reason500Chars_Allowed(t *testing.T) {
	reason500 := string(make([]rune, 500))
	for i := range reason500 {
		reason500 = reason500[:i] + "x" + reason500[i+1:]
	}
	tenantID, userID := uuid.New(), uuid.New()
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID, Status: domain.MembershipActive}
	acl := &fakeACLRepo{
		grantFn: func(_ context.Context, e *domain.TenderACLEntry) (*domain.TenderACLEntry, error) {
			assert.Equal(t, 500, len(e.Reason))
			e.ID = uuid.New()
			return e, nil
		},
	}
	mr := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return mem, nil
		},
	}
	svc := buildACLSvc(acl, mr)

	_, err := svc.Grant(context.Background(), tenantID, uuid.New(), userID,
		domain.ACLView, uuid.New(), reason500, nil)
	require.NoError(t, err)
}

// ── ErrACLAlreadyExists sentinel stability ────────────────────────────────

// Test Case ID:      B-ACL-01 regression guard
// Feature:           ErrACLAlreadyExists wire code stability
// Scenario:          The sentinel wire code must stay "acl_already_exists" — dashboard/client contracts depend on it
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestACLAlreadyExists_WireCodeStable(t *testing.T) {
	err := domain.NewError(domain.ErrACLAlreadyExists, "msg")
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "acl_already_exists", de.Code,
		"wire code must not be renamed — breaks client error handling")
	assert.True(t, errors.Is(err, domain.ErrACLAlreadyExists))
}
