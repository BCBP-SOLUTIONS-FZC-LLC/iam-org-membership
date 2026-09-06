// membership_handler_gaps_test.go — handler-layer branch coverage for
// MembershipHandler.List, MembershipHandler.SeatUsage, and
// MembershipHandler.ResetMFA that was not hit by prior test files.
//
// Covered branches:
//   - List: ?limit=5 → limit variable is set to 5 (the limit > 0 branch at line 59)
//   - List: service returns error → HandleError (lines 77-80)
//   - List: page.NextCursor != nil → response includes next_cursor (lines 82-85)
//   - SeatUsage: usage.OverageSince != nil → resp["overage_since"] included
//   - SeatUsage: usage.GraceEndsAt != nil → resp["grace_ends_at"] included
//   - ResetMFA: parseUUIDParam(c, "user_id") fails → HandleError (invalid user_id)
//
// All tests run in package http (whitebox) and use the shared helpers
// (buildCtx, setParams, assertErrorCode, tenantOwnerCtx) plus the stub
// implementations (mhMemRepo, mhRoleRepo, drhDeptMemRepo, happyTenantRepo,
// iahInviteRepo, happyCacheStub, happyRPClient, drhWorkflowClient,
// happyTxRunner) already defined in this package's other _test.go files.
package http

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// ── MembershipHandler.List: ?limit=5 sets the limit variable ─────────────────
//
// TestMembershipList_ValidLimit_UsesCustomLimit covers the branch at line 59:
// `if s, exists := c.GetQuery("limit"); exists { ... limit = n }`.
// The limit query param is valid (5), so the handler uses 5 not the default 50,
// and then calls the service; the stub returns an empty page which yields 200.

func TestMembershipList_ValidLimit_UsesCustomLimit(t *testing.T) {
	tenantID := uuid.New()
	var capturedLimit int
	mem := &mhMemRepo{
		listFn: func(_ context.Context, tid uuid.UUID, cur *domain.MembershipListCursor, limit int) (*domain.MembershipListPage, error) {
			capturedLimit = limit
			return &domain.MembershipListPage{Items: []domain.MembershipListItem{}}, nil
		},
	}
	svc := service.NewMembershipService(
		mem, &mhRoleRepo{}, &drhDeptMemRepo{},
		&happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{},
		&happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30,
	)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	c.Request.URL.RawQuery = "limit=5"
	h.List(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 5, capturedLimit, "handler must pass the custom limit to the service")
}

// ── MembershipHandler.List: service error → HandleError ──────────────────────
//
// TestMembershipList_ServiceError_Returns500 covers lines 77-80:
// `if err != nil { HandleError(c, err); return }`.
// The stub returns a non-domain error which surfaces as 500 internal_error.

func TestMembershipList_ServiceError_Returns500(t *testing.T) {
	tenantID := uuid.New()
	mem := &mhMemRepo{
		listFn: func(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
			return nil, errors.New("db connection lost")
		},
	}
	svc := service.NewMembershipService(
		mem, &mhRoleRepo{}, &drhDeptMemRepo{},
		&happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{},
		&happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30,
	)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.List(c)

	assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
}

// ── MembershipHandler.List: page.NextCursor != nil → next_cursor in response ─
//
// TestMembershipList_WithNextCursor_ResponseIncludesNextCursor covers lines
// 82-85: `if page.NextCursor != nil { ... resp["next_cursor"] = ... }`.
// The stub returns a page with a non-nil NextCursor; the JSON response must
// contain the "next_cursor" key.

func TestMembershipList_WithNextCursor_ResponseIncludesNextCursor(t *testing.T) {
	tenantID := uuid.New()
	userA := uuid.New()
	mem := &mhMemRepo{
		listFn: func(_ context.Context, tid uuid.UUID, _ *domain.MembershipListCursor, _ int) (*domain.MembershipListPage, error) {
			return &domain.MembershipListPage{
				Items: []domain.MembershipListItem{
					{Membership: domain.TenantMembership{
						ID: uuid.New(), TenantID: tid, UserID: userA,
						Status: domain.MembershipActive, RecordVersion: 1,
					}},
				},
				NextCursor: &domain.MembershipListCursor{
					CreatedAt: time.Now(),
					ID:        uuid.New(),
				},
			}, nil
		},
	}
	svc := service.NewMembershipService(
		mem, &mhRoleRepo{}, &drhDeptMemRepo{},
		&happyTenantRepo{}, &iahInviteRepo{}, happyCacheStub{},
		&happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30,
	)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.List(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"next_cursor"`,
		"response must include next_cursor when page.NextCursor is non-nil")
}

// ── MembershipHandler.SeatUsage: OverageSince non-nil ────────────────────────
//
// TestSeatUsage_WithOverageSince_ResponseIncludesField covers lines 297-299:
// `if usage.OverageSince != nil { resp["overage_since"] = usage.OverageSince }`.
// The tenant returned from the repo has OverageSince set; the response must
// contain the "overage_since" key.

func TestSeatUsage_WithOverageSince_ResponseIncludesField(t *testing.T) {
	tenantID := uuid.New()
	overageSince := time.Now().Add(-48 * time.Hour)
	tenants := &happyTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{
				ID:            id,
				LicensedSeats: 5,
				OverageSince:  &overageSince,
			}, nil
		},
	}
	mem := &mhMemRepo{countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 7, nil }}
	inv := &iahInviteRepo{countPendingFn: func(context.Context, uuid.UUID) (int, error) { return 1, nil }}
	svc := service.NewMembershipService(
		mem, &mhRoleRepo{}, &drhDeptMemRepo{},
		tenants, inv, happyCacheStub{},
		&happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30,
	)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.SeatUsage(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"overage_since"`,
		"response must include overage_since when usage.OverageSince is non-nil")
}

// ── MembershipHandler.SeatUsage: GraceEndsAt non-nil ────────────────────────
//
// TestSeatUsage_WithGraceEndsAt_ResponseIncludesField covers lines 300-302:
// `if usage.GraceEndsAt != nil { resp["grace_ends_at"] = usage.GraceEndsAt }`.
// The service computes GraceEndsAt from OverageSince + SEAT_OVERAGE_GRACE_DAYS;
// since SeatUsage only surfaces GraceEndsAt when OverageSince is set, we supply
// both. The response must contain the "grace_ends_at" key.

func TestSeatUsage_WithGraceEndsAt_ResponseIncludesField(t *testing.T) {
	tenantID := uuid.New()
	// Place OverageSince far enough in the past that GraceEndsAt is in the
	// future (grace = 30 days default), so OverCap is true AND GraceEndsAt is
	// populated by the service layer.
	overageSince := time.Now().Add(-10 * 24 * time.Hour) // 10 days ago → still in grace
	tenants := &happyTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{
				ID:            id,
				LicensedSeats: 2,
				OverageSince:  &overageSince,
			}, nil
		},
	}
	mem := &mhMemRepo{countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 5, nil }}
	inv := &iahInviteRepo{countPendingFn: func(context.Context, uuid.UUID) (int, error) { return 0, nil }}
	// grace days = 30 (passed to NewMembershipService as the last int arg)
	svc := service.NewMembershipService(
		mem, &mhRoleRepo{}, &drhDeptMemRepo{},
		tenants, inv, happyCacheStub{},
		&happyRPClient{}, &drhWorkflowClient{}, happyTxRunner{}, nil, 30,
	)
	h := &MembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.SeatUsage(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"grace_ends_at"`,
		"response must include grace_ends_at when usage.GraceEndsAt is non-nil")
}

// ── MembershipHandler.ResetMFA: invalid user_id UUID → HandleError ────────────
//
// TestResetMFA_InvalidUserID_Returns400 covers lines 367-371:
// `userID, err := parseUUIDParam(c, "user_id"); if err != nil { HandleError(c, err); return }`.
// A non-UUID user_id param must be rejected before the service is touched;
// the nil service proves no service call escapes.

func TestResetMFA_InvalidUserID_Returns400(t *testing.T) {
	tenantID := uuid.New()
	h := &MembershipHandler{} // nil svc: proves the UUID gate fires first

	c, w := buildCtx(http.MethodPost, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "user_id", "not-a-valid-uuid")
	h.ResetMFA(c)

	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}
