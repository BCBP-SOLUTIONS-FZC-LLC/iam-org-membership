// Tests for EVT-18 (CD-50): an event for a tenant with no row yet is parked
// and acknowledged, then replayed in order once the tenant exists.
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeParkedStore is an in-memory pending_tenant_events. Paired with
// parkedTxRunner, a transaction that fails restores the store, the way a
// rollback restores a claimed (deleted) row.
type fakeParkedStore struct {
	events  map[uuid.UUID]port.ParkedTenantEvent
	locks   []uuid.UUID
	parkErr error
	listErr error
}

func newFakeParkedStore() *fakeParkedStore {
	return &fakeParkedStore{events: map[uuid.UUID]port.ParkedTenantEvent{}}
}

func (s *fakeParkedStore) Park(_ context.Context, ev port.ParkedTenantEvent) error {
	if s.parkErr != nil {
		return s.parkErr
	}
	if _, ok := s.events[ev.EventID]; !ok {
		s.events[ev.EventID] = ev
	}
	return nil
}

func (s *fakeParkedStore) ListForTenant(_ context.Context, tenantID uuid.UUID) ([]port.ParkedTenantEvent, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []port.ParkedTenantEvent
	for _, ev := range s.events {
		if ev.TenantID == tenantID {
			out = append(out, ev)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OccurredAt.Before(out[j].OccurredAt) })
	return out, nil
}

func (s *fakeParkedStore) Claim(_ context.Context, eventID uuid.UUID) (bool, error) {
	if _, ok := s.events[eventID]; !ok {
		return false, nil
	}
	delete(s.events, eventID)
	return true, nil
}

func (s *fakeParkedStore) LockTenantProvisioning(_ context.Context, tenantID uuid.UUID) error {
	s.locks = append(s.locks, tenantID)
	return nil
}

var _ port.ParkedTenantEventStore = (*fakeParkedStore)(nil)

type parkedTxRunner struct{ store *fakeParkedStore }

func (r *parkedTxRunner) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	snapshot := map[uuid.UUID]port.ParkedTenantEvent{}
	for k, v := range r.store.events {
		snapshot[k] = v
	}
	if err := fn(ctx); err != nil {
		r.store.events = snapshot
		return err
	}
	return nil
}

func parkingConsumer(repo port.TenantRepository, idemp *htIdempotency, store *fakeParkedStore) *MembershipEventConsumer {
	return NewMembershipEventConsumer(newFakeInbox(&htSyncTxRunner{}, idemp), repo, nil, nil, 5*time.Minute, nil).
		WithParkedEvents(store, &parkedTxRunner{store: store})
}

func parkEnv(t *testing.T, store *fakeParkedStore, tenantID uuid.UUID, eventType string, at time.Time) events.Envelope[json.RawMessage] {
	t.Helper()
	env := mkHandleEnv(eventType, tenantID.String())
	env.Timestamp = at
	env.Payload = json.RawMessage(`{"plan":"pro"}`)
	raw, err := json.Marshal(env)
	require.NoError(t, err)
	id := uuid.MustParse(env.ID)
	store.events[id] = port.ParkedTenantEvent{EventID: id, TenantID: tenantID, EventType: eventType, OccurredAt: at, Envelope: raw}
	return env
}

// ── parking ────────────────────────────────────────────────────────────────

func TestPark_NoRow_ParkedAndRecorded(t *testing.T) {
	tenantID := uuid.New()
	idemp := &htIdempotency{}
	store := newFakeParkedStore()
	c := parkingConsumer(&htTenantRepo{findInclDeletedFn: notFound}, idemp, store)

	env := mkHandleEnv("TenantConverted", tenantID.String())
	env.Payload = json.RawMessage(`{"plan":"enterprise","direct_signup":true}`)
	require.NoError(t, c.Handle(context.Background(), env), "parked, not retried")

	assert.Contains(t, idemp.marked, env.ID, "acknowledged: processed_events recorded")
	assert.Equal(t, []uuid.UUID{tenantID}, store.locks, "parks under the provisioning lock")
	require.Len(t, store.events, 1)
	parked := store.events[uuid.MustParse(env.ID)]
	assert.Equal(t, tenantID, parked.TenantID)
	assert.Equal(t, "TenantConverted", parked.EventType)
	assert.True(t, env.Timestamp.Equal(parked.OccurredAt))

	var back events.Envelope[json.RawMessage]
	require.NoError(t, json.Unmarshal(parked.Envelope, &back))
	assert.Equal(t, env.ID, back.ID)
	assert.JSONEq(t, string(env.Payload), string(back.Payload), "envelope stored as consumed")
}

// I-1 committed while the event waited for the provisioning lock: apply it
// now instead of parking.
func TestPark_RowAppearsUnderLock_AppliedNotParked(t *testing.T) {
	calls := 0
	applied := 0
	store := newFakeParkedStore()
	repo := &htTenantRepo{
		findInclDeletedFn: notFound,
		lockForProjectionFn: func(context.Context, uuid.UUID) (*port.TenantProjectionLock, error) {
			calls++
			if calls == 1 {
				return nil, nil
			}
			return &port.TenantProjectionLock{Status: domain.StatusTrial, Plan: domain.PlanPro}, nil
		},
		applyLifecyclePatchFn: func(context.Context, uuid.UUID, port.TenantLifecyclePatch) (int64, error) {
			applied++
			return 1, nil
		},
	}
	c := parkingConsumer(repo, &htIdempotency{}, store)

	env := mkHandleEnv("TenantConverted", uuid.New().String())
	env.Payload = json.RawMessage(`{"plan":"pro"}`)
	require.NoError(t, c.Handle(context.Background(), env))
	assert.Equal(t, 1, applied, "applied in the same transaction")
	assert.Empty(t, store.events, "nothing parked")
}

func TestPark_TrialReactivated_RowAppearsUnderLock_Redelivered(t *testing.T) {
	calls := 0
	repo := &htTenantRepo{
		findInclDeletedFn: notFound,
		lockForProjectionFn: func(context.Context, uuid.UUID) (*port.TenantProjectionLock, error) {
			calls++
			if calls == 1 {
				return nil, nil
			}
			return &port.TenantProjectionLock{Status: domain.StatusTrialExpired, Plan: domain.PlanPro}, nil
		},
	}
	idemp := &htIdempotency{}
	c := parkingConsumer(repo, idemp, newFakeParkedStore())

	err := c.Handle(context.Background(), mkHandleEnv("TrialReactivated", uuid.New().String()))
	require.ErrorIs(t, err, errTenantNotYetProvisioned, "redelivered so the trial duration is recomputed")
	assert.Empty(t, idemp.marked)
}

func TestPark_SoftDeletedTenant_DroppedNotParked(t *testing.T) {
	store := newFakeParkedStore()
	repo := &htTenantRepo{findInclDeletedFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
		return &domain.Tenant{Status: domain.StatusOffboarded}, nil
	}}
	idemp := &htIdempotency{}
	c := parkingConsumer(repo, idemp, store)

	env := mkHandleEnv("TenantPlanChanged", uuid.New().String())
	require.NoError(t, c.Handle(context.Background(), env))
	assert.Contains(t, idemp.marked, env.ID)
	assert.Empty(t, store.events)
	assert.Empty(t, store.locks)
}

func TestPark_StoreError_Retried(t *testing.T) {
	store := newFakeParkedStore()
	store.parkErr = errors.New("db down")
	idemp := &htIdempotency{}
	c := parkingConsumer(&htTenantRepo{findInclDeletedFn: notFound}, idemp, store)

	err := c.Handle(context.Background(), mkHandleEnv("TenantRealmReady", uuid.New().String()))
	require.ErrorIs(t, err, store.parkErr)
	assert.Empty(t, idemp.marked, "not recorded, so SQS redelivers")
}

// ── replay ─────────────────────────────────────────────────────────────────

func TestReplay_AppliesInOccurredOrderAndClaims(t *testing.T) {
	tenantID := uuid.New()
	store := newFakeParkedStore()
	base := time.Now().UTC().Add(-time.Minute)
	parkEnv(t, store, tenantID, "TenantConverted", base.Add(2*time.Second))
	parkEnv(t, store, tenantID, "TenantRealmReady", base)

	var order []port.TenantLifecycleOp
	var lastEventAt *time.Time
	repo := &htTenantRepo{
		lockForProjectionFn: func(context.Context, uuid.UUID) (*port.TenantProjectionLock, error) {
			return &port.TenantProjectionLock{Status: domain.StatusTrial, Plan: domain.PlanPro, LastEventAt: lastEventAt}, nil
		},
		setLastEventAtFn: func(_ context.Context, _ uuid.UUID, at time.Time) error {
			lastEventAt = &at
			return nil
		},
		applyLifecyclePatchFn: func(_ context.Context, _ uuid.UUID, patch port.TenantLifecyclePatch) (int64, error) {
			order = append(order, patch.Op)
			return 1, nil
		},
	}
	c := parkingConsumer(repo, &htIdempotency{}, store)

	n, err := c.ReplayParked(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, []port.TenantLifecycleOp{port.LifecycleSetRealm, port.LifecycleActivatePaid}, order,
		"realm first, then conversion — occurred_at order")
	assert.Empty(t, store.events, "both claimed")
}

func TestReplay_TenantStillMissing_StaysParked(t *testing.T) {
	tenantID := uuid.New()
	store := newFakeParkedStore()
	env := parkEnv(t, store, tenantID, "TenantConverted", time.Now().UTC())
	c := parkingConsumer(&htTenantRepo{findInclDeletedFn: notFound}, &htIdempotency{}, store)

	n, err := c.ReplayParkedSweep(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Contains(t, store.events, uuid.MustParse(env.ID), "claim rolled back")
}

func TestReplay_TenantOffboardedSinceParked_Dropped(t *testing.T) {
	tenantID := uuid.New()
	store := newFakeParkedStore()
	parkEnv(t, store, tenantID, "TenantPlanChanged", time.Now().UTC())
	applied := false
	repo := &htTenantRepo{
		findInclDeletedFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{Status: domain.StatusOffboarded}, nil
		},
		applyLifecyclePatchFn: func(context.Context, uuid.UUID, port.TenantLifecyclePatch) (int64, error) {
			applied = true
			return 1, nil
		},
	}
	c := parkingConsumer(repo, &htIdempotency{}, store)

	n, err := c.ReplayParked(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.False(t, applied)
	assert.Empty(t, store.events, "dropped, as live delivery would")
}

// A failure stops the replay so a later event never applies ahead of an
// earlier one; the failed event and everything after it stay parked.
func TestReplay_FailureStopsAndKeepsRest(t *testing.T) {
	tenantID := uuid.New()
	store := newFakeParkedStore()
	base := time.Now().UTC().Add(-time.Minute)
	first := parkEnv(t, store, tenantID, "TenantRealmReady", base)
	second := parkEnv(t, store, tenantID, "TenantConverted", base.Add(time.Second))
	boom := errors.New("apply failed")
	repo := &htTenantRepo{
		lockForProjectionFn: func(context.Context, uuid.UUID) (*port.TenantProjectionLock, error) {
			return &port.TenantProjectionLock{Status: domain.StatusTrial, Plan: domain.PlanPro}, nil
		},
		applyLifecyclePatchFn: func(context.Context, uuid.UUID, port.TenantLifecyclePatch) (int64, error) {
			return 0, boom
		},
	}
	c := parkingConsumer(repo, &htIdempotency{}, store)

	n, err := c.ReplayParked(context.Background(), tenantID)
	require.ErrorIs(t, err, boom)
	assert.Zero(t, n)
	assert.Contains(t, store.events, uuid.MustParse(first.ID))
	assert.Contains(t, store.events, uuid.MustParse(second.ID))
}

func TestReplay_ListError_Returned(t *testing.T) {
	store := newFakeParkedStore()
	store.listErr = errors.New("db down")
	c := parkingConsumer(&htTenantRepo{}, &htIdempotency{}, store)

	_, err := c.ReplayParked(context.Background(), uuid.New())
	require.ErrorIs(t, err, store.listErr)
}

func TestReplay_CorruptEnvelope_Returned(t *testing.T) {
	tenantID := uuid.New()
	store := newFakeParkedStore()
	id := uuid.New()
	store.events[id] = port.ParkedTenantEvent{EventID: id, TenantID: tenantID, EventType: "TenantConverted", Envelope: []byte("{not json")}
	c := parkingConsumer(&htTenantRepo{}, &htIdempotency{}, store)

	_, err := c.ReplayParked(context.Background(), tenantID)
	require.Error(t, err)
	assert.Contains(t, store.events, id)
}

func TestReplay_NotWired_NoOp(t *testing.T) {
	c := NewMembershipEventConsumer(nil, &htTenantRepo{}, nil, nil, 0, nil)
	n, err := c.ReplayParked(context.Background(), uuid.New())
	require.NoError(t, err)
	assert.Zero(t, n)
}
