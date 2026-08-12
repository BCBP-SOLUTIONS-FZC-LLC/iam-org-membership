// Coverage backfill for the P-2 (PATCH /api/v1/tenants/:id) scenarios that
// were previously validated only manually. Adds handler-layer and
// service-layer unit tests for the remaining scenarios in the test plan.
package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
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

// ─────────────────────────────────────────────────────────────────────────
// Malformed request
// ─────────────────────────────────────────────────────────────────────────

// P2-M-01: invalid UUID on :id → 400 invalid_uuid.
func TestTenantPatch_InvalidTenantIDInPath(t *testing.T) {
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodPatch, "/api/v1/tenants/not-a-uuid",
		`{"name":"Acme","record_version":1}`, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "not-a-uuid")
	h.Patch(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// P2-M-02: broken JSON body → 400 validation_error.
func TestTenantPatch_MalformedJSONBody(t *testing.T) {
	tenant := uuid.New()
	svc := service.NewTenantService(&happyTenantRepo{}, happyCacheStub{}, &happyRPClient{})
	h := &TenantHandler{svc: svc}
	c, w := buildCtx(http.MethodPatch, "/", `{"name":`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.Patch(c)
	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// P2-M-03: empty request body → 400.
func TestTenantPatch_EmptyBody(t *testing.T) {
	tenant := uuid.New()
	svc := service.NewTenantService(&happyTenantRepo{}, happyCacheStub{}, &happyRPClient{})
	h := &TenantHandler{svc: svc}
	c, w := buildCtx(http.MethodPatch, "/", ``, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.Patch(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────
// Validation — MFA freshness range and default_locale
// ─────────────────────────────────────────────────────────────────────────

// P2-V-04: mfa_freshness_seconds = 60 (inclusive lower bound) → 200.
func TestTenantPatch_MFABoundaryLowerAccepted(t *testing.T) {
	tenant := uuid.New()
	repo := &happyTenantRepo{
		updateFn: func(_ context.Context, id uuid.UUID, patch *domain.TenantPatch) (*domain.Tenant, error) {
			require.NotNil(t, patch.MFAFreshnessSeconds)
			assert.Equal(t, 60, *patch.MFAFreshnessSeconds)
			return &domain.Tenant{ID: id, MFAFreshnessSeconds: 60, RecordVersion: 2}, nil
		},
	}
	svc := service.NewTenantService(repo, happyCacheStub{}, &happyRPClient{})
	h := &TenantHandler{svc: svc}
	c, w := buildCtx(http.MethodPatch, "/",
		`{"mfa_freshness_seconds":60,"record_version":1}`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.Patch(c)
	assert.Equal(t, http.StatusOK, w.Code)
}

// P2-V-05: mfa_freshness_seconds = 900 (inclusive upper bound) → 200.
func TestTenantPatch_MFABoundaryUpperAccepted(t *testing.T) {
	tenant := uuid.New()
	repo := &happyTenantRepo{
		updateFn: func(_ context.Context, id uuid.UUID, patch *domain.TenantPatch) (*domain.Tenant, error) {
			require.NotNil(t, patch.MFAFreshnessSeconds)
			assert.Equal(t, 900, *patch.MFAFreshnessSeconds)
			return &domain.Tenant{ID: id, MFAFreshnessSeconds: 900, RecordVersion: 2}, nil
		},
	}
	svc := service.NewTenantService(repo, happyCacheStub{}, &happyRPClient{})
	h := &TenantHandler{svc: svc}
	c, w := buildCtx(http.MethodPatch, "/",
		`{"mfa_freshness_seconds":900,"record_version":1}`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.Patch(c)
	assert.Equal(t, http.StatusOK, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────
// Authorization — AUTH-1 write gate + cross-tenant guard
// ─────────────────────────────────────────────────────────────────────────

// P2-A-02: no requestctx (identity missing from ctx) → 401 missing_identity.
func TestTenantPatch_MissingIdentity(t *testing.T) {
	tenant := uuid.New()
	svc := service.NewTenantService(&happyTenantRepo{}, happyCacheStub{}, &happyRPClient{})
	h := &TenantHandler{svc: svc}
	c, w := buildCtx(http.MethodPatch, "/",
		`{"name":"Acme","record_version":1}`, nil)
	setParams(c, "id", tenant.String())
	h.Patch(c)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// P2-A-03: caller tenant_id ≠ path :id → 403 insufficient_role.
func TestTenantPatch_CrossTenantForbidden(t *testing.T) {
	pathTenant := uuid.New()
	callerTenant := uuid.New()
	svc := service.NewTenantService(&happyTenantRepo{}, happyCacheStub{}, &happyRPClient{})
	h := &TenantHandler{svc: svc}
	c, w := buildCtx(http.MethodPatch, "/",
		`{"name":"Acme","record_version":1}`, tenantOwnerCtx(callerTenant))
	setParams(c, "id", pathTenant.String())
	h.Patch(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// P2-A-04: caller has only 'member' role → 403 tenant_owner required.
func TestTenantPatch_MemberRoleForbidden(t *testing.T) {
	tenant := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenant, Roles: []string{"member"}}
	svc := service.NewTenantService(&happyTenantRepo{}, happyCacheStub{}, &happyRPClient{})
	h := &TenantHandler{svc: svc}
	c, w := buildCtx(http.MethodPatch, "/",
		`{"name":"Acme","record_version":1}`, rc)
	setParams(c, "id", tenant.String())
	h.Patch(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
	assert.Contains(t, w.Body.String(), "tenant_owner")
}

// P2-A-05: caller has tenant_admin (but NOT tenant_owner) → 403.
func TestTenantPatch_TenantAdminForbidden(t *testing.T) {
	tenant := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: tenant, Roles: []string{"tenant_admin"}}
	svc := service.NewTenantService(&happyTenantRepo{}, happyCacheStub{}, &happyRPClient{})
	h := &TenantHandler{svc: svc}
	c, w := buildCtx(http.MethodPatch, "/",
		`{"name":"Acme","record_version":1}`, rc)
	setParams(c, "id", tenant.String())
	h.Patch(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// P2-A-06: tenant_owner passes the write gate.
func TestTenantPatch_TenantOwnerPassesGate(t *testing.T) {
	tenant := uuid.New()
	repo := &happyTenantRepo{
		updateFn: func(_ context.Context, id uuid.UUID, _ *domain.TenantPatch) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, Name: "OK", RecordVersion: 2}, nil
		},
	}
	svc := service.NewTenantService(repo, happyCacheStub{}, &happyRPClient{})
	h := &TenantHandler{svc: svc}
	c, w := buildCtx(http.MethodPatch, "/",
		`{"name":"OK","record_version":1}`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.Patch(c)
	assert.Equal(t, http.StatusOK, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────
// Happy path — locale, MFA, local_accounts_enabled, combined patch
// ─────────────────────────────────────────────────────────────────────────

// P2-H-02: locale update → 200. Also verifies the cache is called for
// invalidation (om:tenant + om:locale).
func TestTenantPatch_LocaleUpdateInvalidatesCache(t *testing.T) {
	tenant := uuid.New()
	repo := &happyTenantRepo{
		updateFn: func(_ context.Context, id uuid.UUID, patch *domain.TenantPatch) (*domain.Tenant, error) {
			require.NotNil(t, patch.DefaultLocale)
			assert.Equal(t, "fr-FR", *patch.DefaultLocale)
			return &domain.Tenant{ID: id, DefaultLocale: "fr-FR", RecordVersion: 2}, nil
		},
	}
	cache := &recordingCache{}
	svc := service.NewTenantService(repo, cache, &happyRPClient{})
	h := &TenantHandler{svc: svc}
	c, w := buildCtx(http.MethodPatch, "/",
		`{"default_locale":"fr-FR","record_version":1}`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.Patch(c)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotEmpty(t, cache.deleted, "invalidateCache must delete at least one key on locale change (CACHE-6)")
}

// P2-H-03: mfa_freshness_seconds in-range (300) → 200.
func TestTenantPatch_MFAFreshnessInRange(t *testing.T) {
	tenant := uuid.New()
	repo := &happyTenantRepo{
		updateFn: func(_ context.Context, id uuid.UUID, patch *domain.TenantPatch) (*domain.Tenant, error) {
			require.NotNil(t, patch.MFAFreshnessSeconds)
			assert.Equal(t, 300, *patch.MFAFreshnessSeconds)
			return &domain.Tenant{ID: id, MFAFreshnessSeconds: 300, RecordVersion: 2}, nil
		},
	}
	svc := service.NewTenantService(repo, happyCacheStub{}, &happyRPClient{})
	h := &TenantHandler{svc: svc}
	c, w := buildCtx(http.MethodPatch, "/",
		`{"mfa_freshness_seconds":300,"record_version":1}`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.Patch(c)
	assert.Equal(t, http.StatusOK, w.Code)
}

// P2-H-05: local_accounts_enabled toggled + RP.PatchRealmConfig succeeds → 200
// (synchronous, no deferred sync).
func TestTenantPatch_LocalAccountsToggled_RPSucceeds(t *testing.T) {
	tenant := uuid.New()
	before := &domain.Tenant{ID: tenant, LocalAccountsEnabled: false}
	rpCalled := false
	repo := &happyTenantRepo{
		findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) { return before, nil },
		updateFn: func(_ context.Context, id uuid.UUID, _ *domain.TenantPatch) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, LocalAccountsEnabled: true, RecordVersion: 2}, nil
		},
	}
	rp := &happyRPClient{patchRealmConfigFn: func(context.Context, uuid.UUID, port.RealmConfigPatch) error {
		rpCalled = true
		return nil
	}}
	svc := service.NewTenantService(repo, happyCacheStub{}, rp)
	h := &TenantHandler{svc: svc}
	c, w := buildCtx(http.MethodPatch, "/",
		`{"local_accounts_enabled":true,"record_version":1}`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.Patch(c)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, rpCalled, "RP.PatchRealmConfig must be invoked on change (T-15 Option A)")
}

// P2-H-06: combined patch (name + locale + mfa) → 200 with all fields applied.
func TestTenantPatch_CombinedPatchAllFieldsApplied(t *testing.T) {
	tenant := uuid.New()
	repo := &happyTenantRepo{
		updateFn: func(_ context.Context, id uuid.UUID, patch *domain.TenantPatch) (*domain.Tenant, error) {
			require.NotNil(t, patch.Name)
			require.NotNil(t, patch.DefaultLocale)
			require.NotNil(t, patch.MFAFreshnessSeconds)
			assert.Equal(t, "Acme Renamed", *patch.Name)
			assert.Equal(t, "es-ES", *patch.DefaultLocale)
			assert.Equal(t, 600, *patch.MFAFreshnessSeconds)
			return &domain.Tenant{ID: id, Name: *patch.Name, DefaultLocale: *patch.DefaultLocale,
				MFAFreshnessSeconds: *patch.MFAFreshnessSeconds, RecordVersion: 2}, nil
		},
	}
	svc := service.NewTenantService(repo, happyCacheStub{}, &happyRPClient{})
	h := &TenantHandler{svc: svc}
	body := `{"name":"Acme Renamed","default_locale":"es-ES","mfa_freshness_seconds":600,"record_version":1}`
	c, w := buildCtx(http.MethodPatch, "/", body, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.Patch(c)
	assert.Equal(t, http.StatusOK, w.Code)
	var got TenantResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "Acme Renamed", got.Name)
}

// ─────────────────────────────────────────────────────────────────────────
// Concurrency
// ─────────────────────────────────────────────────────────────────────────

// P2-CONC-02: record_version omitted (defaults to 0) → treated as stale
// version → repo raises optimistic-lock conflict → 409.
func TestTenantPatch_MissingRecordVersionRaises409(t *testing.T) {
	tenant := uuid.New()
	repo := &happyTenantRepo{
		updateFn: func(_ context.Context, _ uuid.UUID, patch *domain.TenantPatch) (*domain.Tenant, error) {
			assert.Equal(t, int64(0), patch.RecordVersion, "omitted record_version arrives as 0")
			return nil, domain.NewError(domain.ErrOptimisticLockConflict, "record_version mismatch")
		},
	}
	svc := service.NewTenantService(repo, happyCacheStub{}, &happyRPClient{})
	h := &TenantHandler{svc: svc}
	c, w := buildCtx(http.MethodPatch, "/",
		`{"name":"Acme"}`, tenantOwnerCtx(tenant)) // no record_version field
	setParams(c, "id", tenant.String())
	h.Patch(c)
	assert.Equal(t, http.StatusConflict, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────
// Not Found
// ─────────────────────────────────────────────────────────────────────────

// P2-NF-01 + P2-NF-02: soft-deleted / never-existed tenant → 404.
// Repo raises ErrTenantNotFound; handler maps to 404.
func TestTenantPatch_TenantNotFound(t *testing.T) {
	tenant := uuid.New()
	repo := &happyTenantRepo{
		updateFn: func(context.Context, uuid.UUID, *domain.TenantPatch) (*domain.Tenant, error) {
			return nil, domain.NewError(domain.ErrTenantNotFound, "tenant not found")
		},
	}
	svc := service.NewTenantService(repo, happyCacheStub{}, &happyRPClient{})
	h := &TenantHandler{svc: svc}
	c, w := buildCtx(http.MethodPatch, "/",
		`{"name":"Acme","record_version":1}`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.Patch(c)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────
// Cache
// ─────────────────────────────────────────────────────────────────────────

// P2-CA-01: successful PATCH deletes both om:tenant + om:locale on locale
// change; deletes only om:tenant for non-locale changes.
func TestTenantPatch_CacheInvalidatesBothKeysOnLocaleChange(t *testing.T) {
	tenant := uuid.New()
	repo := &happyTenantRepo{
		updateFn: func(_ context.Context, id uuid.UUID, _ *domain.TenantPatch) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, RecordVersion: 2}, nil
		},
	}
	cache := &recordingCache{}
	svc := service.NewTenantService(repo, cache, &happyRPClient{})
	h := &TenantHandler{svc: svc}
	c, w := buildCtx(http.MethodPatch, "/",
		`{"default_locale":"de-DE","record_version":1}`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.Patch(c)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, joinKeys(cache.deleted), "om:tenant", "om:tenant key should be invalidated")
	assert.Contains(t, joinKeys(cache.deleted), "om:locale", "om:locale key should be invalidated on locale change")
}

// P2-CA-02: cache Delete failure must NOT roll back the PATCH (CACHE-9 advisory).
func TestTenantPatch_CacheDeleteFailure_DoesNotRollBack(t *testing.T) {
	tenant := uuid.New()
	repo := &happyTenantRepo{
		updateFn: func(_ context.Context, id uuid.UUID, _ *domain.TenantPatch) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, RecordVersion: 2}, nil
		},
	}
	cache := &erroringCache{}
	svc := service.NewTenantService(repo, cache, &happyRPClient{})
	h := &TenantHandler{svc: svc}
	c, w := buildCtx(http.MethodPatch, "/",
		`{"name":"Acme","record_version":1}`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.Patch(c)
	assert.Equal(t, http.StatusOK, w.Code, "cache failure must not surface as an error to the client (CACHE-9 advisory)")
}

// ─────────────────────────────────────────────────────────────────────────
// Local test-doubles used only in this file.
// ─────────────────────────────────────────────────────────────────────────

// recordingCache tracks every Delete/Set/SetNX invocation so tests can
// verify the cache-invalidation surface without needing Valkey.
type recordingCache struct {
	deleted [][]string
}

func (c *recordingCache) Get(context.Context, string) ([]byte, error)      { return nil, nil }
func (c *recordingCache) MGet(context.Context, []string) ([][]byte, error) { return nil, nil }
func (c *recordingCache) Set(context.Context, string, []byte, time.Duration) error {
	return nil
}
func (c *recordingCache) SetNX(context.Context, string, []byte, time.Duration) (bool, error) {
	return true, nil
}
func (c *recordingCache) Delete(_ context.Context, keys ...string) error {
	c.deleted = append(c.deleted, keys)
	return nil
}
func (c *recordingCache) Health(context.Context) error { return nil }
func (c *recordingCache) Close() error                 { return nil }

// erroringCache always fails on Delete — verifies CACHE-9 advisory guarantee
// that cache failures do NOT surface as PATCH errors.
type erroringCache struct{}

func (erroringCache) Get(context.Context, string) ([]byte, error)      { return nil, nil }
func (erroringCache) MGet(context.Context, []string) ([][]byte, error) { return nil, nil }
func (erroringCache) Set(context.Context, string, []byte, time.Duration) error {
	return nil
}
func (erroringCache) SetNX(context.Context, string, []byte, time.Duration) (bool, error) {
	return true, nil
}
func (erroringCache) Delete(context.Context, ...string) error {
	return errors.New("valkey unreachable")
}
func (erroringCache) Health(context.Context) error { return nil }
func (erroringCache) Close() error                 { return nil }

// Interface-conformance guards.
var _ port.Cache = (*recordingCache)(nil)
var _ port.Cache = (*erroringCache)(nil)

// joinKeys flattens the recorded key groups for substring assertions.
func joinKeys(groups [][]string) string {
	var b strings.Builder
	for _, g := range groups {
		for _, k := range g {
			b.WriteString(k)
			b.WriteString(" ")
		}
	}
	return b.String()
}
