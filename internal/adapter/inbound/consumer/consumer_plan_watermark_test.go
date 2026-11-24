package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pwTenantRepo records which EVT-14 watermark each event advances and the
// lifecycle patches it applies, against a fixed projection lock.
type pwTenantRepo struct {
	htTenantRepo
	lock       port.TenantProjectionLock
	patches    []port.TenantLifecyclePatch
	lastSet    []time.Time
	seatsSet   []time.Time
	planSet    []time.Time
	planSetErr error
	peekPlan   domain.TenantPlan // plan the pre-tx peek sees; defaults to lock.Plan
	pub        *htPublisher      // optional EVT-16 relay capture
}

func (r *pwTenantRepo) LockForProjection(context.Context, uuid.UUID) (*port.TenantProjectionLock, error) {
	l := r.lock
	return &l, nil
}

func (r *pwTenantRepo) ApplyLifecyclePatch(_ context.Context, _ uuid.UUID, p port.TenantLifecyclePatch) (int64, error) {
	r.patches = append(r.patches, p)
	return 1, nil
}

func (r *pwTenantRepo) SetLastEventAt(_ context.Context, _ uuid.UUID, t time.Time) error {
	r.lastSet = append(r.lastSet, t)
	return nil
}

func (r *pwTenantRepo) SetSeatsLastEventAt(_ context.Context, _ uuid.UUID, t time.Time) error {
	r.seatsSet = append(r.seatsSet, t)
	return nil
}

func (r *pwTenantRepo) SetPlanLastEventAt(_ context.Context, _ uuid.UUID, t time.Time) error {
	if r.planSetErr != nil {
		return r.planSetErr
	}
	r.planSet = append(r.planSet, t)
	return nil
}

func pwHandle(t *testing.T, repo *pwTenantRepo, eventType string, at time.Time, payload any) error {
	t.Helper()
	b, err := json.Marshal(payload)
	require.NoError(t, err)
	runner := &htSyncTxRunner{}
	if repo.pub != nil {
		runner.publisher = repo.pub
	}
	c := NewMembershipEventConsumer(newFakeInbox(runner, &htIdempotency{}), repo, &trNoopCatalog{}, nil, 5*time.Minute, nil)
	repo.findByIDFn = func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
		plan := repo.lock.Plan
		if repo.peekPlan != "" {
			plan = repo.peekPlan
		}
		return &domain.Tenant{ID: id, Plan: plan}, nil
	}
	return c.Handle(context.Background(), events.Envelope[json.RawMessage]{
		ID: uuid.New().String(), Type: eventType, TenantID: uuid.New().String(),
		Timestamp: at, Payload: b,
	})
}

func opsOf(patches []port.TenantLifecyclePatch) []port.TenantLifecycleOp {
	out := make([]port.TenantLifecycleOp, len(patches))
	for i, p := range patches {
		out[i] = p.Op
	}
	return out
}

// The rev 2.62 bug: Billing's TenantPlanChanged (stamped T2) delivered before
// RP's revert TrialReactivated (stamped T1 < T2). On the shared watermark the
// revert was skipped as stale. Now the plan change only advances
// plan_last_event_at, so the revert still applies.
func TestPlanWatermark_PlanChangeDoesNotMakeRevertStale(t *testing.T) {
	now := time.Now().UTC()
	t0, t1, t2 := now.Add(-3*time.Minute), now.Add(-2*time.Minute), now.Add(-time.Minute)
	repo := &pwTenantRepo{lock: port.TenantProjectionLock{Status: domain.StatusActive, Plan: domain.PlanPro, LastEventAt: &t0}}

	require.NoError(t, pwHandle(t, repo, "TenantPlanChanged", t2, map[string]string{"plan": "starter"}))
	assert.Equal(t, []time.Time{t2}, repo.planSet, "TenantPlanChanged advances only the plan watermark")
	assert.Empty(t, repo.lastSet)

	// What the row now holds: plan watermark T2, lifecycle watermark still T0.
	repo.lock.PlanLastEventAt, repo.lock.Plan, repo.patches = &t2, domain.PlanStarter, nil
	require.NoError(t, pwHandle(t, repo, "TrialReactivated", t1, map[string]string{"reason": "conversion_revert"}))
	assert.Equal(t, []port.TenantLifecycleOp{port.LifecycleSetRealm, port.LifecycleRevertConversion}, opsOf(repo.patches),
		"the earlier-stamped revert is not stale against the plan watermark")
	assert.Equal(t, []time.Time{t1}, repo.lastSet)
}

// TenantPlanChanged is guarded by plan_last_event_at, not last_event_at.
func TestPlanWatermark_PlanChangeGuardedByPlanWatermark(t *testing.T) {
	now := time.Now().UTC()
	planWM, lifeWM := now.Add(-time.Minute), now.Add(time.Minute)

	t.Run("newer than plan watermark, older than lifecycle: applied", func(t *testing.T) {
		repo := &pwTenantRepo{lock: port.TenantProjectionLock{Status: domain.StatusActive, Plan: domain.PlanPro, LastEventAt: &lifeWM, PlanLastEventAt: &planWM}}
		require.NoError(t, pwHandle(t, repo, "TenantPlanChanged", now, map[string]string{"plan": "starter"}))
		require.Len(t, repo.patches, 1)
		assert.Equal(t, port.TenantLifecyclePatch{Op: port.LifecycleSetPlan, Plan: domain.PlanStarter}, repo.patches[0])
	})
	t.Run("not newer than plan watermark: skipped", func(t *testing.T) {
		repo := &pwTenantRepo{lock: port.TenantProjectionLock{Status: domain.StatusActive, Plan: domain.PlanPro, PlanLastEventAt: &planWM}}
		require.NoError(t, pwHandle(t, repo, "TenantPlanChanged", planWM, map[string]string{"plan": "starter"}))
		assert.Empty(t, repo.patches)
		assert.Empty(t, repo.planSet)
	})
}

// TenantConverted is ordered by last_event_at, but its plan yields to a newer
// TenantPlanChanged and it advances the plan watermark only when it applies
// its plan.
func TestPlanWatermark_ConvertedPlanYieldsToNewerPlanChange(t *testing.T) {
	now := time.Now().UTC()
	older, newer := now.Add(-time.Minute), now.Add(time.Minute)

	t.Run("plan watermark newer: status applied, plan kept", func(t *testing.T) {
		repo := &pwTenantRepo{lock: port.TenantProjectionLock{Status: domain.StatusTrial, Plan: domain.PlanStarter, PlanLastEventAt: &newer}}
		require.NoError(t, pwHandle(t, repo, "TenantConverted", now, map[string]string{"plan": "pro"}))
		require.Len(t, repo.patches, 1)
		assert.Equal(t, port.TenantLifecyclePatch{Op: port.LifecycleActivatePaid, Plan: domain.PlanStarter}, repo.patches[0])
		assert.Equal(t, []time.Time{now}, repo.lastSet)
		assert.Empty(t, repo.planSet, "a stale plan does not advance the plan watermark")
	})
	for _, typ := range []string{"TenantConverted", "DirectPaidSignup"} {
		t.Run(typ+": plan watermark older: plan applied and watermark advanced", func(t *testing.T) {
			repo := &pwTenantRepo{lock: port.TenantProjectionLock{Status: domain.StatusTrial, Plan: domain.PlanStarter, PlanLastEventAt: &older}}
			require.NoError(t, pwHandle(t, repo, typ, now, map[string]string{"plan": "pro"}))
			require.Len(t, repo.patches, 1)
			assert.Equal(t, domain.PlanPro, repo.patches[0].Plan)
			assert.Equal(t, []time.Time{now}, repo.lastSet)
			assert.Equal(t, []time.Time{now}, repo.planSet)
		})
	}
}

// Other lifecycle events never touch the plan watermark.
func TestPlanWatermark_OtherLifecycleEventsLeavePlanWatermark(t *testing.T) {
	repo := &pwTenantRepo{lock: port.TenantProjectionLock{Status: domain.StatusTrial, Plan: domain.PlanStarter}}
	now := time.Now().UTC()
	require.NoError(t, pwHandle(t, repo, "TrialExpired", now, struct{}{}))
	assert.Equal(t, []time.Time{now}, repo.lastSet)
	assert.Empty(t, repo.planSet)
	assert.Empty(t, repo.seatsSet)
}

func TestPlanWatermark_SetErrorsPropagate(t *testing.T) {
	setErr := errors.New("db down")
	now := time.Now().UTC()
	for _, typ := range []string{"TenantPlanChanged", "TenantConverted"} {
		t.Run(typ, func(t *testing.T) {
			repo := &pwTenantRepo{lock: port.TenantProjectionLock{Status: domain.StatusTrial, Plan: domain.PlanStarter}, planSetErr: setErr}
			err := pwHandle(t, repo, typ, now, map[string]string{"plan": "pro"})
			assert.ErrorIs(t, err, setErr)
		})
	}
}

// Review finding 2: a TenantConverted without a plan keeps the current plan
// and must not advance the plan watermark, or an earlier real
// TenantPlanChanged delivered after it would be skipped.
func TestPlanWatermark_PlanlessConversionLeavesPlanWatermark(t *testing.T) {
	for name, payload := range map[string]any{
		"no plan key": struct{}{},
		"empty plan":  map[string]string{"plan": ""},
	} {
		t.Run(name, func(t *testing.T) {
			repo := &pwTenantRepo{lock: port.TenantProjectionLock{Status: domain.StatusTrial, Plan: domain.PlanStarter}}
			now := time.Now().UTC()
			require.NoError(t, pwHandle(t, repo, "TenantConverted", now, payload))
			require.Len(t, repo.patches, 1)
			assert.Equal(t, domain.PlanStarter, repo.patches[0].Plan)
			assert.Equal(t, []time.Time{now}, repo.lastSet)
			assert.Empty(t, repo.planSet)
		})
	}
}

// Review finding 3: the trial length was resolved for the plan peeked before
// the transaction. If a TenantPlanChanged landed in between, the event is
// redelivered rather than applying the old plan's trial length.
func TestPlanWatermark_TrialReactivatedRetriesWhenPlanChangedSincePeek(t *testing.T) {
	repo := &pwTenantRepo{
		lock:     port.TenantProjectionLock{Status: domain.StatusActive, Plan: domain.PlanStarter},
		peekPlan: domain.PlanPro,
	}
	err := pwHandle(t, repo, "TrialReactivated", time.Now().UTC(), map[string]string{"reason": "conversion_revert"})
	require.ErrorIs(t, err, errPlanChangedSincePeek)
	assert.Empty(t, repo.patches, "nothing applied; the transaction rolls back and the event is redelivered")
	assert.Empty(t, repo.lastSet)
}

// Review finding 1: with status and plan on separate watermarks, a relay's
// changed_at is the newest event time applied to either, so relays stay
// monotonic. Billing's plan change (T2) relays first; RP's earlier-stamped
// revert (T1) then relays with changed_at T2, not T1.
func TestPlanWatermark_RelayChangedAtStaysMonotonic(t *testing.T) {
	now := time.Now().UTC()
	t1, t2 := now.Add(-2*time.Minute), now.Add(-time.Minute)
	pub := &htPublisher{}
	repo := &pwTenantRepo{
		lock: port.TenantProjectionLock{Status: domain.StatusActive, Plan: domain.PlanStarter, PlanLastEventAt: &t2},
		pub:  pub,
	}
	require.NoError(t, pwHandle(t, repo, "TrialReactivated", t1, map[string]string{"reason": "conversion_revert"}))
	require.Len(t, pub.events, 1)
	payload, ok := pub.events[0].Data.(domain.TenantStateChangedPayload)
	require.True(t, ok)
	assert.Equal(t, domain.StatusTrial, payload.Status)
	assert.Equal(t, t2, payload.ChangedAt, "changed_at must not go backwards behind the applied plan change")

	t.Run("no newer watermark: changed_at is the event time", func(t *testing.T) {
		pub := &htPublisher{}
		repo := &pwTenantRepo{lock: port.TenantProjectionLock{Status: domain.StatusTrial, Plan: domain.PlanStarter}, pub: pub}
		require.NoError(t, pwHandle(t, repo, "TrialExpired", now, struct{}{}))
		require.Len(t, pub.events, 1)
		assert.Equal(t, now, pub.events[0].Data.(domain.TenantStateChangedPayload).ChangedAt)
	})
}

// ── rev 2.63: new_trial_ends_at is authoritative ────────────────────────

func TestPayloadTrialEndsAt(t *testing.T) {
	want := time.Date(2026, 12, 9, 10, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		payload string
		want    *time.Time
		ok      bool
	}{
		"empty payload": {``, nil, true},
		"absent":        {`{"reason":"conversion_revert"}`, nil, true},
		"null":          {`{"new_trial_ends_at":null}`, nil, true},
		"empty string":  {`{"new_trial_ends_at":""}`, nil, true},
		"valid UTC":     {`{"new_trial_ends_at":"2026-12-09T10:00:00Z"}`, &want, true},
		"valid offset":  {`{"new_trial_ends_at":"2026-12-09T14:00:00+04:00"}`, &want, true},
		"fractional":    {`{"new_trial_ends_at":"2026-12-09T10:00:00.000Z"}`, &want, true},
		"malformed":     {`{"new_trial_ends_at":"next tuesday"}`, nil, false},
		"date only":     {`{"new_trial_ends_at":"2026-12-09"}`, nil, false},
		"zero time":     {`{"new_trial_ends_at":"0001-01-01T00:00:00Z"}`, nil, false},
		"not a string":  {`{"new_trial_ends_at":1765274400}`, nil, false},
		"undecodable":   {`not json`, nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := payloadTrialEndsAt(json.RawMessage(tc.payload))
			assert.Equal(t, tc.ok, ok)
			if tc.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.True(t, tc.want.Equal(*got))
			assert.Equal(t, time.UTC, got.Location())
		})
	}
}

// A valid new_trial_ends_at is used as is: no plan peek, no Catalog call
// (the failing catalog would surface an error), and no plan-changed retry.
func TestTrialEndsAt_PayloadDateWinsWithoutCatalog(t *testing.T) {
	endsAt := time.Date(2026, 12, 9, 10, 0, 0, 0, time.UTC)
	for _, reason := range []string{"conversion_revert", "trial_reactivation"} {
		t.Run(reason, func(t *testing.T) {
			repo := &pwTenantRepo{
				lock:     port.TenantProjectionLock{Status: domain.StatusActive, Plan: domain.PlanStarter},
				peekPlan: domain.PlanPro, // would trigger errPlanChangedSincePeek if peeked
			}
			c := NewMembershipEventConsumer(newFakeInbox(&htSyncTxRunner{}, &htIdempotency{}), repo,
				&trErrCatalog{err: errors.New("catalog must not be called")}, nil, 5*time.Minute, nil)
			repo.findByIDFn = func(context.Context, uuid.UUID) (*domain.Tenant, error) {
				return nil, errors.New("plan must not be peeked")
			}
			b, _ := json.Marshal(map[string]string{"reason": reason, "new_trial_ends_at": endsAt.Format(time.RFC3339)})
			require.NoError(t, c.Handle(context.Background(), events.Envelope[json.RawMessage]{
				ID: uuid.New().String(), Type: "TrialReactivated", TenantID: uuid.New().String(),
				Timestamp: time.Now().UTC(), Payload: b,
			}))
			require.Len(t, repo.patches, 2)
			require.NotNil(t, repo.patches[1].TrialEndsAt)
			assert.True(t, endsAt.Equal(*repo.patches[1].TrialEndsAt))
			assert.Zero(t, repo.patches[1].TrialDurationDays)
		})
	}
}

// Absent or malformed new_trial_ends_at falls back to the plan's
// trial_duration_days (14 from trNoopCatalog).
func TestTrialEndsAt_AbsentOrMalformedFallsBackToPlan(t *testing.T) {
	for name, payload := range map[string]map[string]string{
		"absent":    {"reason": "conversion_revert"},
		"malformed": {"reason": "conversion_revert", "new_trial_ends_at": "soon"},
	} {
		t.Run(name, func(t *testing.T) {
			repo := &pwTenantRepo{lock: port.TenantProjectionLock{Status: domain.StatusActive, Plan: domain.PlanStarter}}
			require.NoError(t, pwHandle(t, repo, "TrialReactivated", time.Now().UTC(), payload))
			require.Len(t, repo.patches, 2)
			assert.Nil(t, repo.patches[1].TrialEndsAt)
			assert.Equal(t, 14, repo.patches[1].TrialDurationDays)
		})
	}
}

// ── rev 2.64: SHARED_TRIAL_REALM ────────────────────────────────────────

// TrialReactivated resets the tenant to the configured shared trial realm,
// or "trial" when unset; the shard stays shard-0.
func TestTrialReactivated_ResetsToConfiguredSharedTrialRealm(t *testing.T) {
	for name, tc := range map[string]struct{ configured, want string }{
		"unset uses default": {"", "trial"},
		"configured":         {"trial-eu", "trial-eu"},
	} {
		t.Run(name, func(t *testing.T) {
			tenants := &recordingTenants{}
			c := NewMembershipEventConsumer(newFakeInbox(nil, nil), tenants, nil, nil, 300*time.Second, nil).
				WithSharedTrialRealm(tc.configured)
			_, _, err := applyOn(c, mkEnv("TrialReactivated", map[string]string{"reason": "conversion_revert"}),
				domain.StatusActive, domain.PlanStarter)
			require.NoError(t, err)
			require.NotEmpty(t, tenants.patches)
			assert.Equal(t, port.TenantLifecyclePatch{
				Op:            port.LifecycleSetRealm,
				RealmID:       tc.want,
				RealmType:     string(domain.RealmShared),
				KeycloakShard: domain.SharedTrialKeycloakShard,
			}, tenants.patches[0])
		})
	}
}
