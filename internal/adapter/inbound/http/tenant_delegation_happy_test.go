// Phase 19 — Handler happy-path coverage for TenantHandler and DelegationHandler.
//
// Complements handler_matrix_test.go (which nil-services the input-validation
// early-returns) by wiring REAL services with fake port implementations.
// Exercises the success paths and the service-error → HandleError translations.
//
// Test IDs use the P19-<endpoint-code>-NNN convention (Phase 19 sweep).
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
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── LOCAL fakes for this file (prefix "happy" — must not collide with
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

var _ port.RealmProvisionerClient = (*happyRPClient)(nil)

type happyDelegationRepo struct {
	listFn                          func(context.Context, uuid.UUID) ([]domain.Delegation, error)
	findByIDFn                      func(context.Context, uuid.UUID, uuid.UUID) (*domain.Delegation, error)
	insertFn                        func(context.Context, *domain.Delegation) (*domain.Delegation, error)
	endFn                           func(context.Context, uuid.UUID, uuid.UUID, domain.DelegationStatus, int64) (*domain.Delegation, error)
	listByDelegatorFn               func(context.Context, uuid.UUID, uuid.UUID) ([]domain.Delegation, error)
	listExpiringBeforeFn            func(context.Context, time.Time, int) ([]domain.Delegation, error)
	softDeleteForUserFn             func(context.Context, uuid.UUID, uuid.UUID) ([]domain.Delegation, error)
	findActiveDeptDelegateForUserFn func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.Delegation, error)
}

func (f *happyDelegationRepo) List(ctx context.Context, tenantID uuid.UUID) ([]domain.Delegation, error) {
	if f.listFn != nil {
		return f.listFn(ctx, tenantID)
	}
	return nil, nil
}
func (f *happyDelegationRepo) ListByDelegator(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, error) {
	if f.listByDelegatorFn != nil {
		return f.listByDelegatorFn(ctx, tenantID, delegatorID)
	}
	return nil, nil
}
func (f *happyDelegationRepo) FindByID(ctx context.Context, tenantID, id uuid.UUID) (*domain.Delegation, error) {
	if f.findByIDFn != nil {
		return f.findByIDFn(ctx, tenantID, id)
	}
	return nil, nil
}
func (f *happyDelegationRepo) Insert(ctx context.Context, d *domain.Delegation) (*domain.Delegation, error) {
	if f.insertFn != nil {
		return f.insertFn(ctx, d)
	}
	return d, nil
}
func (f *happyDelegationRepo) End(ctx context.Context, tenantID, id uuid.UUID, status domain.DelegationStatus, expectedVersion int64) (*domain.Delegation, error) {
	if f.endFn != nil {
		return f.endFn(ctx, tenantID, id, status, expectedVersion)
	}
	return nil, nil
}
func (f *happyDelegationRepo) ListExpiringBefore(ctx context.Context, before time.Time, limit int) ([]domain.Delegation, error) {
	if f.listExpiringBeforeFn != nil {
		return f.listExpiringBeforeFn(ctx, before, limit)
	}
	return nil, nil
}
func (f *happyDelegationRepo) SoftDeleteForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.Delegation, error) {
	if f.softDeleteForUserFn != nil {
		return f.softDeleteForUserFn(ctx, tenantID, userID)
	}
	return nil, nil
}
func (f *happyDelegationRepo) FindActiveDeptDelegateForUser(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.Delegation, error) {
	if f.findActiveDeptDelegateForUserFn != nil {
		return f.findActiveDeptDelegateForUserFn(ctx, tenantID, userID, deptID)
	}
	return nil, nil
}
func (f *happyDelegationRepo) ExtendReview(context.Context, uuid.UUID, uuid.UUID, int, int64) (*domain.Delegation, error) {
	return nil, nil
}
func (f *happyDelegationRepo) FindOpenEndedForReview(context.Context, time.Time, int) ([]domain.Delegation, error) {
	return nil, nil
}
func (f *happyDelegationRepo) FindOpenEndedForWarning(context.Context, time.Time, int) ([]domain.Delegation, error) {
	return nil, nil
}
func (f *happyDelegationRepo) MarkReviewNoticeSent(context.Context, uuid.UUID, uuid.UUID, int64) error {
	return nil
}

var _ port.DelegationRepository = (*happyDelegationRepo)(nil)

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

type happyUPClient struct {
	setAvailabilityFn func(context.Context, port.SetAvailabilityRequest) error
}

func (f *happyUPClient) SetAvailability(ctx context.Context, req port.SetAvailabilityRequest) error {
	if f.setAvailabilityFn != nil {
		return f.setAvailabilityFn(ctx, req)
	}
	return nil
}

var _ port.UserProfileClient = (*happyUPClient)(nil)

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

// ═════════════════════════════════════════════════════════════════════════
// P-18 · DelegationHandler.List — happy path
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID: P19-P18-001
func TestDelegationList_Success_200(t *testing.T) {
	tenantID := uuid.New()
	dr := &happyDelegationRepo{listFn: func(_ context.Context, tt uuid.UUID) ([]domain.Delegation, error) {
		assert.Equal(t, tenantID, tt)
		return []domain.Delegation{
			{ID: uuid.New(), DelegatorID: uuid.New(), DelegateID: uuid.New(), Scope: domain.ScopeAll, Status: domain.DelegationActive},
		}, nil
	}}
	svc := service.NewDelegationService(dr, &happyMembershipRepo{}, &happyUPClient{}, nil, happyTxRunner{})
	h := &DelegationHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	h.List(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"items"`)
}

// Test Case ID: P19-P18-002 · repo returns error → 500 via HandleError
func TestDelegationList_RepoError(t *testing.T) {
	dr := &happyDelegationRepo{listFn: func(context.Context, uuid.UUID) ([]domain.Delegation, error) {
		return nil, errors.New("connection reset")
	}}
	svc := service.NewDelegationService(dr, &happyMembershipRepo{}, &happyUPClient{}, nil, happyTxRunner{})
	h := &DelegationHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	h.List(c)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// ═════════════════════════════════════════════════════════════════════════
// P-19 · DelegationHandler.Create — happy path
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID: P19-P19-001 · Create scope=all → 201 + §8.6 UP-first
func TestDelegationCreate_ScopeAll_201(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()

	upCalled := false
	up := &happyUPClient{setAvailabilityFn: func(_ context.Context, req port.SetAvailabilityRequest) error {
		upCalled = true
		assert.Equal(t, tenantID, req.TenantID)
		assert.Equal(t, delegator, req.UserID)
		require.NotNil(t, req.DelegateID)
		assert.Equal(t, delegate, *req.DelegateID)
		return nil
	}}
	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), TenantID: tt, UserID: uu, Status: domain.MembershipActive}, nil
	}}
	dr := &happyDelegationRepo{insertFn: func(_ context.Context, d *domain.Delegation) (*domain.Delegation, error) {
		d.ID = uuid.New()
		d.Status = domain.DelegationActive
		return d, nil
	}}
	svc := service.NewDelegationService(dr, mr, up, nil, happyTxRunner{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"all","reason":"ooo"}`
	rc := tenantOwnerCtx(tenantID)
	rc.UserID = delegator
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.True(t, upCalled, "§8.6 UP.SetAvailability must be called before delegation insert")
}

// Test Case ID: P19-P19-002 · self-delegation → 422 (domain.ErrSelfDelegation)
func TestDelegationCreate_SelfDelegation(t *testing.T) {
	tenantID := uuid.New()
	me := uuid.New()

	svc := service.NewDelegationService(&happyDelegationRepo{}, &happyMembershipRepo{}, &happyUPClient{}, nil, happyTxRunner{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + me.String() + `","scope":"all"}`
	rc := tenantOwnerCtx(tenantID)
	rc.UserID = me
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assert.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
}

// Test Case ID: P19-P19-003 · UP returns error → 422 invalid_delegate
func TestDelegationCreate_UPFailure(t *testing.T) {
	tenantID := uuid.New()
	me, delegate := uuid.New(), uuid.New()

	up := &happyUPClient{setAvailabilityFn: func(context.Context, port.SetAvailabilityRequest) error {
		return errors.New("UP down")
	}}
	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), TenantID: tt, UserID: uu, Status: domain.MembershipActive}, nil
	}}
	svc := service.NewDelegationService(&happyDelegationRepo{}, mr, up, nil, happyTxRunner{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"all"}`
	rc := tenantOwnerCtx(tenantID)
	rc.UserID = me
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assert.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
}

// ═════════════════════════════════════════════════════════════════════════
// P-20 · DelegationHandler.Cancel — happy path + version conflict
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID: P19-P20-001 · Cancel success → 200 · §8.7 pointer-clear
func TestDelegationCancel_Success_200(t *testing.T) {
	tenantID := uuid.New()
	delID := uuid.New()
	delegator := uuid.New()

	dr := &happyDelegationRepo{
		findByIDFn: func(_ context.Context, tt, id uuid.UUID) (*domain.Delegation, error) {
			return &domain.Delegation{ID: id, TenantID: tt, DelegatorID: delegator, Scope: domain.ScopeAll, Status: domain.DelegationActive, RecordVersion: 2}, nil
		},
		endFn: func(_ context.Context, tt, id uuid.UUID, status domain.DelegationStatus, ver int64) (*domain.Delegation, error) {
			assert.Equal(t, domain.DelegationCancelled, status)
			assert.EqualValues(t, 2, ver, "handler forwards record_version=2 from query")
			return &domain.Delegation{ID: id, TenantID: tt, DelegatorID: delegator, Status: status, RecordVersion: 3}, nil
		},
	}
	upCalled := false
	up := &happyUPClient{setAvailabilityFn: func(_ context.Context, req port.SetAvailabilityRequest) error {
		upCalled = true
		assert.True(t, req.ClearDelegate, "§8.7 pointer-clear — never sends status")
		assert.Nil(t, req.Status)
		return nil
	}}
	svc := service.NewDelegationService(dr, &happyMembershipRepo{}, up, nil, happyTxRunner{})
	h := &DelegationHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", delID.String())
	// buildCtx does NOT parse a query string from `path`; assign explicitly.
	c.Request.URL.RawQuery = "record_version=2"
	h.Cancel(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, upCalled, "§8.7 UP.SetAvailability(ClearDelegate=true) must run before End")
}

// Test Case ID: P19-P20-002 · optimistic-lock conflict → 409
func TestDelegationCancel_VersionMismatch(t *testing.T) {
	tenantID := uuid.New()
	dr := &happyDelegationRepo{
		findByIDFn: func(_ context.Context, tt, id uuid.UUID) (*domain.Delegation, error) {
			return &domain.Delegation{ID: id, TenantID: tt, Status: domain.DelegationActive, RecordVersion: 5}, nil
		},
		endFn: func(context.Context, uuid.UUID, uuid.UUID, domain.DelegationStatus, int64) (*domain.Delegation, error) {
			return nil, domain.NewError(domain.ErrOptimisticLockConflict, "record_version mismatch")
		},
	}
	svc := service.NewDelegationService(dr, &happyMembershipRepo{}, &happyUPClient{}, nil, happyTxRunner{})
	h := &DelegationHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", uuid.New().String())
	c.Request.URL.RawQuery = "record_version=1"
	h.Cancel(c)

	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
}

// Test Case ID: P19-P20-003 · delegation not found → 404
func TestDelegationCancel_NotFound(t *testing.T) {
	tenantID := uuid.New()
	dr := &happyDelegationRepo{findByIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.Delegation, error) {
		return nil, domain.NewError(domain.ErrDelegationNotFound, "delegation not found")
	}}
	svc := service.NewDelegationService(dr, &happyMembershipRepo{}, &happyUPClient{}, nil, happyTxRunner{})
	h := &DelegationHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", uuid.New().String())
	h.Cancel(c)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// silence unused warnings when this file is copy-pasted as a template.
var _ = strings.NewReader
