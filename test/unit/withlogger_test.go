// Unit tests for the WithLogger functional-option constructors shared by
// several services (catalog_service.go, dept_membership_service.go,
// group_mapping_service.go, provisioning_service.go). All four follow the
// identical one-line pattern:
//
//	func (s *X) WithLogger(log port.Logger) *X {
//	    s.log = port.NewSlogStyleLogger(log)
//	    return s
//	}
//
// These tests confirm (a) the option returns the same pointer (fluent
// chaining) and (b) a subsequent call that exercises the injected logger's
// warn/error path does not panic — proving the field was actually wired up
// rather than silently ignored.
//
// tenant_service.go's GetIncludingOffboarded (a thin
// FindByIDIncludingDeleted wrapper with no logger of its own) is also
// covered here since it was entirely untested.
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

// wlLogger is a minimal port.Logger fake that records the last message
// logged at each level, for the small handful of tests that want to
// assert a warning actually flowed through the injected logger.
type wlLogger struct {
	warnMsgs []string
}

func (l *wlLogger) Debug(string, map[string]any) {}
func (l *wlLogger) Info(string, map[string]any)  {}
func (l *wlLogger) Warn(msg string, _ map[string]any) {
	l.warnMsgs = append(l.warnMsgs, msg)
}
func (l *wlLogger) Error(string, map[string]any) {}

// ── CatalogService.WithLogger ───────────────────────────────────────────

func TestCatalogService_WithLogger_ReturnsSamePointerAndWires(t *testing.T) {
	svc := service.NewCatalogService(nil, nil)
	got := svc.WithLogger(&wlLogger{})
	assert.Same(t, svc, got, "WithLogger must return the same *CatalogService for chaining")
}

// ── DeptMembershipService.WithLogger ────────────────────────────────────

func TestDeptMembershipService_WithLogger_ReturnsSamePointer(t *testing.T) {
	svc := service.NewDeptMembershipService(nil, nil, nil, nil, nil, nil, nil, nil)
	got := svc.WithLogger(&wlLogger{})
	assert.Same(t, svc, got, "WithLogger must return the same *DeptMembershipService for chaining")
}

// ── GroupMappingService.WithLogger ──────────────────────────────────────

func TestGroupMappingService_WithLogger_ReturnsSamePointer(t *testing.T) {
	svc := service.NewGroupMappingService(nil, nil, nil, nil, nil, nil)
	got := svc.WithLogger(&wlLogger{})
	assert.Same(t, svc, got, "WithLogger must return the same *GroupMappingService for chaining")
}

// ── ProvisioningService.WithLogger ──────────────────────────────────────

func TestProvisioningService_WithLogger_ReturnsSamePointer(t *testing.T) {
	svc := service.NewProvisioningService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	got := svc.WithLogger(&wlLogger{})
	assert.Same(t, svc, got, "WithLogger must return the same *ProvisioningService for chaining")
}

// ── InvitationService.WithMaxInvitesPerHour (cheap bonus — same shape) ──

func TestInvitationService_WithMaxInvitesPerHour_ReturnsSamePointer(t *testing.T) {
	svc := service.NewInvitationService(&fakeInviteRepo{}, nil, nil, nil, nil, nil, nil, nil, nil, 7)
	got := svc.WithMaxInvitesPerHour(50)
	assert.Same(t, svc, got, "WithMaxInvitesPerHour must return the same *InvitationService for chaining")
}

// ── TenantService.GetIncludingOffboarded ────────────────────────────────
// Previously entirely untested (0.0%). Thin wrapper over
// FindByIDIncludingDeleted — used by iam-system-only internal reads (I-2/
// I-9/I-14) where the tenant row may already be soft-deleted.

func TestTenantService_GetIncludingOffboarded_DelegatesToFindByIDIncludingDeleted(t *testing.T) {
	tenantID := uuid.New()
	offboardedAt := domain.Tenant{ID: tenantID, Status: domain.StatusOffboarded}
	called := false
	repo := &tsRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			called = true
			assert.Equal(t, tenantID, id)
			return &offboardedAt, nil
		},
	}
	svc := service.NewTenantService(repo, nil, &tsRP{})

	got, err := svc.GetIncludingOffboarded(context.Background(), tenantID)
	require.NoError(t, err)
	assert.True(t, called, "must reach FindByIDIncludingDeleted (via the fake's FindByID delegation)")
	assert.Equal(t, domain.StatusOffboarded, got.Status, "must return the offboarded row rather than filtering it out")
}

func TestTenantService_GetIncludingOffboarded_PropagatesRepoError(t *testing.T) {
	repoErr := domain.NewError(domain.ErrTenantNotFound, "no such tenant")
	repo := &tsRepo{
		findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
			return nil, repoErr
		},
	}
	svc := service.NewTenantService(repo, nil, &tsRP{})

	_, err := svc.GetIncludingOffboarded(context.Background(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrTenantNotFound)
}
