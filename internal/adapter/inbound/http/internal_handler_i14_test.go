// Handler-layer tests for:
//
//	I-14 GET /api/v1/internal/tenants/{id}/mfa-freshness (InternalHandler.GetMFAFreshness)
//
// I14-VAL-01, I14-AUTH-01, I14-AUTH-02, I14-AUTH-03, I14-AUTH-04,
// I14-HP-03, I14-BL-01
//
// I-14 is an internal route gated by RequireSystemRole (iam-system role only).
// The handler does no handler-level auth gate — the middleware owns it.
package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// iamSystemCtx returns a RequestContext for an iam-system caller.
func iamSystemCtx(tenantID uuid.UUID) *requestctx.RequestContext {
	return &requestctx.RequestContext{
		UserID:   uuid.New(),
		TenantID: tenantID,
		Roles:    []string{"iam-system"},
	}
}

// buildMFAHandler wires an InternalHandler whose tenants field is set.
func buildMFAHandler(tenantRepo *happyTenantRepo) *InternalHandler {
	svc := service.NewTenantService(tenantRepo, happyCacheStub{}, &happyRPClient{})
	return &InternalHandler{tenants: svc}
}

// ── I14-VAL-01: invalid UUID for tenant id → 400 ─────────────────────────────

// Test Case ID: I14-VAL-01
func TestGetMFAFreshness_InvalidUUID_400(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/", "", iamSystemCtx(uuid.New()))
	setParams(c, "id", "not-a-uuid")
	h.GetMFAFreshness(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ── I14-AUTH-01: no identity header → middleware aborts with 403 ─────────────

// Test Case ID: I14-AUTH-01
func TestGetMFAFreshness_NoIdentity_403(t *testing.T) {
	// RequireSystemRole aborts: missing identity → insufficient_role.
	mw := RequireSystemRole()
	c, w := buildCtx(http.MethodGet, "/", "", nil)
	setParams(c, "id", uuid.New().String())
	mw(c)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}

// ── I14-AUTH-02: regular member → 403 ────────────────────────────────────────

// Test Case ID: I14-AUTH-02
func TestGetMFAFreshness_RegularMember_403(t *testing.T) {
	mw := RequireSystemRole()
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenantID, Roles: []string{"member"}}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	setParams(c, "id", tenantID.String())
	mw(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── I14-AUTH-03: tenant_admin → 403 ──────────────────────────────────────────

// Test Case ID: I14-AUTH-03
func TestGetMFAFreshness_TenantAdmin_403(t *testing.T) {
	mw := RequireSystemRole()
	tenantID := uuid.New()
	c, w := buildCtx(http.MethodGet, "/", "", tenantAdminCtx(tenantID))
	setParams(c, "id", tenantID.String())
	mw(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── I14-AUTH-04: platform_operator → 403 ─────────────────────────────────────

// Test Case ID: I14-AUTH-04
func TestGetMFAFreshness_PlatformOperator_403(t *testing.T) {
	mw := RequireSystemRole()
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenantID, Roles: []string{"platform_operator"}}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	setParams(c, "id", tenantID.String())
	mw(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── I14-HP-03 / I14-BL-01: tenant not found → 404 ────────────────────────────

// Test Case ID: I14-HP-03 / I14-BL-01
func TestGetMFAFreshness_TenantNotFound_404(t *testing.T) {
	tenantID := uuid.New()
	repo := &happyTenantRepo{
		findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
			return nil, domain.NewError(domain.ErrTenantNotFound, "tenant not found")
		},
	}
	h := buildMFAHandler(repo)
	c, w := buildCtx(http.MethodGet, "/", "", iamSystemCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.GetMFAFreshness(c)
	assertErrorCode(t, w, http.StatusNotFound, "tenant_not_found")
}

// ── I14-HP happy path: iam-system gets mfa_freshness_seconds → 200 ────────────

func TestGetMFAFreshness_HappyPath_200(t *testing.T) {
	tenantID := uuid.New()
	repo := &happyTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, MFAFreshnessSeconds: 600}, nil
		},
	}
	h := buildMFAHandler(repo)
	c, w := buildCtx(http.MethodGet, "/", "", iamSystemCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.GetMFAFreshness(c)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "mfa_freshness_seconds")
}
