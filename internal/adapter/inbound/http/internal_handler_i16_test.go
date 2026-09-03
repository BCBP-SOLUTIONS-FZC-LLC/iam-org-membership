// Handler-layer tests for:
//
//	I-16 GET /api/v1/internal/subscription-lapses (InternalHandler.ListSubscriptionLapses)
//
// Test case IDs: I16-HP-01, I16-HP-02, I16-HP-03, I16-RESP-01, I16-RESP-03,
// I16-RESP-04, I16-AUTH-03, I16-AUTH-04, I16-AUTH-06, I16-AUTH-07,
// I16-CFG-01, I16-CFG-02, I16-CFG-03, I16-ERR-01
//
// Auth scenarios I16-AUTH-01 / I16-AUTH-02 / I16-AUTH-05 require
// platform-gincommon's RequireAuth middleware which is not wired in
// handler unit tests — covered by manual tests and middleware-level tests.
package http

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── fake repo ────────────────────────────────────────────────────────────────

// i16LapseRepo is a TenantRepository stub whose ListSubscriptionLapses is
// overridable per test. All other methods delegate to TenantRepositoryNoop
// (they are never called on the I-16 handler path).
type i16LapseRepo struct {
	port.TenantRepositoryNoop
	listFn func(ctx context.Context, graceDays int) ([]domain.Tenant, error)
}

func (r *i16LapseRepo) ListSubscriptionLapses(ctx context.Context, graceDays int) ([]domain.Tenant, error) {
	if r.listFn != nil {
		return r.listFn(ctx, graceDays)
	}
	return nil, nil
}

// buildLapseHandler wires an InternalHandler whose subscriptionLapse field is
// backed by the given repo and grace-day window.
func buildLapseHandler(repo *i16LapseRepo, graceDays int) *InternalHandler {
	svc := service.NewSubscriptionLapseService(repo, graceDays)
	return &InternalHandler{subscriptionLapse: svc}
}

// nilUUIDCtx returns an iam-system RequestContext with the sentinel nil UUID
// as the tenant — matching RP's real call pattern for I-16 (§16 OQ-9).
func nilUUIDCtx() *requestctx.RequestContext {
	return iamSystemCtx(uuid.Nil)
}

// makeCancelledTenant returns a minimal domain.Tenant in cancelled state.
func makeCancelledTenant(realmID string, realmType domain.RealmType, daysAgo int) domain.Tenant {
	t := time.Now().UTC().Add(-time.Duration(daysAgo) * 24 * time.Hour)
	return domain.Tenant{
		ID:          uuid.New(),
		RealmID:     realmID,
		RealmType:   realmType,
		Status:      domain.StatusCancelled,
		CancelledAt: &t,
	}
}

// ── I16-HP-01: single cancelled tenant past grace → 200 with tenant in list ──

// Test Case ID: I16-HP-01
func TestListSubscriptionLapses_SingleTenant_200(t *testing.T) {
	tenant := makeCancelledTenant("dcpp-01", domain.RealmShared, 35)
	repo := &i16LapseRepo{
		listFn: func(context.Context, int) ([]domain.Tenant, error) {
			return []domain.Tenant{tenant}, nil
		},
	}
	h := buildLapseHandler(repo, 30)
	c, w := buildCtx(http.MethodGet, "/", "", nilUUIDCtx())
	h.ListSubscriptionLapses(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var got SubscriptionLapseListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got.Tenants, 1)
	assert.Equal(t, tenant.ID, got.Tenants[0].TenantID)
	assert.Equal(t, "dcpp-01", got.Tenants[0].RealmID)
	assert.Equal(t, "shared", got.Tenants[0].RealmType)
	assert.False(t, got.Tenants[0].CancelledAt.IsZero())
}

// ── I16-HP-02: multiple cancelled tenants → all returned ─────────────────────

// Test Case ID: I16-HP-02
func TestListSubscriptionLapses_MultipleTenantsAllReturned_200(t *testing.T) {
	tenants := []domain.Tenant{
		makeCancelledTenant("realm-a", domain.RealmShared, 60),
		makeCancelledTenant("realm-b", domain.RealmDedicated, 45),
		makeCancelledTenant("realm-c", domain.RealmShared, 31),
	}
	repo := &i16LapseRepo{
		listFn: func(context.Context, int) ([]domain.Tenant, error) { return tenants, nil },
	}
	h := buildLapseHandler(repo, 30)
	c, w := buildCtx(http.MethodGet, "/", "", nilUUIDCtx())
	h.ListSubscriptionLapses(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var got SubscriptionLapseListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Len(t, got.Tenants, 3)
}

// ── I16-HP-03 / I16-RESP-01: no qualifying tenants → {"tenants":[]} not null ─

// Test Case IDs: I16-HP-03, I16-RESP-01
func TestListSubscriptionLapses_EmptyResult_200(t *testing.T) {
	repo := &i16LapseRepo{
		listFn: func(context.Context, int) ([]domain.Tenant, error) { return nil, nil },
	}
	h := buildLapseHandler(repo, 30)
	c, w := buildCtx(http.MethodGet, "/", "", nilUUIDCtx())
	h.ListSubscriptionLapses(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{"tenants":[]}`, w.Body.String(), "empty list must be [] not null")
}

// ── I16-RESP-03: response has exactly 4 fields per tenant entry ───────────────

// Test Case ID: I16-RESP-03
func TestListSubscriptionLapses_ResponseHasExactlyFourFields(t *testing.T) {
	tenant := makeCancelledTenant("realm-resp", domain.RealmShared, 35)
	repo := &i16LapseRepo{
		listFn: func(context.Context, int) ([]domain.Tenant, error) {
			return []domain.Tenant{tenant}, nil
		},
	}
	h := buildLapseHandler(repo, 30)
	c, w := buildCtx(http.MethodGet, "/", "", nilUUIDCtx())
	h.ListSubscriptionLapses(c)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	items := raw["tenants"].([]any)
	require.Len(t, items, 1)
	item := items[0].(map[string]any)
	keys := make([]string, 0, len(item))
	for k := range item {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t,
		[]string{"tenant_id", "realm_id", "realm_type", "cancelled_at"},
		keys,
		"response must expose exactly these 4 fields — no PII, no extra internals",
	)
}

// ── I16-RESP-04: realm_type values are only shared or dedicated ───────────────

// Test Case ID: I16-RESP-04
func TestListSubscriptionLapses_RealmTypeEnumValues(t *testing.T) {
	tenants := []domain.Tenant{
		makeCancelledTenant("realm-shared", domain.RealmShared, 35),
		makeCancelledTenant("realm-dedicated", domain.RealmDedicated, 35),
	}
	repo := &i16LapseRepo{
		listFn: func(context.Context, int) ([]domain.Tenant, error) { return tenants, nil },
	}
	h := buildLapseHandler(repo, 30)
	c, w := buildCtx(http.MethodGet, "/", "", nilUUIDCtx())
	h.ListSubscriptionLapses(c)

	var got SubscriptionLapseListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got.Tenants, 2)
	realmTypes := map[string]bool{}
	for _, item := range got.Tenants {
		realmTypes[item.RealmType] = true
	}
	assert.True(t, realmTypes["shared"], "shared realm_type must be serialised correctly")
	assert.True(t, realmTypes["dedicated"], "dedicated realm_type must be serialised correctly")
}

// ── I16-AUTH-03: no x-tenant-roles header → RequireSystemRole → 403 ──────────

// Test Case ID: I16-AUTH-03
func TestListSubscriptionLapses_NoRoles_403(t *testing.T) {
	mw := RequireSystemRole()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: uuid.Nil, Roles: nil}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	mw(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── I16-AUTH-04: wrong role (tenant_admin) → 403 ─────────────────────────────

// Test Case ID: I16-AUTH-04
func TestListSubscriptionLapses_WrongRole_TenantAdmin_403(t *testing.T) {
	mw := RequireSystemRole()
	c, w := buildCtx(http.MethodGet, "/", "", tenantAdminCtx(uuid.New()))
	mw(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── I16-AUTH-06: non-nil UUID in x-tenant-id → 200 (handler ignores value) ───

// Test Case ID: I16-AUTH-06
func TestListSubscriptionLapses_NonNilTenantID_HandlerIgnoresValue_200(t *testing.T) {
	repo := &i16LapseRepo{
		listFn: func(context.Context, int) ([]domain.Tenant, error) { return nil, nil },
	}
	h := buildLapseHandler(repo, 30)
	// Use a real tenant UUID instead of uuid.Nil — handler must not filter by it.
	c, w := buildCtx(http.MethodGet, "/", "", iamSystemCtx(uuid.New()))
	h.ListSubscriptionLapses(c)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// ── I16-AUTH-07: real user UUID + tenant_admin role → 403 ────────────────────

// Test Case ID: I16-AUTH-07
func TestListSubscriptionLapses_RealUserUUID_TenantAdmin_403(t *testing.T) {
	mw := RequireSystemRole()
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{
		UserID:   uuid.New(), // real user UUID, not iam-system string
		TenantID: tenantID,
		Roles:    []string{"tenant_admin"},
	}
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	mw(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── I16-CFG-01: graceDays=0 → grace window propagated to repo ────────────────

// Test Case ID: I16-CFG-01
func TestListSubscriptionLapses_GraceDays0_PropagatedToRepo(t *testing.T) {
	var capturedGrace int
	repo := &i16LapseRepo{
		listFn: func(_ context.Context, graceDays int) ([]domain.Tenant, error) {
			capturedGrace = graceDays
			return nil, nil
		},
	}
	h := buildLapseHandler(repo, 0)
	c, w := buildCtx(http.MethodGet, "/", "", nilUUIDCtx())
	h.ListSubscriptionLapses(c)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 0, capturedGrace, "graceDays=0 must be passed through to repo")
}

// ── I16-CFG-02: graceDays=60 → grace window propagated to repo ───────────────

// Test Case ID: I16-CFG-02
func TestListSubscriptionLapses_GraceDays60_PropagatedToRepo(t *testing.T) {
	var capturedGrace int
	repo := &i16LapseRepo{
		listFn: func(_ context.Context, graceDays int) ([]domain.Tenant, error) {
			capturedGrace = graceDays
			return nil, nil
		},
	}
	h := buildLapseHandler(repo, 60)
	c, w := buildCtx(http.MethodGet, "/", "", nilUUIDCtx())
	h.ListSubscriptionLapses(c)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 60, capturedGrace, "graceDays=60 must be passed through to repo")
}

// ── I16-CFG-03: graceDays=30 (default) ───────────────────────────────────────

// Test Case ID: I16-CFG-03
func TestListSubscriptionLapses_GraceDays30_Default(t *testing.T) {
	var capturedGrace int
	repo := &i16LapseRepo{
		listFn: func(_ context.Context, graceDays int) ([]domain.Tenant, error) {
			capturedGrace = graceDays
			return nil, nil
		},
	}
	h := buildLapseHandler(repo, 30)
	c, w := buildCtx(http.MethodGet, "/", "", nilUUIDCtx())
	h.ListSubscriptionLapses(c)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 30, capturedGrace)
}

// ── I16-ERR-01: service/repo error → 503 dependency_unavailable ──────────────

// Test Case ID: I16-ERR-01
func TestListSubscriptionLapses_RepoError_503(t *testing.T) {
	repo := &i16LapseRepo{
		listFn: func(context.Context, int) ([]domain.Tenant, error) {
			return nil, domain.NewError(domain.ErrDBUnavailable, "database unavailable")
		},
	}
	h := buildLapseHandler(repo, 30)
	c, w := buildCtx(http.MethodGet, "/", "", nilUUIDCtx())
	h.ListSubscriptionLapses(c)
	assertErrorCode(t, w, http.StatusServiceUnavailable, "db_unavailable")
}

// ── I16-IDEMP-01: repeated calls → identical responses (no side effects) ──────

// Test Case ID: I16-IDEMP-01
func TestListSubscriptionLapses_RepeatedCalls_Identical(t *testing.T) {
	tenant := makeCancelledTenant("realm-idemp", domain.RealmShared, 35)
	calls := 0
	repo := &i16LapseRepo{
		listFn: func(context.Context, int) ([]domain.Tenant, error) {
			calls++
			return []domain.Tenant{tenant}, nil
		},
	}
	h := buildLapseHandler(repo, 30)

	c1, w1 := buildCtx(http.MethodGet, "/", "", nilUUIDCtx())
	h.ListSubscriptionLapses(c1)
	c2, w2 := buildCtx(http.MethodGet, "/", "", nilUUIDCtx())
	h.ListSubscriptionLapses(c2)

	assert.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "repeated polls must return identical response")
	assert.Equal(t, 2, calls, "repo must be called fresh on each poll — no caching")
}

// ── compile-time check ────────────────────────────────────────────────────────

var _ port.TenantRepository = (*i16LapseRepo)(nil)
