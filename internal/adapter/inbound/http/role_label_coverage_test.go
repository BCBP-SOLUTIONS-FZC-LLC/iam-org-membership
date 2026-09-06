// Handler-layer unit tests for:
//
//	P-13 PATCH /api/v1/tenants/{id}/role-labels/{role_code} (RoleLabelHandler.Patch)
//	P-12 GET  /api/v1/tenants/{id}/role-labels              (RoleLabelHandler.List)
//
// Complements handler_matrix_test.go (malformed/cross-tenant/non-admin) and
// dept_role_happy_test.go (basic happy paths) with the full P28RL scenario
// matrix: validation, auth, concurrency, business-logic, and dependency
// resilience branches.
package http

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// ── Validation ────────────────────────────────────────────────────────────────

// P28RL-VAL-01: tenant_id path param is not a UUID → 400 invalid_uuid.
// The handler calls parseTenantIDParam before any service interaction.
func TestP28RL_VAL01_InvalidTenantUUID(t *testing.T) {
	h := &RoleLabelHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"display_name":"Buyer","record_version":1}`, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "not-uuid", "role_code", "approver")
	h.Patch(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// P28RL-VAL-02: role_code path param contains an unrecognised value → 400.
// RoleLabelService.Update rejects any value that is not preparator/reviewer/approver.
func TestP28RL_VAL02_InvalidRoleCode(t *testing.T) {
	tenantID := uuid.New()
	svc := service.NewRoleLabelService(&drhLabelRepo{}, drhCache{})
	h := &RoleLabelHandler{svc: svc}

	body := `{"display_name":"Wizard","record_version":1}`
	c, w := buildCtx(http.MethodPatch, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "role_code", "wizard")
	h.Patch(c)

	assertErrorCode(t, w, http.StatusBadRequest, "invalid_role")
}

// P28RL-VAL-03: role_code=member → 400.
// 'member' is a derived-only tenant role (TR-7 / §16 A29), never a dept role.
// The service rejects it with ErrValidation, same as any other non-dept code.
func TestP28RL_VAL03_MemberRoleCodeRejected(t *testing.T) {
	tenantID := uuid.New()
	svc := service.NewRoleLabelService(&drhLabelRepo{}, drhCache{})
	h := &RoleLabelHandler{svc: svc}

	body := `{"display_name":"Plain Member","record_version":1}`
	c, w := buildCtx(http.MethodPatch, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "role_code", "member")
	h.Patch(c)

	assertErrorCode(t, w, http.StatusBadRequest, "invalid_role")
}

// P28RL-VAL-04: display_name="" → 400.
// RoleLabelService.Update requires a non-empty display_name (DRL-2).
func TestP28RL_VAL04_EmptyDisplayName(t *testing.T) {
	tenantID := uuid.New()
	svc := service.NewRoleLabelService(&drhLabelRepo{}, drhCache{})
	h := &RoleLabelHandler{svc: svc}

	body := `{"display_name":"","record_version":1}`
	c, w := buildCtx(http.MethodPatch, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "role_code", "reviewer")
	h.Patch(c)

	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// P28RL-VAL-05: body=bad-json → 400.
// ShouldBindJSON fails before the service is invoked; handler returns
// validation_error (see role_label_handler.go Patch).
func TestP28RL_VAL05_MalformedBody(t *testing.T) {
	tenantID := uuid.New()
	// nil svc — handler must not reach the service on malformed input.
	h := &RoleLabelHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{bad-json`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "role_code", "approver")
	h.Patch(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ── Auth ──────────────────────────────────────────────────────────────────────

// P28RL-AUTH-01: nil RequestContext → 401 missing_identity_headers.
// parseTenantIDParam succeeds but requireTenantAdmin calls extractCaller
// which returns ErrMissingIdentity when no context is present.
func TestP28RL_AUTH01_MissingIdentity(t *testing.T) {
	tenantID := uuid.New()
	h := &RoleLabelHandler{}

	c, w := buildCtx(http.MethodPatch, "/", `{"display_name":"Approver","record_version":1}`, nil)
	setParams(c, "id", tenantID.String(), "role_code", "approver")
	h.Patch(c)

	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

// P28RL-AUTH-02: plain member (no elevated role) cannot PATCH → 403.
// AUTH-2: only tenant_owner and tenant_admin may mutate role labels.
func TestP28RL_AUTH02_MemberCannotPatch(t *testing.T) {
	tenantID := uuid.New()
	h := &RoleLabelHandler{}

	rc := &requestctx.RequestContext{
		UserID:   uuid.New(),
		TenantID: tenantID,
		Roles:    []string{"member"},
	}
	c, w := buildCtx(http.MethodPatch, "/", `{"display_name":"Sign-off","record_version":1}`, rc)
	setParams(c, "id", tenantID.String(), "role_code", "approver")
	h.Patch(c)

	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// P28RL-AUTH-03: caller's TenantID != path tenant_id → 403 (cross-tenant).
// requireTenantAdmin compares the caller's TenantID from the JWT context
// with the path param and rejects cross-tenant mutations.
func TestP28RL_AUTH03_CrossTenantPatch(t *testing.T) {
	pathTenant := uuid.New()
	callerTenant := uuid.New() // different tenant
	h := &RoleLabelHandler{}

	rc := &requestctx.RequestContext{
		UserID:   uuid.New(),
		TenantID: callerTenant,
		Roles:    []string{"tenant_owner"},
	}
	c, w := buildCtx(http.MethodPatch, "/", `{"display_name":"Buyer","record_version":1}`, rc)
	setParams(c, "id", pathTenant.String(), "role_code", "approver")
	h.Patch(c)

	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── Concurrency ───────────────────────────────────────────────────────────────

// P28RL-CONC-01: repo returns ErrOptimisticLockConflict → 409 (CONC-4).
// Happens when a concurrent caller has already bumped the record_version;
// the client must re-read and retry.
func TestP28RL_CONC01_StaleRecordVersion(t *testing.T) {
	tenantID := uuid.New()
	repo := &drhLabelRepo{
		updateFn: func(_ context.Context, _ uuid.UUID, _ domain.DeptRole, _ string, _ int64) (*domain.DeptRoleLabel, error) {
			return nil, domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict")
		},
	}
	svc := service.NewRoleLabelService(repo, drhCache{})
	h := &RoleLabelHandler{svc: svc}

	body := `{"display_name":"Sign-off Authority","record_version":1}`
	c, w := buildCtx(http.MethodPatch, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "role_code", "approver")
	h.Patch(c)

	assertErrorCode(t, w, http.StatusConflict, "optimistic_lock_conflict")
}

// ── Happy paths ───────────────────────────────────────────────────────────────

// P28RL-HP-02: rename the preparator label → 200 with the new display_name.
// Admin renames "Preparator" to "Document Author"; response echoes back
// the updated label with the bumped record_version.
func TestP28RL_HP02_RenamePreparatorAdmin(t *testing.T) {
	tenantID := uuid.New()
	repo := &drhLabelRepo{
		updateFn: func(_ context.Context, tid uuid.UUID, code domain.DeptRole, dn string, ver int64) (*domain.DeptRoleLabel, error) {
			return &domain.DeptRoleLabel{
				TenantID:      tid,
				RoleCode:      code,
				DisplayName:   dn,
				RecordVersion: ver + 1,
			}, nil
		},
	}
	svc := service.NewRoleLabelService(repo, drhCache{})
	h := &RoleLabelHandler{svc: svc}

	body := `{"display_name":"Document Author","record_version":3}`
	c, w := buildCtx(http.MethodPatch, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "role_code", "preparator")
	h.Patch(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Document Author")
	assert.Contains(t, w.Body.String(), "preparator")
}

// ── Business-logic ────────────────────────────────────────────────────────────

// P28RL-BL-01: updating display_name does not alter role_code.
// The role_code in the 200 response must match the path param, regardless
// of the display_name provided in the request body (DRL-1: role_code immutable).
func TestP28RL_BL01_DisplayNameDoesNotChangeRoleCode(t *testing.T) {
	tenantID := uuid.New()
	repo := &drhLabelRepo{
		updateFn: func(_ context.Context, tid uuid.UUID, code domain.DeptRole, dn string, ver int64) (*domain.DeptRoleLabel, error) {
			return &domain.DeptRoleLabel{
				TenantID:      tid,
				RoleCode:      code, // service always returns the code as passed, not derived from display_name
				DisplayName:   dn,
				RecordVersion: ver + 1,
			}, nil
		},
	}
	svc := service.NewRoleLabelService(repo, drhCache{})
	h := &RoleLabelHandler{svc: svc}

	body := `{"display_name":"Approver Renamed","record_version":2}`
	c, w := buildCtx(http.MethodPatch, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "role_code", "approver")
	h.Patch(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	// role_code in response must be the canonical dept role code, not the display_name
	assert.Contains(t, w.Body.String(), `"role_code":"approver"`)
	assert.Contains(t, w.Body.String(), "Approver Renamed")
}

// P28RL-BL-02: label not found for tenant (no seeded labels) → 400 or 404.
// When the repo has never seeded labels for the tenant the Update call returns
// a not-found error; the handler must surface a non-2xx status (404 preferred,
// but 400 is also acceptable for service-layer validation_error).
func TestP28RL_BL02_NoSeededLabels_NotFound(t *testing.T) {
	tenantID := uuid.New()
	repo := &drhLabelRepo{
		updateFn: func(_ context.Context, _ uuid.UUID, _ domain.DeptRole, _ string, _ int64) (*domain.DeptRoleLabel, error) {
			return nil, domain.NewError(domain.ErrDepartmentNotFound, "role label not found")
		},
	}
	svc := service.NewRoleLabelService(repo, drhCache{})
	h := &RoleLabelHandler{svc: svc}

	body := `{"display_name":"Approver","record_version":1}`
	c, w := buildCtx(http.MethodPatch, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "role_code", "approver")
	h.Patch(c)

	assert.True(t, w.Code == http.StatusNotFound || w.Code == http.StatusBadRequest,
		"expected 404 or 400 when label row is missing, got %d: %s", w.Code, w.Body.String())
}

// ── Dependency resilience ─────────────────────────────────────────────────────

// P28RL-DEP-01: cache.Delete returns an error → still 200 (fail-open, CACHE-2).
// RoleLabelService.Update swallows cache invalidation errors and returns the
// updated label as long as the repo write succeeded (see role_label_service.go
// `_ = s.cache.Delete(…)`).
func TestP28RL_DEP01_CacheErrorSwallowed(t *testing.T) {
	tenantID := uuid.New()

	// errCache is a drhCache that reports an error from Delete.
	errCache := &rlErrCache{}

	repo := &drhLabelRepo{
		updateFn: func(_ context.Context, tid uuid.UUID, code domain.DeptRole, dn string, ver int64) (*domain.DeptRoleLabel, error) {
			return &domain.DeptRoleLabel{
				TenantID:      tid,
				RoleCode:      code,
				DisplayName:   dn,
				RecordVersion: ver + 1,
			}, nil
		},
	}
	svc := service.NewRoleLabelService(repo, errCache)
	h := &RoleLabelHandler{svc: svc}

	body := `{"display_name":"Sign-off","record_version":1}`
	c, w := buildCtx(http.MethodPatch, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "role_code", "approver")
	h.Patch(c)

	// Cache failure must not propagate to the caller — still 200.
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Sign-off")
}

// rlErrCache is a port.Cache stub that returns an error from Delete.
// All other operations delegate to drhCache (no-op, no error).
type rlErrCache struct{}

func (rlErrCache) Get(ctx context.Context, key string) ([]byte, error) {
	return drhCache{}.Get(ctx, key)
}
func (rlErrCache) MGet(ctx context.Context, keys []string) ([][]byte, error) {
	return drhCache{}.MGet(ctx, keys)
}
func (rlErrCache) Set(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return nil
}
func (rlErrCache) SetNX(_ context.Context, _ string, _ []byte, _ time.Duration) (bool, error) {
	return true, nil
}
func (rlErrCache) Delete(_ context.Context, _ ...string) error {
	return errors.New("cache: connection refused")
}
func (rlErrCache) Health(_ context.Context) error { return nil }
func (rlErrCache) Close() error                   { return nil }
