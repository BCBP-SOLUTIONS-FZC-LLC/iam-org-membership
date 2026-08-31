// Phase 19 — Handler happy-path coverage for TenantHandler.
//
// Complements handler_matrix_test.go (which nil-services the input-validation
// early-returns) by wiring REAL services with fake port implementations.
// Exercises the success paths and the service-error → HandleError translations.
//
// Test IDs use the P19-<endpoint-code>-NNN convention (Phase 19 sweep).
//
// happyTenantRepo/happyCacheStub/happyRPClient/happyMembershipRepo/
// happyTxRunner are also used by other _test.go files in this package
// (coverage_backfill_test.go, invite_acl_gm_happy_test.go,
// membership_happy_test.go, p6_i3_p28_coverage_test.go,
// p7_p8_p26_coverage_test.go, tenant_patch_coverage_test.go) — this file
// was originally tenant_delegation_happy_test.go before ADR-0008 v2
// dropped its DelegationHandler half (P-18/19/20); these shared fakes and
// the P-1/P-2 TenantHandler tests moved here so they survive that cut.
package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── shared fakes for this package (prefix "happy" — must not collide with
//     other package-http test files).

type happyTenantRepo struct {
	findByIDFn func(context.Context, uuid.UUID) (*domain.Tenant, error)
	updateFn   func(context.Context, uuid.UUID, *domain.TenantPatch) (*domain.Tenant, error)
	insertFn   func(context.Context, *domain.Tenant) (*domain.Tenant, bool, error)
}

func (f *happyTenantRepo) FindByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	if f.findByIDFn != nil {
		return f.findByIDFn(ctx, id)
	}
	return nil, errors.New("not implemented")
}

func (f *happyTenantRepo) FindByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return f.FindByID(ctx, id)
}

func (f *happyTenantRepo) Update(ctx context.Context, id uuid.UUID, patch *domain.TenantPatch) (*domain.Tenant, error) {
	if f.updateFn != nil {
		return f.updateFn(ctx, id, patch)
	}
	return nil, errors.New("not implemented")
}
func (f *happyTenantRepo) SetRealmSyncPending(context.Context, uuid.UUID) error { return nil }
func (f *happyTenantRepo) Insert(ctx context.Context, t *domain.Tenant) (*domain.Tenant, bool, error) {
	if f.insertFn != nil {
		return f.insertFn(ctx, t)
	}
	return nil, false, errors.New("not implemented")
}

var _ port.TenantRepository = (*happyTenantRepo)(nil)

type happyCacheStub struct{}

func (happyCacheStub) Get(context.Context, string) ([]byte, error) { return nil, nil }
func (happyCacheStub) MGet(context.Context, []string) ([][]byte, error) {
	return nil, nil
}
func (happyCacheStub) Set(context.Context, string, []byte, time.Duration) error {
	return nil
}
func (happyCacheStub) SetNX(context.Context, string, []byte, time.Duration) (bool, error) {
	return true, nil
}
func (happyCacheStub) Delete(context.Context, ...string) error { return nil }
func (happyCacheStub) Health(context.Context) error            { return nil }
func (happyCacheStub) Close() error                            { return nil }

var _ port.Cache = happyCacheStub{}

type happyRPClient struct {
	patchRealmConfigFn   func(context.Context, uuid.UUID, port.RealmConfigPatch) error
	createInvitedUserFn  func(context.Context, port.CreateInvitedUserRequest) (*port.CreateInvitedUserResponse, error)
	deleteUserFn         func(context.Context, uuid.UUID, uuid.UUID) error
	revokeUserSessionsFn func(context.Context, uuid.UUID, uuid.UUID) error
	resetMFAFn           func(context.Context, uuid.UUID, uuid.UUID) error
}

func (f *happyRPClient) CreateInvitedUser(ctx context.Context, req port.CreateInvitedUserRequest) (*port.CreateInvitedUserResponse, error) {
	if f.createInvitedUserFn != nil {
		return f.createInvitedUserFn(ctx, req)
	}
	return &port.CreateInvitedUserResponse{KeycloakUserID: uuid.New()}, nil
}
func (f *happyRPClient) DeleteUser(ctx context.Context, tenantID, keycloakUserID uuid.UUID) error {
	if f.deleteUserFn != nil {
		return f.deleteUserFn(ctx, tenantID, keycloakUserID)
	}
	return nil
}
func (f *happyRPClient) PatchRealmConfig(ctx context.Context, tenantID uuid.UUID, patch port.RealmConfigPatch) error {
	if f.patchRealmConfigFn != nil {
		return f.patchRealmConfigFn(ctx, tenantID, patch)
	}
	return nil
}
func (f *happyRPClient) RevokeUserSessions(ctx context.Context, tenantID, keycloakUserID uuid.UUID) error {
	if f.revokeUserSessionsFn != nil {
		return f.revokeUserSessionsFn(ctx, tenantID, keycloakUserID)
	}
	return nil
}
func (f *happyRPClient) ResetMFA(ctx context.Context, tenantID, keycloakUserID uuid.UUID) error {
	if f.resetMFAFn != nil {
		return f.resetMFAFn(ctx, tenantID, keycloakUserID)
	}
	return nil
}

var _ port.RealmProvisionerClient = (*happyRPClient)(nil)

type happyMembershipRepo struct {
	findByUserIDFn func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error)
}

func (f *happyMembershipRepo) List(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
	return nil, nil
}
func (f *happyMembershipRepo) FindByUserID(ctx context.Context, tenantID, userID uuid.UUID) (*domain.TenantMembership, error) {
	if f.findByUserIDFn != nil {
		return f.findByUserIDFn(ctx, tenantID, userID)
	}
	return nil, errors.New("not implemented")
}
func (f *happyMembershipRepo) Insert(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
	return nil, nil
}
func (f *happyMembershipRepo) SetStatus(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
	return nil, nil
}
func (f *happyMembershipRepo) SoftDelete(context.Context, uuid.UUID, uuid.UUID, int64) error {
	return nil
}
func (f *happyMembershipRepo) CountActive(context.Context, uuid.UUID) (int, error) {
	return 0, nil
}

var _ port.MembershipRepository = (*happyMembershipRepo)(nil)

type happyTxRunner struct{}

func (happyTxRunner) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

var _ port.TxRunner = happyTxRunner{}

// ═════════════════════════════════════════════════════════════════════════
// P-1 · TenantHandler.Get — happy path & error branches
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID: P19-P1-001
// Feature:      P-1 · GET /tenants/{id} · happy path
// Expected:     200 + JSON body with tenant projection
func TestTenantGet_SameTenant_200(t *testing.T) {
	tenantID := uuid.New()
	want := &domain.Tenant{
		ID: tenantID, Slug: "acme", Name: "Acme", Plan: "pro",
		Status: domain.StatusActive, MFAFreshnessSeconds: 300,
		DefaultLocale: "en-US", RecordVersion: 1, UpdatedAt: time.Now().UTC(),
	}
	repo := &happyTenantRepo{findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
		assert.Equal(t, tenantID, id)
		return want, nil
	}}
	svc := service.NewTenantService(repo, happyCacheStub{}, &happyRPClient{})
	h := &TenantHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.Get(c)

	assert.Equal(t, http.StatusOK, w.Code)
	var got TenantResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "acme", got.Slug)
	assert.Equal(t, tenantID, got.ID)
}

// Test Case ID: P19-P1-002
// Feature:      P-1 · service returns not-found → 404
func TestTenantGet_NotFound(t *testing.T) {
	tenantID := uuid.New()
	repo := &happyTenantRepo{findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
		return nil, domain.NewError(domain.ErrTenantNotFound, "tenant not found")
	}}
	svc := service.NewTenantService(repo, happyCacheStub{}, &happyRPClient{})
	h := &TenantHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.Get(c)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ═════════════════════════════════════════════════════════════════════════
// P-2 · TenantHandler.Patch — happy paths + validation + T-15 202
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID: P19-P2-001
// Feature:      P-2 · PATCH /tenants/{id} · name-only patch → 200
func TestTenantPatch_NameOnly_200(t *testing.T) {
	tenantID := uuid.New()
	updatedName := "Acme (renamed)"
	repo := &happyTenantRepo{
		updateFn: func(_ context.Context, id uuid.UUID, patch *domain.TenantPatch) (*domain.Tenant, error) {
			assert.Equal(t, tenantID, id)
			require.NotNil(t, patch.Name)
			assert.Equal(t, updatedName, *patch.Name)
			return &domain.Tenant{ID: id, Name: updatedName, RecordVersion: patch.RecordVersion + 1}, nil
		},
	}
	svc := service.NewTenantService(repo, happyCacheStub{}, &happyRPClient{})
	h := &TenantHandler{svc: svc}

	body := `{"name":"` + updatedName + `","record_version":3}`
	c, w := buildCtx(http.MethodPatch, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.Patch(c)

	assert.Equal(t, http.StatusOK, w.Code)
	var got TenantResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, updatedName, got.Name)
}

// Test Case ID: P19-P2-002
// Feature:      P-2 · T-15 local-first + reconcile · RP fails → 202
func TestTenantPatch_RealmSyncDeferred_202(t *testing.T) {
	tenantID := uuid.New()
	before := &domain.Tenant{ID: tenantID, LocalAccountsEnabled: false}
	repo := &happyTenantRepo{
		findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) { return before, nil },
		updateFn: func(_ context.Context, id uuid.UUID, patch *domain.TenantPatch) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, LocalAccountsEnabled: true, RecordVersion: 2}, nil
		},
	}
	rp := &happyRPClient{patchRealmConfigFn: func(context.Context, uuid.UUID, port.RealmConfigPatch) error {
		return errors.New("keycloak unreachable")
	}}
	svc := service.NewTenantService(repo, happyCacheStub{}, rp)
	h := &TenantHandler{svc: svc}

	body := `{"local_accounts_enabled":true,"record_version":1}`
	c, w := buildCtx(http.MethodPatch, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.Patch(c)

	assert.Equal(t, http.StatusAccepted, w.Code, "T-15: RP error → deferred sync → 202")
}

// Test Case ID: P19-P2-003
// Feature:      P-2 · T-10 mfa_freshness_seconds out of range → 400
func TestTenantPatch_MFAOutOfRange(t *testing.T) {
	tenantID := uuid.New()
	svc := service.NewTenantService(&happyTenantRepo{}, happyCacheStub{}, &happyRPClient{})
	h := &TenantHandler{svc: svc}

	body := `{"mfa_freshness_seconds":30,"record_version":1}`
	c, w := buildCtx(http.MethodPatch, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.Patch(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// Test Case ID: P19-P2-004
// Feature:      P-2 · repo returns optimistic-lock conflict → 409
func TestTenantPatch_OptimisticLock(t *testing.T) {
	tenantID := uuid.New()
	repo := &happyTenantRepo{updateFn: func(context.Context, uuid.UUID, *domain.TenantPatch) (*domain.Tenant, error) {
		return nil, domain.NewError(domain.ErrOptimisticLockConflict, "record_version mismatch")
	}}
	svc := service.NewTenantService(repo, happyCacheStub{}, &happyRPClient{})
	h := &TenantHandler{svc: svc}

	body := `{"name":"x","record_version":99}`
	c, w := buildCtx(http.MethodPatch, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.Patch(c)

	assert.Equal(t, http.StatusConflict, w.Code)
}
