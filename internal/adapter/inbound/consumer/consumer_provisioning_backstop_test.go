// Tests for the signup provisioning backstop (TenantIdentityProvisioned,
// BFF-2 option B) and the missing-tenant retry rule that came with it.
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeProvisioner struct {
	calls   []service.TrialSignupInput
	created bool
	err     error
}

func (f *fakeProvisioner) TrialSignup(_ context.Context, req service.TrialSignupInput) (*domain.Tenant, bool, error) {
	f.calls = append(f.calls, req)
	if f.err != nil {
		return nil, false, f.err
	}
	return &domain.Tenant{ID: req.TenantID}, f.created, nil
}

func tipEnv(t *testing.T, tenantID uuid.UUID, payload map[string]any) events.Envelope[json.RawMessage] {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	env := mkHandleEnv(eventTenantIdentityProvisioned, tenantID.String())
	env.Payload = raw
	return env
}

func validTIPPayload(tenantID, owner uuid.UUID) map[string]any {
	return map[string]any{
		"tenant_id": tenantID, "owner_user_id": owner, "slug": "acme", "name": "Acme Bidding Co",
		"plan": "pro", "signup_type": "trial", "realm": "trial", "realm_type": "shared",
	}
}

func notFound(context.Context, uuid.UUID) (*domain.Tenant, error) {
	return nil, domain.NewError(domain.ErrTenantNotFound, "tenant not found")
}

func backstopConsumer(tenants port.TenantRepository, idemp *htIdempotency, p TenantProvisioner) *MembershipEventConsumer {
	c := NewMembershipEventConsumer(newFakeInbox(&htSyncTxRunner{}, idemp), tenants, nil, nil, 5*time.Minute, nil)
	if p != nil {
		c.WithTenantProvisioner(p)
	}
	return c
}

// ── TenantIdentityProvisioned ────────────────────────────────────────────

func TestBackstop_TenantMissing_RunsI1WithPayloadAndRecords(t *testing.T) {
	tenantID, owner := uuid.New(), uuid.New()
	idemp := &htIdempotency{}
	prov := &fakeProvisioner{created: true}
	c := backstopConsumer(&htTenantRepo{findInclDeletedFn: notFound}, idemp, prov)

	env := tipEnv(t, tenantID, validTIPPayload(tenantID, owner))
	require.NoError(t, c.Handle(context.Background(), env))

	require.Len(t, prov.calls, 1)
	assert.Equal(t, service.TrialSignupInput{
		TenantID: tenantID, Slug: "acme", Name: "Acme Bidding Co", Plan: domain.PlanPro, OwnerUserID: owner,
	}, prov.calls[0], "I-1 must get exactly the payload's values")
	assert.Contains(t, idemp.marked, env.ID, "processed_events recorded after I-1")
}

func TestBackstop_TenantExists_NoI1(t *testing.T) {
	for name, tenant := range map[string]*domain.Tenant{
		"live":         {Status: domain.StatusTrial},
		"soft_deleted": {Status: domain.StatusOffboarded, DeletedAt: ptrTime(time.Now())},
	} {
		t.Run(name, func(t *testing.T) {
			tenantID := uuid.New()
			idemp := &htIdempotency{}
			prov := &fakeProvisioner{created: true}
			repo := &htTenantRepo{findInclDeletedFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) { return tenant, nil }}
			c := backstopConsumer(repo, idemp, prov)

			env := tipEnv(t, tenantID, validTIPPayload(tenantID, uuid.New()))
			require.NoError(t, c.Handle(context.Background(), env))
			assert.Empty(t, prov.calls, "an existing (or offboarded) tenant must never be recreated")
			assert.Contains(t, idemp.marked, env.ID)
		})
	}
}

// The BFF's own I-1 retry won the race: I-1 reports the existing row.
func TestBackstop_LostRaceToBFF_Records(t *testing.T) {
	tenantID := uuid.New()
	idemp := &htIdempotency{}
	prov := &fakeProvisioner{created: false}
	c := backstopConsumer(&htTenantRepo{findInclDeletedFn: notFound}, idemp, prov)

	env := tipEnv(t, tenantID, validTIPPayload(tenantID, uuid.New()))
	require.NoError(t, c.Handle(context.Background(), env))
	assert.Len(t, prov.calls, 1)
	assert.Contains(t, idemp.marked, env.ID)
}

func TestBackstop_PermanentI1Errors_DLQNotRecorded(t *testing.T) {
	for name, perr := range map[string]error{
		"validation":   domain.NewError(domain.ErrValidation, "slug must be 3–63 …"),
		"invalid_plan": domain.NewError(domain.ErrInvalidPlan, "plan must be one of …"),
		"slug_taken":   domain.NewError(domain.ErrSlugAlreadyTaken, "slug already taken"),
	} {
		t.Run(name, func(t *testing.T) {
			tenantID := uuid.New()
			idemp := &htIdempotency{}
			c := backstopConsumer(&htTenantRepo{findInclDeletedFn: notFound}, idemp, &fakeProvisioner{err: perr})

			err := c.Handle(context.Background(), tipEnv(t, tenantID, validTIPPayload(tenantID, uuid.New())))
			require.ErrorIs(t, err, ErrProvisioningRejected, "no retry can fix this — straight to the DLQ")
			assert.Empty(t, idemp.marked, "a rejected event must not be recorded as processed")
		})
	}
}

func TestBackstop_TransientI1Error_RetriedNotRecorded(t *testing.T) {
	tenantID := uuid.New()
	idemp := &htIdempotency{}
	transient := domain.NewError(domain.ErrCatalogUnavailable, "catalog down")
	c := backstopConsumer(&htTenantRepo{findInclDeletedFn: notFound}, idemp, &fakeProvisioner{err: transient})

	err := c.Handle(context.Background(), tipEnv(t, tenantID, validTIPPayload(tenantID, uuid.New())))
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrProvisioningRejected, "a Catalog outage must be retried, not DLQ'd")
	assert.Empty(t, idemp.marked)
}

func TestBackstop_LookupError_RetriedWithoutI1(t *testing.T) {
	tenantID := uuid.New()
	prov := &fakeProvisioner{created: true}
	repo := &htTenantRepo{findInclDeletedFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
		return nil, domain.NewError(domain.ErrDBUnavailable, "db down")
	}}
	c := backstopConsumer(repo, &htIdempotency{}, prov)

	err := c.Handle(context.Background(), tipEnv(t, tenantID, validTIPPayload(tenantID, uuid.New())))
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrProvisioningRejected)
	assert.Empty(t, prov.calls)
}

func TestBackstop_BadPayload_Rejected(t *testing.T) {
	tenantID := uuid.New()
	cases := map[string]events.Envelope[json.RawMessage]{
		"tenant_mismatch": tipEnv(t, tenantID, validTIPPayload(uuid.New(), uuid.New())),
		"not_json": func() events.Envelope[json.RawMessage] {
			e := mkHandleEnv(eventTenantIdentityProvisioned, tenantID.String())
			e.Payload = json.RawMessage(`{"tenant_id": 42`)
			return e
		}(),
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			prov := &fakeProvisioner{created: true}
			c := backstopConsumer(&htTenantRepo{findInclDeletedFn: notFound}, &htIdempotency{}, prov)
			require.ErrorIs(t, c.Handle(context.Background(), env), ErrProvisioningRejected)
			assert.Empty(t, prov.calls)
		})
	}
}

// Without a provisioner the event is retried (and ends in the DLQ), never
// silently acknowledged.
func TestBackstop_NoProvisioner_Retried(t *testing.T) {
	tenantID := uuid.New()
	idemp := &htIdempotency{}
	c := backstopConsumer(&htTenantRepo{findInclDeletedFn: notFound}, idemp, nil)

	err := c.Handle(context.Background(), tipEnv(t, tenantID, validTIPPayload(tenantID, uuid.New())))
	require.Error(t, err)
	assert.Empty(t, idemp.marked)
}

// ── missing tenant on other events ───────────────────────────────────────

func TestAbsentTenant_NoRow_RetriedNotRecorded(t *testing.T) {
	for _, et := range []string{"TenantRealmReady", "DirectPaidSignup", "TenantConverted", "TrialExpired", "TenantPlanChanged"} {
		t.Run(et, func(t *testing.T) {
			idemp := &htIdempotency{}
			repo := &htTenantRepo{findInclDeletedFn: notFound} // LockForProjection → (nil, nil)
			c := backstopConsumer(repo, idemp, nil)

			env := mkHandleEnv(et, uuid.New().String())
			err := c.Handle(context.Background(), env)
			require.ErrorIs(t, err, errTenantNotYetProvisioned, "a not-yet-created tenant must be retried, not dropped")
			assert.Empty(t, idemp.marked, "nothing recorded, so the redelivery is processed")
		})
	}
}

func TestAbsentTenant_NoOpTypes_StillDropped(t *testing.T) {
	for _, et := range []string{"TenantOffboarded"} {
		t.Run(et, func(t *testing.T) {
			idemp := &htIdempotency{}
			looked := false
			repo := &htTenantRepo{findInclDeletedFn: func(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
				looked = true
				return notFound(ctx, id)
			}}
			c := backstopConsumer(repo, idemp, nil)

			env := mkHandleEnv(et, uuid.New().String())
			require.NoError(t, c.Handle(context.Background(), env))
			assert.Contains(t, idemp.marked, env.ID)
			assert.False(t, looked, "no lookup needed for types that are dropped regardless")
		})
	}
}

func TestAbsentTenant_LookupError_Propagated(t *testing.T) {
	boom := errors.New("db down")
	repo := &htTenantRepo{findInclDeletedFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) { return nil, boom }}
	c := backstopConsumer(repo, &htIdempotency{}, nil)

	err := c.Handle(context.Background(), mkHandleEnv("TenantRealmReady", uuid.New().String()))
	require.ErrorIs(t, err, boom)
}

func ptrTime(t time.Time) *time.Time { return &t }
