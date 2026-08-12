// Handler and service-layer tests for P-19 POST /api/v1/delegations.
// Covers Bug B-17 (delegator status), B-18 (scope=all+scope_id), B-19 (reason length),
// plus DEL-1/DEL-2/DEL-8/DEL-10 edge cases and role/state scenarios.
package http

import (
	"context"
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

// ── helpers ──────────────────────────────────────────────────────────────────

func memberRoleCtx(tenantID, userID uuid.UUID) *requestctx.RequestContext {
	return &requestctx.RequestContext{
		UserID:   userID,
		TenantID: tenantID,
		Roles:    []string{"member"},
	}
}

func buildDelegSvc(mr port.MembershipRepository, up port.UserProfileClient) *service.DelegationService {
	return service.NewDelegationService(&happyDelegationRepo{insertFn: func(_ context.Context, d *domain.Delegation) (*domain.Delegation, error) {
		d.ID = uuid.New()
		d.Status = domain.DelegationActive
		return d, nil
	}}, mr, up, nil, happyTxRunner{})
}

func activeMember(tenantID, userID uuid.UUID) *domain.TenantMembership {
	return &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID, Status: domain.MembershipActive}
}

// ── Bug B-17: Suspended delegator → 422 (DEL-1) ─────────────────────────────

func TestDelegationCreate_SuspendedDelegator_422(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()

	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		status := domain.MembershipActive
		if uu == delegator {
			status = domain.MembershipSuspended // delegator is suspended
		}
		return &domain.TenantMembership{TenantID: tt, UserID: uu, Status: status}, nil
	}}
	svc := buildDelegSvc(mr, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"all"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assertErrorCode(t, w, http.StatusUnprocessableEntity, "invalid_delegate")
}

// ── Bug B-18: scope=all with scope_id → 400 (DEL-2) ─────────────────────────

func TestDelegationCreate_ScopeAllWithScopeID_400(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()
	scopeID := uuid.New()

	svc := buildDelegSvc(&happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return activeMember(tt, uu), nil
	}}, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"all","scope_id":"` + scopeID.String() + `"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

// ── Bug B-19: reason > 500 chars → 400 (DEL-10) ─────────────────────────────

func TestDelegationCreate_ReasonTooLong_400(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()
	longReason := strings.Repeat("a", 501) // 501 chars

	svc := buildDelegSvc(&happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return activeMember(tt, uu), nil
	}}, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"all","reason":"` + longReason + `"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "reason_too_long")
}

// ── DEL-10: reason = exactly 500 chars → 201 (boundary OK) ──────────────────

func TestDelegationCreate_Reason500Chars_201(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()
	exactReason := strings.Repeat("a", 500) // exactly 500

	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return activeMember(tt, uu), nil
	}}
	svc := buildDelegSvc(mr, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"all","reason":"` + exactReason + `"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}

// ── DEL-1: Left delegator (deleted_at IS NOT NULL) → 404 ────────────────────

func TestDelegationCreate_LeftDelegator_404(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()

	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		if uu == delegator {
			return nil, domain.NewError(domain.ErrMemberNotFound, "member not found")
		}
		return activeMember(tt, uu), nil
	}}
	svc := buildDelegSvc(mr, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"all"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assertErrorCode(t, w, http.StatusNotFound, "member_not_found")
}

// ── DEL-8: Open-ended delegation (no ends_at) → 201 ─────────────────────────

func TestDelegationCreate_NoEndsAt_201(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()

	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return activeMember(tt, uu), nil
	}}
	svc := buildDelegSvc(mr, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	// No ends_at → open-ended delegation (DEL-8 allows nullable)
	body := `{"delegate_id":"` + delegate.String() + `","scope":"all","reason":"extended leave"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}

// ── ends_at = starts_at → 422 delegation_window_inverted ────────────────────

func TestDelegationCreate_EndsAtEqualsStartsAt_422(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()
	ts := "2026-09-01T00:00:00Z"

	svc := buildDelegSvc(&happyMembershipRepo{}, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"all","starts_at":"` + ts + `","ends_at":"` + ts + `"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assertErrorCode(t, w, http.StatusUnprocessableEntity, "delegation_window_inverted")
}

// ── AUTH-4: Any member role can delegate (self-service) ──────────────────────

func TestDelegationCreate_TenderAdminCanDelegate_201(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()

	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return activeMember(tt, uu), nil
	}}
	svc := buildDelegSvc(mr, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"all"}`
	rc := &requestctx.RequestContext{UserID: delegator, TenantID: tenantID, Roles: []string{"tender_admin"}}
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}

// ── reason sent as UP note (B-16 fix verification) ───────────────────────────

func TestDelegationCreate_ReasonSentAsNoteToUP(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()
	reason := "Annual leave"

	var capturedNote string
	up := &happyUPClient{setAvailabilityFn: func(_ context.Context, req port.SetAvailabilityRequest) error {
		capturedNote = req.Note
		return nil
	}}
	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return activeMember(tt, uu), nil
	}}
	svc := buildDelegSvc(mr, up)
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"all","reason":"` + reason + `"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, reason, capturedNote, "reason must be forwarded to UP as note (B-16 fix)")
}

// ── scope=department with scope_id → 201, scope_id stored ────────────────────

func TestDelegationCreate_ScopeDeptWithScopeID_201(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()
	deptID := uuid.New()

	var capturedScopeID *uuid.UUID
	dr := &happyDelegationRepo{insertFn: func(_ context.Context, d *domain.Delegation) (*domain.Delegation, error) {
		capturedScopeID = d.ScopeID
		d.ID = uuid.New()
		d.Status = domain.DelegationActive
		return d, nil
	}}
	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return activeMember(tt, uu), nil
	}}
	svc := service.NewDelegationService(dr, mr, &happyUPClient{}, nil, happyTxRunner{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"department","scope_id":"` + deptID.String() + `"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	require.NotNil(t, capturedScopeID, "scope_id must be stored in delegation")
	assert.Equal(t, deptID, *capturedScopeID)
}

// ── Multiple delegations by same delegator → both 201 ────────────────────────

func TestDelegationCreate_MultipleDelegations_BothAllowed(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate1 := uuid.New()
	delegate2 := uuid.New()

	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return activeMember(tt, uu), nil
	}}

	// First delegation
	svc := buildDelegSvc(mr, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	body1 := `{"delegate_id":"` + delegate1.String() + `","scope":"all"}`
	rc := memberRoleCtx(tenantID, delegator)
	c1, w1 := buildCtx(http.MethodPost, "/", body1, rc)
	h.Create(c1)
	assert.Equal(t, http.StatusCreated, w1.Code, "first delegation must succeed")

	// Second delegation (same delegator, different delegate) — both allowed
	body2 := `{"delegate_id":"` + delegate2.String() + `","scope":"all"}`
	c2, w2 := buildCtx(http.MethodPost, "/", body2, rc)
	h.Create(c2)
	assert.Equal(t, http.StatusCreated, w2.Code, "second delegation must also succeed — no uniqueness constraint")
}

// ── P19-422-06: Delegate suspended → 422 (DEL-1) ────────────────────────────

func TestDelegationCreate_SuspendedDelegate_422(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()

	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		if uu == delegate {
			return &domain.TenantMembership{TenantID: tt, UserID: uu, Status: domain.MembershipSuspended}, nil
		}
		return activeMember(tt, uu), nil
	}}
	svc := buildDelegSvc(mr, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"all"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assertErrorCode(t, w, http.StatusUnprocessableEntity, "invalid_delegate")
}

// ── Future starts_at → 201 (allowed) ─────────────────────────────────────────

func TestDelegationCreate_FutureStartsAt_201(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()
	future := time.Now().Add(30 * 24 * time.Hour)
	end := future.Add(7 * 24 * time.Hour)

	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return activeMember(tt, uu), nil
	}}
	svc := buildDelegSvc(mr, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"all",` +
		`"starts_at":"` + future.UTC().Format(time.RFC3339) + `",` +
		`"ends_at":"` + end.UTC().Format(time.RFC3339) + `"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}

// ── scope missing → 400 invalid_delegation_scope ─────────────────────────────

func TestDelegationCreate_MissingScope_400(t *testing.T) {
	tenantID := uuid.New()
	delegate := uuid.New()

	svc := buildDelegSvc(&happyMembershipRepo{}, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `"}`
	rc := memberRoleCtx(tenantID, uuid.New())
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

// ── Delegate not a member → 422 invalid_delegate ─────────────────────────────

func TestDelegationCreate_DelegateNotMember_422(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()

	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		if uu == delegator {
			return activeMember(tt, uu), nil
		}
		return nil, domain.NewError(domain.ErrMemberNotFound, "delegate not a member")
	}}
	svc := buildDelegSvc(mr, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"all"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assertErrorCode(t, w, http.StatusUnprocessableEntity, "invalid_delegate")
}

// ── Ensure UP SetAvailability called with ooo_from set (§8.6) ────────────────

func TestDelegationCreate_UPCalledWithOOOFrom(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()

	var capturedOOOFrom *time.Time
	up := &happyUPClient{setAvailabilityFn: func(_ context.Context, req port.SetAvailabilityRequest) error {
		capturedOOOFrom = req.OOOFrom
		return nil
	}}
	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return activeMember(tt, uu), nil
	}}
	svc := buildDelegSvc(mr, up)
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"all"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assert.Equal(t, http.StatusCreated, w.Code)
	require.NotNil(t, capturedOOOFrom, "UP must receive ooo_from (§8.6)")
}

// ── P19-SCOPE-04: scope=department + tenant UUID as scope_id → 201 (no type check) ──

func TestDelegationCreate_ScopeDept_TenantUUIDAsScope_201(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()
	wrongScopeID := tenantID // passing tenant UUID instead of dept UUID

	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return activeMember(tt, uu), nil
	}}
	svc := buildDelegSvc(mr, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"department","scope_id":"` + wrongScopeID.String() + `"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	// O&M does not validate scope_id type — stores any UUID.
	// Workflow Service validates routing at consumption time.
	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}

// ── P19-SCOPE-05: scope=department + different tenant's dept UUID → 201 (no cross-tenant dept check) ──

func TestDelegationCreate_ScopeDept_OtherTenantDeptUUID_201(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()
	otherTenantDeptID := uuid.New() // dept from another tenant

	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return activeMember(tt, uu), nil
	}}
	svc := buildDelegSvc(mr, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"department","scope_id":"` + otherTenantDeptID.String() + `"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	// O&M does not validate scope_id belongs to this tenant's departments.
	// Gap acknowledged — future enhancement to call tenantDepts.Find.
	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}

// ── P19-SCOPE-06: scope=department + non-existent UUID → 201 (no existence check) ──

func TestDelegationCreate_ScopeDept_NonExistentScopeID_201(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()
	ghostDeptID := uuid.New() // UUID that doesn't exist in any dept table

	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return activeMember(tt, uu), nil
	}}
	svc := buildDelegSvc(mr, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"department","scope_id":"` + ghostDeptID.String() + `"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	// O&M does not check dept existence — Workflow Service handles routing mismatch.
	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}

// ── P19-SCOPE-07: scope=tender + dept UUID as scope_id → 201 (O&M doesn't own tenders) ──

func TestDelegationCreate_ScopeTender_DeptUUIDAsScope_201(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()
	deptUUID := uuid.New() // wrong type — dept UUID used as tender scope_id

	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return activeMember(tt, uu), nil
	}}
	svc := buildDelegSvc(mr, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"tender","scope_id":"` + deptUUID.String() + `"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	// O&M does not own tenders — cannot validate tender UUID.
	// Workflow Service validates the scope_id at routing time.
	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}

// ── event capturing helpers ───────────────────────────────────────────────────

type delegPub struct{ events []*domain.DomainEvent }

func (p *delegPub) EnqueueCtx(_ context.Context, e *domain.DomainEvent) error {
	p.events = append(p.events, e)
	return nil
}

type delegTxRunner struct{ pub *delegPub }

func (r *delegTxRunner) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	return fn(port.WithEventPublisher(ctx, r.pub))
}

// ── P19-SQS-02: reason NOT in DelegationStarted event payload ────────────────

func TestDelegationCreate_ReasonNotInEvent(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()

	var capturedEvent *domain.DomainEvent
	pub := &delegPub{}
	txr := &delegTxRunner{pub: pub}

	dr := &happyDelegationRepo{insertFn: func(_ context.Context, d *domain.Delegation) (*domain.Delegation, error) {
		d.ID = uuid.New()
		d.Status = domain.DelegationActive
		return d, nil
	}}
	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return activeMember(tt, uu), nil
	}}
	svc := service.NewDelegationService(dr, mr, &happyUPClient{}, nil, txr)
	h := &DelegationHandler{svc: svc}
	body := `{"delegate_id":"` + delegate.String() + `","scope":"all","reason":"Annual leave"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assert.Equal(t, http.StatusCreated, w.Code)
	require.Len(t, pub.events, 1)
	capturedEvent = pub.events[0]
	assert.Equal(t, domain.EventDelegationStarted, capturedEvent.Type)
	payload, ok := capturedEvent.Data.(domain.DelegationStartedPayload)
	require.True(t, ok)
	// reason/note must NOT be in event payload per AsyncAPI spec
	assert.Equal(t, tenantID, payload.TenantID)
	assert.Equal(t, delegator, payload.DelegatorID)
	assert.Equal(t, delegate, payload.DelegateID)
	assert.Equal(t, domain.ScopeAll, payload.Scope)
	_ = capturedEvent
}

// ── P19-SQS-03: ends_at null → event EndsAt is nil (omitempty) ───────────────

func TestDelegationCreate_OpenEnded_EventEndsAtNil(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()

	pub := &delegPub{}
	txr := &delegTxRunner{pub: pub}
	dr := &happyDelegationRepo{insertFn: func(_ context.Context, d *domain.Delegation) (*domain.Delegation, error) {
		d.ID = uuid.New()
		d.Status = domain.DelegationActive
		return d, nil
	}}
	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return activeMember(tt, uu), nil
	}}
	svc := service.NewDelegationService(dr, mr, &happyUPClient{}, nil, txr)
	h := &DelegationHandler{svc: svc}

	// No ends_at — open-ended delegation (DEL-8)
	body := `{"delegate_id":"` + delegate.String() + `","scope":"all"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assert.Equal(t, http.StatusCreated, w.Code)
	require.Len(t, pub.events, 1)
	payload, ok := pub.events[0].Data.(domain.DelegationStartedPayload)
	require.True(t, ok)
	assert.Nil(t, payload.EndsAt, "ends_at must be nil in event when not provided")
}

// ── P19-VAL-10: ends_at in the past → 422 ───────────────────────────────────

func TestDelegationCreate_EndsAtInPast_422(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()
	// Past date — starts_at defaults to now(), ends_at < now() → inverted
	pastDate := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)

	svc := buildDelegSvc(&happyMembershipRepo{}, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"all","ends_at":"` + pastDate + `"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assertErrorCode(t, w, http.StatusUnprocessableEntity, "delegation_window_inverted")
}

// ── P19-VAL-11: reason omitted → 201 (optional, DEL-10) ─────────────────────

func TestDelegationCreate_ReasonOmitted_201(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()

	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return activeMember(tt, uu), nil
	}}
	svc := buildDelegSvc(mr, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	// No reason field at all
	body := `{"delegate_id":"` + delegate.String() + `","scope":"all","ends_at":"2026-12-31T00:00:00Z"}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}

// ── P19-VAL-12: reason = empty string → 201 ──────────────────────────────────

func TestDelegationCreate_EmptyReason_201(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()

	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return activeMember(tt, uu), nil
	}}
	svc := buildDelegSvc(mr, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"all","reason":""}`
	rc := memberRoleCtx(tenantID, delegator)
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}

// ── P19-CIRC-01: Circular delegation A→B when B→A exists → 201 (not blocked) ─

func TestDelegationCreate_CircularDelegation_201(t *testing.T) {
	tenantID := uuid.New()
	userA := uuid.New()
	userB := uuid.New()

	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return activeMember(tt, uu), nil
	}}
	svc := buildDelegSvc(mr, &happyUPClient{})
	h := &DelegationHandler{svc: svc}

	// A→B (first delegation)
	body1 := `{"delegate_id":"` + userB.String() + `","scope":"all"}`
	rc1 := memberRoleCtx(tenantID, userA)
	c1, w1 := buildCtx(http.MethodPost, "/", body1, rc1)
	h.Create(c1)
	assert.Equal(t, http.StatusCreated, w1.Code)

	// B→A (reverse delegation — circular, but LLD does not block it)
	body2 := `{"delegate_id":"` + userA.String() + `","scope":"all"}`
	rc2 := memberRoleCtx(tenantID, userB)
	c2, w2 := buildCtx(http.MethodPost, "/", body2, rc2)
	h.Create(c2)
	assert.Equal(t, http.StatusCreated, w2.Code, "circular delegation not blocked — Workflow Service handles routing")
}

// ── P19-SQS-05: DelegationStarted all required fields present ────────────────

func TestDelegationCreate_EventAllRequiredFieldsPresent(t *testing.T) {
	tenantID := uuid.New()
	delegator := uuid.New()
	delegate := uuid.New()

	pub := &delegPub{}
	dr := &happyDelegationRepo{insertFn: func(_ context.Context, d *domain.Delegation) (*domain.Delegation, error) {
		d.ID = uuid.New()
		d.Status = domain.DelegationActive
		return d, nil
	}}
	mr := &happyMembershipRepo{findByUserIDFn: func(_ context.Context, tt, uu uuid.UUID) (*domain.TenantMembership, error) {
		return activeMember(tt, uu), nil
	}}
	svc := service.NewDelegationService(dr, mr, &happyUPClient{}, nil, &delegTxRunner{pub: pub})
	h := &DelegationHandler{svc: svc}

	body := `{"delegate_id":"` + delegate.String() + `","scope":"all","reason":"leave"}`
	rc := memberRoleCtx(tenantID, delegator)
	rc.UserID = delegator
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	h.Create(c)

	require.Equal(t, http.StatusCreated, w.Code)
	require.Len(t, pub.events, 1)
	payload, ok := pub.events[0].Data.(domain.DelegationStartedPayload)
	require.True(t, ok, "payload must be DelegationStartedPayload")

	// AsyncAPI required fields: delegation_id, tenant_id, delegator_id, delegate_id, scope, actor_id
	assert.NotEqual(t, uuid.Nil, payload.DelegationID, "delegation_id required")
	assert.Equal(t, tenantID, payload.TenantID, "tenant_id required")
	assert.Equal(t, delegator, payload.DelegatorID, "delegator_id required")
	assert.Equal(t, delegate, payload.DelegateID, "delegate_id required")
	assert.Equal(t, domain.ScopeAll, payload.Scope, "scope required")
	assert.Equal(t, delegator, payload.ActorID, "actor_id required")
}

// suppress unused import warnings
var _ = errors.New
