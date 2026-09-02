// Unit tests for RequireActiveTenant — the TRIAL-4 / §16 A53 defense-in-depth
// gate. Verifies status → verdict for every subscription_status value across
// read + write methods, and confirms iam-system and platform_operator
// principals bypass the gate.
//
// Pure unit test: uses a stub TenantRepository and gin.TestMode; no DB.
package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubTenantRepo returns a preset tenant from FindByID.
type stubTenantRepo struct {
	port.TenantRepositoryNoop
	tenant *domain.Tenant
	err    error
}

func (s *stubTenantRepo) FindByID(context.Context, uuid.UUID) (*domain.Tenant, error) {
	return s.tenant, s.err
}
func (s *stubTenantRepo) FindByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return s.FindByID(ctx, id)
}
func (s *stubTenantRepo) Update(context.Context, uuid.UUID, *domain.TenantPatch) (*domain.Tenant, error) {
	return nil, nil
}
func (s *stubTenantRepo) SetRealmSyncPending(context.Context, uuid.UUID) error { return nil }
func (s *stubTenantRepo) Insert(context.Context, *domain.Tenant) (*domain.Tenant, bool, error) {
	return nil, false, nil
}
func (s *stubTenantRepo) ListSubscriptionLapses(context.Context, int) ([]domain.Tenant, error) {
	return nil, nil
}

func runGate(t *testing.T, method string, status domain.SubscriptionStatus, roles []string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	tenantID := uuid.New()
	repo := &stubTenantRepo{tenant: &domain.Tenant{ID: tenantID, Status: status}}
	mw := RequireActiveTenant(repo)

	rc := &requestctx.RequestContext{
		UserID:   uuid.New(),
		TenantID: tenantID,
		Roles:    roles,
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequestWithContext(context.Background(), method, "/api/v1/tenants/x", strings.NewReader("{}"))
	req = req.WithContext(requestctx.WithContext(req.Context(), rc))
	c.Request = req

	called := false
	next := gin.HandlerFunc(func(c *gin.Context) { called = true; c.Status(http.StatusOK) })

	mw(c)
	if !c.IsAborted() {
		next(c)
	}
	return w, called
}

func decodeErr(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	return body
}

// ─── trial_expired — always blocked (TRIAL-4) ──────────────────────────

func TestRequireActiveTenant_TrialExpired_BlocksRead(t *testing.T) {
	w, called := runGate(t, http.MethodGet, domain.StatusTrialExpired, []string{"tenant_owner"})
	assert.False(t, called, "handler must NOT run — TRIAL-4 forbids reads")
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "tenant_trial_expired", decodeErr(t, w)["code"])
}

func TestRequireActiveTenant_TrialExpired_BlocksWrite(t *testing.T) {
	w, called := runGate(t, http.MethodPatch, domain.StatusTrialExpired, []string{"tenant_owner"})
	assert.False(t, called)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "tenant_trial_expired", decodeErr(t, w)["code"])
}

func TestRequireActiveTenant_TrialExpired_BlocksInvite(t *testing.T) {
	w, called := runGate(t, http.MethodPost, domain.StatusTrialExpired, []string{"tenant_owner"})
	assert.False(t, called, "P-6 invite on trial_expired must be blocked")
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// ─── suspended — always blocked ────────────────────────────────────────

func TestRequireActiveTenant_Suspended_BlocksAllMethods(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPatch, http.MethodPost, http.MethodDelete, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			w, called := runGate(t, method, domain.StatusSuspended, []string{"tenant_owner"})
			assert.False(t, called)
			assert.Equal(t, http.StatusForbidden, w.Code)
			assert.Equal(t, "tenant_suspended", decodeErr(t, w)["code"])
		})
	}
}

// ─── offboarded → 404 (row is soft-deleted anyway) ────────────────────

func TestRequireActiveTenant_Offboarded_Returns404(t *testing.T) {
	w, called := runGate(t, http.MethodGet, domain.StatusOffboarded, []string{"tenant_owner"})
	assert.False(t, called)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "tenant_not_found", decodeErr(t, w)["code"])
}

// ─── cancelled → reads pass, writes 403 (§16 A53 read-only) ───────────

func TestRequireActiveTenant_Cancelled_AllowsRead(t *testing.T) {
	w, called := runGate(t, http.MethodGet, domain.StatusCancelled, []string{"tenant_owner"})
	assert.True(t, called, "cancelled tenants remain readable per §16 A53")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRequireActiveTenant_Cancelled_BlocksWrite(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			w, called := runGate(t, method, domain.StatusCancelled, []string{"tenant_owner"})
			assert.False(t, called)
			assert.Equal(t, http.StatusForbidden, w.Code)
			assert.Equal(t, "tenant_read_only", decodeErr(t, w)["code"])
		})
	}
}

// ─── active / trial / past_due — proceed ──────────────────────────────

func TestRequireActiveTenant_LiveStates_ProceedForAllMethods(t *testing.T) {
	live := []domain.SubscriptionStatus{
		domain.StatusTrial,
		domain.StatusActive,
		domain.StatusPastDue,
	}
	for _, status := range live {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
			t.Run(string(status)+"/"+method, func(t *testing.T) {
				w, called := runGate(t, method, status, []string{"tenant_owner"})
				assert.True(t, called, "%s + %s must proceed", status, method)
				assert.Equal(t, http.StatusOK, w.Code)
			})
		}
	}
}

// ─── iam-system principal bypasses the gate on ALL statuses ───────────

func TestRequireActiveTenant_SystemPrincipal_BypassesEvenTrialExpired(t *testing.T) {
	w, called := runGate(t, http.MethodPost, domain.StatusTrialExpired, []string{"iam-system"})
	assert.True(t, called, "iam-system must reach handlers on trial_expired (TrialReactivated consumer, reconcilers)")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRequireActiveTenant_SystemPrincipal_BypassesSuspended(t *testing.T) {
	w, called := runGate(t, http.MethodPatch, domain.StatusSuspended, []string{"iam-system"})
	assert.True(t, called)
	assert.Equal(t, http.StatusOK, w.Code)
}

// ─── platform_operator bypasses the gate on ALL statuses ──────────────

func TestRequireActiveTenant_Operator_BypassesTrialExpired(t *testing.T) {
	w, called := runGate(t, http.MethodPost, domain.StatusTrialExpired, []string{"platform_operator"})
	assert.True(t, called, "operator must reach O-7 reassign / O-4 flags on trial_expired")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRequireActiveTenant_Operator_BypassesOffboarded(t *testing.T) {
	w, called := runGate(t, http.MethodGet, domain.StatusOffboarded, []string{"platform_operator"})
	assert.True(t, called)
	assert.Equal(t, http.StatusOK, w.Code)
}

// ─── no request context → let downstream middleware handle ────────────

func TestRequireActiveTenant_NoIdentity_PassesThrough(t *testing.T) {
	repo := &stubTenantRepo{tenant: &domain.Tenant{Status: domain.StatusTrialExpired}}
	mw := RequireActiveTenant(repo)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)

	called := false
	mw(c)
	if !c.IsAborted() {
		called = true
	}
	assert.True(t, called, "no identity context → gate must not abort; auth middleware handles it")
}

// ─── tenant lookup error → fall through to handler (which produces 404) ─

func TestRequireActiveTenant_LookupError_FallsThrough(t *testing.T) {
	repo := &stubTenantRepo{err: assertRepoErr}
	mw := RequireActiveTenant(repo)

	rc := &requestctx.RequestContext{TenantID: uuid.New(), Roles: []string{"tenant_owner"}}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	req = req.WithContext(requestctx.WithContext(req.Context(), rc))
	c.Request = req

	called := false
	mw(c)
	if !c.IsAborted() {
		called = true
	}
	assert.True(t, called, "repo error → gate must not abort; downstream produces the 404")
}

// sentinel used by the lookup-error test
var assertRepoErr = &sentinelErr{msg: "db down"}

type sentinelErr struct{ msg string }

func (e *sentinelErr) Error() string { return e.msg }
