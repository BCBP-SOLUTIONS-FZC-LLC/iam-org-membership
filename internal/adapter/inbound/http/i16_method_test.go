// Router-level tests verifying that I-16 (GET /api/v1/internal/subscription-lapses)
// only accepts GET — all other HTTP methods must return 405 Method Not Allowed.
//
// These tests drive the real NewRouter through net/http/httptest with the
// exact header shape RP's orgmembership.Client sends (x-user-id: iam-system,
// x-tenant-roles: iam-system, x-tenant-id: 00000000-0000-0000-0000-000000000000).
//
// Test case IDs: I16-METHOD-01, I16-METHOD-02
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
)

// newTestRouterForI16 wires a minimal Router with only the InternalHandler
// needed to exercise the /api/v1/internal/subscription-lapses route.
func newTestRouterForI16() *Router {
	repo := &slhTenantRepo{listFn: func(_ context.Context, _ int) ([]domain.Tenant, error) {
		return nil, nil
	}}
	svc := service.NewSubscriptionLapseService(repo, 30)
	internalH := NewInternalHandler(nil, nil, nil, nil, nil, nil, svc)
	return NewRouter(RouterConfig{
		GinConfig:       gincommon.Config{ServiceName: "iam-org-membership-test"},
		InternalHandler: internalH,
	})
}

// iamSystemHeaders sets the three headers RP sends for I-16 calls.
func iamSystemHeaders(req *http.Request) {
	req.Header.Set("x-user-id", "iam-system")
	req.Header.Set("x-tenant-roles", "iam-system")
	req.Header.Set("x-tenant-id", uuid.Nil.String())
}

// ── I16-METHOD-01: POST /api/v1/internal/subscription-lapses → 405 ──────────

// Test Case ID: I16-METHOD-01
// I-16 is a read-only endpoint; POST must be rejected at the router level
// with 405 Method Not Allowed.
func TestListSubscriptionLapses_POST_405(t *testing.T) {
	router := newTestRouterForI16()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/internal/subscription-lapses", http.NoBody)
	iamSystemHeaders(req)
	w := httptest.NewRecorder()

	router.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code,
		"POST to I-16 must return 405; got: %s", w.Body.String())
}

// ── I16-METHOD-02: DELETE /api/v1/internal/subscription-lapses → 405 ────────

// Test Case ID: I16-METHOD-02
// DELETE must also be rejected with 405 — only GET is registered.
func TestListSubscriptionLapses_DELETE_405(t *testing.T) {
	router := newTestRouterForI16()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/api/v1/internal/subscription-lapses", http.NoBody)
	iamSystemHeaders(req)
	w := httptest.NewRecorder()

	router.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code,
		"DELETE to I-16 must return 405; got: %s", w.Body.String())
}
