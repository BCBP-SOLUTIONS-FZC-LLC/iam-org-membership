// Router-level regression test for I-16 (§16 RP-C3). Unlike
// subscription_lapse_handler_test.go (which calls InternalHandler.
// ListSubscriptionLapses directly against a hand-built gin.Context,
// bypassing the middleware chain entirely), this test drives the REAL
// NewRouter through net/http/httptest with the EXACT header shape RP's
// orgmembership.Client.ListLapsedSubscriptions now sends: x-user-id and
// x-tenant-roles, plus a sentinel uuid.Nil x-tenant-id (never a missing
// header — gincommon's RequireAuth hard-rejects that on every /api/v1
// route with no per-route opt-out, which is exactly what broke this call
// before the sentinel value was added on RP's side).
package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRouter_I16_ReachableWithRPsSentinelTenantHeader(t *testing.T) {
	repo := &slhTenantRepo{listFn: func(_ context.Context, _ int) ([]domain.Tenant, error) {
		return nil, nil
	}}
	svc := service.NewSubscriptionLapseService(repo, 30)
	internalH := NewInternalHandler(nil, nil, nil, nil, nil, nil, svc)

	router := NewRouter(RouterConfig{
		GinConfig:       gincommon.Config{ServiceName: "iam-org-membership-test"},
		InternalHandler: internalH,
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/internal/subscription-lapses", http.NoBody)
	req.Header.Set("x-user-id", "iam-system")
	req.Header.Set("x-tenant-roles", "iam-system")
	req.Header.Set("x-tenant-id", uuid.Nil.String())
	w := httptest.NewRecorder()

	router.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "I-16 must accept RP's actual request shape: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), `"tenants":[]`)
}

// TestRouter_I16_MissingTenantHeaderStill401s documents the platform
// constraint that made the sentinel header necessary in the first place:
// gincommon's RequireAuth requires x-tenant-id on every /api/v1 route
// unconditionally — there is no O&M-side route-level opt-out, so any
// future caller of I-16 MUST send some value for this header, even though
// the handler itself never reads it.
func TestRouter_I16_MissingTenantHeaderStill401s(t *testing.T) {
	internalH := NewInternalHandler(nil, nil, nil, nil, nil, nil, nil)

	router := NewRouter(RouterConfig{
		GinConfig:       gincommon.Config{ServiceName: "iam-org-membership-test"},
		InternalHandler: internalH,
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/internal/subscription-lapses", http.NoBody)
	req.Header.Set("x-user-id", "iam-system")
	req.Header.Set("x-tenant-roles", "iam-system")
	w := httptest.NewRecorder()

	router.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
