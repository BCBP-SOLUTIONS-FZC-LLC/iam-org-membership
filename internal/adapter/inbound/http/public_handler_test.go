// Tests for PUB-1 (GET /api/v1/public/tenants/resolve, LLD §5.4): handler
// behaviour against a fake resolver, plus router-level guards that it is
// the one route reachable with no headers at all (PUBRES-1).
package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

type fakeResolver struct {
	res   domain.TenantResolution
	err   error
	calls []string
}

func (f *fakeResolver) Resolve(_ context.Context, identifier string) (domain.TenantResolution, error) {
	f.calls = append(f.calls, identifier)
	return f.res, f.err
}

func publicRouter(r TenantResolver) *Router {
	return NewRouter(RouterConfig{
		GinConfig:     gincommon.Config{ServiceName: "iam-org-membership-test"},
		PublicHandler: NewPublicHandler(r),
	})
}

// doPublic sends a request with NO headers of any kind.
func doPublic(t *testing.T, rt *Router, rawQuery string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/public/tenants/resolve"+rawQuery, http.NoBody)
	w := httptest.NewRecorder()
	rt.Handler().ServeHTTP(w, req)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w, body
}

func TestPUB1_Handler_MissingIdentifier_400(t *testing.T) {
	f := &fakeResolver{}
	for _, q := range []string{"", "?identifier=", "?identifier=%20%20"} {
		w, body := doPublic(t, publicRouter(f), q)
		assert.Equal(t, http.StatusBadRequest, w.Code, q)
		assert.Equal(t, "invalid_identifier", body["code"], q)
	}
	assert.Empty(t, f.calls, "an invalid identifier must never reach the lookup")
}

func TestPUB1_Handler_TooLongIdentifier_400(t *testing.T) {
	f := &fakeResolver{}
	w, body := doPublic(t, publicRouter(f), "?identifier="+strings.Repeat("a", maxIdentifierLen+1))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "invalid_identifier", body["code"])

	w, _ = doPublic(t, publicRouter(f), "?identifier="+strings.Repeat("a", maxIdentifierLen))
	assert.Equal(t, http.StatusOK, w.Code, "254 characters is the inclusive limit")
}

func TestPUB1_Handler_TrimsIdentifier(t *testing.T) {
	f := &fakeResolver{}
	doPublic(t, publicRouter(f), "?identifier=%20acme%20")
	assert.Equal(t, []string{"acme"}, f.calls)
}

func TestPUB1_Handler_FoundDedicated_NoTenantID(t *testing.T) {
	id := uuid.New()
	f := &fakeResolver{res: domain.TenantResolution{Found: true, Realm: "tenant-acme", RealmType: domain.RealmDedicated, TenantID: &id}}
	w, body := doPublic(t, publicRouter(f), "?identifier=acme")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, map[string]any{"found": true, "realm": "tenant-acme", "strategy": "dedicated"}, body,
		"a dedicated-realm tenant never exposes tenant_id pre-login (PUBRES-4), even if one is supplied")
}

func TestPUB1_Handler_FoundShared_HasTenantID(t *testing.T) {
	id := uuid.New()
	f := &fakeResolver{res: domain.TenantResolution{Found: true, Realm: "trial", RealmType: domain.RealmShared, TenantID: &id}}
	w, body := doPublic(t, publicRouter(f), "?identifier=acme")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, map[string]any{"found": true, "realm": "trial", "strategy": "shared", "tenant_id": id.String()}, body)
}

// The most important assertion here (PUBRES-2): a no-match is a 200 with
// exactly {"found":false}, never a 404 and never a null placeholder.
func TestPUB1_Handler_NotFound_Is200NotFalse404(t *testing.T) {
	w, _ := doPublic(t, publicRouter(&fakeResolver{}), "?identifier=nobody@example.com")
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotEqual(t, http.StatusNotFound, w.Code)
	assert.JSONEq(t, `{"found":false}`, w.Body.String())
}

func TestPUB1_Handler_DBUnavailable_503(t *testing.T) {
	f := &fakeResolver{err: domain.NewError(domain.ErrDBUnavailable, "db down")}
	w, body := doPublic(t, publicRouter(f), "?identifier=acme")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "db_unavailable", body["code"])
}

func TestPUB1_Router_NilHandler_NotRegistered(t *testing.T) {
	rt := NewRouter(RouterConfig{GinConfig: gincommon.Config{ServiceName: "t"}})
	w, _ := doPublic(t, rt, "?identifier=acme")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// PUBRES-1 regression guard: if registerPublicRoutes is ever moved under
// the protected /api/v1 group, this 200 becomes a 401.
func TestPUB1_Router_ReachableWithZeroHeaders(t *testing.T) {
	w, body := doPublic(t, publicRouter(&fakeResolver{}), "?identifier=acme")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, false, body["found"])
}

// PUB-1 must be the ONLY /api/v1 route that answers without identity
// headers: every other registered /api/v1 route must still 401 on a bare
// request. Catches both a public route drifting into the protected group
// and a new endpoint accidentally registered outside it.
func TestPUB1_Router_EveryOtherAPIRouteStill401sWithoutHeaders(t *testing.T) {
	rt := NewRouter(RouterConfig{
		GinConfig:             gincommon.Config{ServiceName: "iam-org-membership-test"},
		TenantHandler:         &TenantHandler{},
		DepartmentHandler:     &DepartmentHandler{},
		MembershipHandler:     &MembershipHandler{},
		DeptMembershipHandler: &DeptMembershipHandler{},
		RoleLabelHandler:      &RoleLabelHandler{},
		InvitationHandler:     &InvitationHandler{},
		OperatorHandler:       &OperatorHandler{},
		InternalHandler:       &InternalHandler{},
		PublicHandler:         NewPublicHandler(&fakeResolver{}),
	})
	param := regexp.MustCompile(`:[^/]+`)
	checked := 0
	for _, route := range rt.engine.Routes() {
		if !strings.HasPrefix(route.Path, "/api/v1/") {
			continue
		}
		path := param.ReplaceAllString(route.Path, uuid.NewString())
		req := httptest.NewRequestWithContext(context.Background(), route.Method, path, http.NoBody)
		w := httptest.NewRecorder()
		rt.Handler().ServeHTTP(w, req)
		if strings.HasPrefix(route.Path, "/api/v1/public/") {
			assert.NotEqual(t, http.StatusUnauthorized, w.Code, "%s %s must not require auth", route.Method, route.Path)
			continue
		}
		assert.Equal(t, http.StatusUnauthorized, w.Code, "%s %s answered %d with no identity headers", route.Method, route.Path, w.Code)
		checked++
	}
	assert.Greater(t, checked, 30, "expected to sweep every protected /api/v1 route")
}
