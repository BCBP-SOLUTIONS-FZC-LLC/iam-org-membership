// consumer_handle_tx_test.go covers the Handle() branches that require a
// TxRunner to be called. These include:
//
//   - kindUnknown path: dedup write via RunInTx when event type is unrecognised
//   - already-seen dedup: IsProcessed=true → early nil return (no RunInTx)
//   - nil-locked tenant: LockForProjection returns (nil, nil) → MarkProcessed + drop
//   - EVT-14 stale guard: last_event_at ≥ event timestamp → MarkProcessed + drop
//   - happy-path chain: LockForProjection + applyProjection + MarkProcessed
//   - GDPR wipe path: TenantOffboarded transition → WipeTenantChildren + cache eviction
//     (gdprWipeCacheKeys is 0% in the function coverage report)
//   - EVT-16 relay: status/plan change fires TenantStateChanged enqueue
//   - TenantMembershipsPurged relay: offboard transition fires purge enqueue
package consumer

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── shared stubs ─────────────────────────────────────────────────────────────

// htSyncTxRunner is a simple pass-through TxRunner for Handle unit tests.
// It calls fn with the context, optionally injecting a publisher first.
type htSyncTxRunner struct {
	publisher port.EventPublisher // may be nil
}

func (r *htSyncTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if r.publisher != nil {
		ctx = port.WithEventPublisher(ctx, r.publisher)
	}
	return fn(ctx)
}

// htIdempotency is a configurable IdempotencyStore for Handle unit tests.
type htIdempotency struct {
	isProcessedFn   func(ctx context.Context, consumer, eventID string) (bool, error)
	markProcessedFn func(ctx context.Context, consumer, eventID string) error
	marked          []string // records eventIDs passed to MarkProcessed
}

func (s *htIdempotency) IsProcessed(ctx context.Context, consumer, eventID string) (bool, error) {
	if s.isProcessedFn != nil {
		return s.isProcessedFn(ctx, consumer, eventID)
	}
	return false, nil
}

func (s *htIdempotency) MarkProcessed(ctx context.Context, consumer, eventID string) error {
	s.marked = append(s.marked, eventID)
	if s.markProcessedFn != nil {
		return s.markProcessedFn(ctx, consumer, eventID)
	}
	return nil
}

var _ port.IdempotencyStore = (*htIdempotency)(nil)

// htTenantRepo extends TenantRepositoryNoop with configurable fns for the
// methods Handle() calls.
type htTenantRepo struct {
	port.TenantRepositoryNoop
	lockForProjectionFn   func(ctx context.Context, id uuid.UUID) (*port.TenantProjectionLock, error)
	setLastEventAtFn      func(ctx context.Context, id uuid.UUID, t time.Time) error
	applyLifecyclePatchFn func(ctx context.Context, id uuid.UUID, patch port.TenantLifecyclePatch) (int64, error)
	wipeTenantChildrenFn  func(ctx context.Context, id uuid.UUID) error
	findByIDFn            func(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)
}

func (r *htTenantRepo) LockForProjection(ctx context.Context, id uuid.UUID) (*port.TenantProjectionLock, error) {
	if r.lockForProjectionFn != nil {
		return r.lockForProjectionFn(ctx, id)
	}
	return nil, nil
}

func (r *htTenantRepo) SetLastEventAt(ctx context.Context, id uuid.UUID, t time.Time) error {
	if r.setLastEventAtFn != nil {
		return r.setLastEventAtFn(ctx, id, t)
	}
	return nil
}

func (r *htTenantRepo) ApplyLifecyclePatch(ctx context.Context, id uuid.UUID, patch port.TenantLifecyclePatch) (int64, error) {
	if r.applyLifecyclePatchFn != nil {
		return r.applyLifecyclePatchFn(ctx, id, patch)
	}
	return 1, nil
}

func (r *htTenantRepo) WipeTenantChildren(ctx context.Context, id uuid.UUID) error {
	if r.wipeTenantChildrenFn != nil {
		return r.wipeTenantChildrenFn(ctx, id)
	}
	return nil
}

func (r *htTenantRepo) FindByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	if r.findByIDFn != nil {
		return r.findByIDFn(ctx, id)
	}
	return nil, domain.NewError(domain.ErrTenantNotFound, "not found")
}

var _ port.TenantRepository = (*htTenantRepo)(nil)

// htCache is a spy port.Cache that records Delete calls.
type htCache struct {
	deletedKeys []string
}

func (c *htCache) Get(_ context.Context, _ string) ([]byte, error) { return nil, nil }
func (c *htCache) MGet(_ context.Context, keys []string) ([][]byte, error) {
	return make([][]byte, len(keys)), nil
}
func (c *htCache) Set(_ context.Context, _ string, _ []byte, _ time.Duration) error { return nil }
func (c *htCache) SetNX(_ context.Context, _ string, _ []byte, _ time.Duration) (bool, error) {
	return true, nil
}
func (c *htCache) Delete(_ context.Context, keys ...string) error {
	c.deletedKeys = append(c.deletedKeys, keys...)
	return nil
}
func (c *htCache) Health(_ context.Context) error { return nil }
func (c *htCache) Close() error                   { return nil }

var _ port.Cache = (*htCache)(nil)

// htPublisher is a spy EventPublisher that records Enqueue calls.
type htPublisher struct {
	events []*domain.DomainEvent
}

func (p *htPublisher) Enqueue(_ context.Context, event *domain.DomainEvent) error {
	p.events = append(p.events, event)
	return nil
}

var _ port.EventPublisher = (*htPublisher)(nil)

// mkHandleEnv builds an Envelope for Handle() tests with now() timestamp.
func mkHandleEnv(eventType string, tenantID string) events.Envelope[json.RawMessage] {
	return events.Envelope[json.RawMessage]{
		ID:        uuid.New().String(),
		Type:      eventType,
		TenantID:  tenantID,
		Timestamp: time.Now().UTC().Add(-1 * time.Second), // slightly in the past, never EVT-15
	}
}

// ── Test cases ────────────────────────────────────────────────────────────────

// TestHandle_KindUnknown_DedupsViaRunInTx verifies that an unrecognised event
// type silently records a dedup entry (MarkProcessed) inside RunInTx and
// returns nil — forward-compat branch, line 107-116.
func TestHandle_KindUnknown_DedupsViaRunInTx(t *testing.T) {
	idemp := &htIdempotency{}
	txRunner := &htSyncTxRunner{}
	c := NewMembershipEventConsumer(txRunner, nil, idemp, nil, nil, 5*time.Minute, nil)

	env := mkHandleEnv("CompletelyUnknownEventType", uuid.New().String())
	err := c.Handle(context.Background(), env)

	require.NoError(t, err)
	assert.Contains(t, idemp.marked, env.ID,
		"MarkProcessed must be called for unknown event types so SQS acks cleanly")
}

// TestHandle_AlreadySeen_ReturnsNilWithoutRunInTx verifies the cheap pre-tx
// dedup probe (line 123-130): when IsProcessed returns true, Handle returns
// nil immediately without acquiring the DB row lock.
func TestHandle_AlreadySeen_ReturnsNilWithoutRunInTx(t *testing.T) {
	var runInTxCalled bool

	idemp := &htIdempotency{
		isProcessedFn: func(_ context.Context, _, _ string) (bool, error) {
			return true, nil // already seen
		},
	}

	// Use a known event type (TrialExpired → kindTenantLifecycle) so the code
	// reaches the IsProcessed probe; unknown types bypass it entirely.
	c := NewMembershipEventConsumer(&spyRunnerDetect{called: &runInTxCalled}, nil, idemp, nil, nil, 5*time.Minute, nil)

	env := mkHandleEnv("TrialExpired", uuid.New().String())
	err := c.Handle(context.Background(), env)

	require.NoError(t, err)
	assert.False(t, runInTxCalled, "RunInTx must NOT be called when event is already-seen")
}

// spyRunnerDetect wraps a syncTxRunner but sets *called on each RunInTx.
type spyRunnerDetect struct {
	called *bool
}

func (r *spyRunnerDetect) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	*r.called = true
	return fn(ctx)
}

// TestHandle_NilLockedTenant_DropsSilentlyWithDedup verifies the
// "tenant absent" branch (line 174-178): when LockForProjection returns
// (nil, nil), Handle records MarkProcessed and returns nil (silent drop).
func TestHandle_NilLockedTenant_DropsSilentlyWithDedup(t *testing.T) {
	idemp := &htIdempotency{}
	tenants := &htTenantRepo{
		lockForProjectionFn: func(_ context.Context, _ uuid.UUID) (*port.TenantProjectionLock, error) {
			return nil, nil // tenant absent
		},
	}
	c := NewMembershipEventConsumer(&htSyncTxRunner{}, tenants, idemp, nil, nil, 5*time.Minute, nil)

	env := mkHandleEnv("TrialExpired", uuid.New().String()) // known type → reaches LockForProjection
	err := c.Handle(context.Background(), env)

	require.NoError(t, err)
	assert.Contains(t, idemp.marked, env.ID,
		"absent-tenant drop must still call MarkProcessed so the event is not replayed")
}

// TestHandle_EVT14_StaleEvent_DropsSilentlyWithDedup verifies the recency
// guard (line 181-187): when locked.LastEventAt ≥ env.Timestamp the event is
// stale, no projection runs, but MarkProcessed is called so it won't replay.
func TestHandle_EVT14_StaleEvent_DropsSilentlyWithDedup(t *testing.T) {
	idemp := &htIdempotency{}
	pastTime := time.Now().UTC().Add(-5 * time.Minute)
	eventTime := time.Now().UTC().Add(-10 * time.Minute) // before lastEventAt → stale

	tenants := &htTenantRepo{
		lockForProjectionFn: func(_ context.Context, _ uuid.UUID) (*port.TenantProjectionLock, error) {
			return &port.TenantProjectionLock{
				Status:      domain.StatusActive,
				Plan:        domain.PlanStarter,
				LastEventAt: &pastTime, // event must be after this to be fresh
			}, nil
		},
	}
	c := NewMembershipEventConsumer(&htSyncTxRunner{}, tenants, idemp, nil, nil, 5*time.Minute, nil)

	env := events.Envelope[json.RawMessage]{
		ID:        uuid.New().String(),
		Type:      "TrialExpired", // known type → reaches LockForProjection
		TenantID:  uuid.New().String(),
		Timestamp: eventTime, // stale: not after pastTime
	}
	err := c.Handle(context.Background(), env)

	require.NoError(t, err)
	assert.Contains(t, idemp.marked, env.ID,
		"EVT-14 stale drop must still call MarkProcessed")
}

// TestHandle_GDPRWipe_WipesTenantChildrenAndInvalidatesCache verifies the full
// offboard path (lines 241-289): when the projection transitions a tenant from
// active → offboarded, Handle calls WipeTenantChildren and then evicts the
// bounded set of tenant-scoped cache keys via gdprWipeCacheKeys.
//
// This test covers gdprWipeCacheKeys (0% in function coverage) indirectly —
// the function is called only when gdprWipeRan=true after the tx commits.
func TestHandle_GDPRWipe_WipesTenantChildrenAndInvalidatesCache(t *testing.T) {
	tenantID := uuid.New()
	idemp := &htIdempotency{}
	cache := &htCache{}
	wipeCalled := false

	tenants := &htTenantRepo{
		lockForProjectionFn: func(_ context.Context, id uuid.UUID) (*port.TenantProjectionLock, error) {
			return &port.TenantProjectionLock{
				Status: domain.StatusActive, // not yet offboarded
				Plan:   domain.PlanStarter,
			}, nil
		},
		applyLifecyclePatchFn: func(_ context.Context, _ uuid.UUID, patch port.TenantLifecyclePatch) (int64, error) {
			return 1, nil // UPDATE ran — LifecycleOffboard transitions to StatusOffboarded
		},
		wipeTenantChildrenFn: func(_ context.Context, _ uuid.UUID) error {
			wipeCalled = true
			return nil
		},
	}

	c := NewMembershipEventConsumer(&htSyncTxRunner{}, tenants, idemp, nil, cache, 5*time.Minute, nil)

	env := events.Envelope[json.RawMessage]{
		ID:        uuid.New().String(),
		Type:      "TenantOffboarded",
		TenantID:  tenantID.String(),
		Timestamp: time.Now().UTC().Add(-1 * time.Second),
	}

	err := c.Handle(context.Background(), env)
	require.NoError(t, err)

	assert.True(t, wipeCalled, "WipeTenantChildren must be called when tenant transitions to offboarded")
	assert.Contains(t, idemp.marked, env.ID, "MarkProcessed must be called after the GDPR wipe tx")

	// Verify gdprWipeCacheKeys produced the expected bounded set of keys.
	expectedKeys := []string{
		"om:tenant:" + tenantID.String(),
		"om:locale:" + tenantID.String(),
		"om:roles:" + tenantID.String(),
		"om:seat_usage:" + tenantID.String(),
		"om:members:" + tenantID.String() + ":50",
		"om:grm:" + tenantID.String(),
		"om:grm:stale:" + tenantID.String(),
		"om:gdm:" + tenantID.String(),
		"om:gdm:stale:" + tenantID.String(),
		"om:gtrm:" + tenantID.String(),
		"om:gtrm:stale:" + tenantID.String(),
	}
	for _, k := range expectedKeys {
		assert.Contains(t, cache.deletedKeys, k,
			"gdprWipeCacheKeys must include %q in the cache eviction set", k)
	}
	assert.Len(t, cache.deletedKeys, 11, "gdprWipeCacheKeys must return exactly 11 keys")
}

// TestHandle_GDPRWipe_NilCache_SkipsCacheInvalidation verifies that when
// cache is nil, the wipe still completes without panic and returns nil.
func TestHandle_GDPRWipe_NilCache_SkipsCacheInvalidation(t *testing.T) {
	tenantID := uuid.New()
	idemp := &htIdempotency{}

	tenants := &htTenantRepo{
		lockForProjectionFn: func(_ context.Context, _ uuid.UUID) (*port.TenantProjectionLock, error) {
			return &port.TenantProjectionLock{Status: domain.StatusActive, Plan: domain.PlanStarter}, nil
		},
		applyLifecyclePatchFn: func(_ context.Context, _ uuid.UUID, _ port.TenantLifecyclePatch) (int64, error) {
			return 1, nil
		},
	}

	// nil cache — the conditional `if gdprWipeRan && c.cache != nil` must guard cleanly.
	c := NewMembershipEventConsumer(&htSyncTxRunner{}, tenants, idemp, nil, nil, 5*time.Minute, nil)

	env := events.Envelope[json.RawMessage]{
		ID:        uuid.New().String(),
		Type:      "TenantOffboarded",
		TenantID:  tenantID.String(),
		Timestamp: time.Now().UTC().Add(-1 * time.Second),
	}

	err := c.Handle(context.Background(), env)
	require.NoError(t, err, "nil cache must not cause a panic or error during GDPR wipe")
}

// TestHandle_EVT16_StatusChange_EnqueuesTenantStateChanged verifies that when
// a consumed event genuinely changes tenants.status, Handle enqueues a
// TenantStateChanged relay event in the same transaction (EVT-16 relay).
func TestHandle_EVT16_StatusChange_EnqueuesTenantStateChanged(t *testing.T) {
	tenantID := uuid.New()
	idemp := &htIdempotency{}
	pub := &htPublisher{}

	tenants := &htTenantRepo{
		lockForProjectionFn: func(_ context.Context, _ uuid.UUID) (*port.TenantProjectionLock, error) {
			return &port.TenantProjectionLock{
				Status: domain.StatusTrial, // will change to active
				Plan:   domain.PlanStarter,
			}, nil
		},
		applyLifecyclePatchFn: func(_ context.Context, _ uuid.UUID, _ port.TenantLifecyclePatch) (int64, error) {
			return 1, nil // UPDATE ran → newStatus = StatusActive
		},
	}

	txRunner := &htSyncTxRunner{publisher: pub}
	c := NewMembershipEventConsumer(txRunner, tenants, idemp, nil, nil, 5*time.Minute, nil)

	// "TenantSuspended" transitions trial→suspended (a genuine status change).
	// Include source payload so the JSON unmarshal in applyProjection succeeds.
	env := events.Envelope[json.RawMessage]{
		ID:        uuid.New().String(),
		Type:      "TenantSuspended",
		TenantID:  tenantID.String(),
		Timestamp: time.Now().UTC().Add(-1 * time.Second),
		Payload:   json.RawMessage(`{"source":"billing_lapse"}`),
	}

	err := c.Handle(context.Background(), env)
	require.NoError(t, err)

	// EVT-16 relay: at least one TenantStateChanged must have been enqueued.
	var found bool
	for _, e := range pub.events {
		if e.Type == domain.EventTenantStateChanged {
			found = true
			break
		}
	}
	assert.True(t, found, "EVT-16: TenantStateChanged must be enqueued when status changes trial→suspended")
}

// TestHandle_OffboardWithPub_EnqueuesTenantMembershipsPurged verifies that
// the offboard path fires both TenantStateChanged (EVT-16) and
// TenantMembershipsPurged (ADR-0008 §6.4 cascade signal) when a publisher is
// present in the transaction context.
func TestHandle_OffboardWithPub_EnqueuesTenantMembershipsPurged(t *testing.T) {
	tenantID := uuid.New()
	idemp := &htIdempotency{}
	pub := &htPublisher{}

	tenants := &htTenantRepo{
		lockForProjectionFn: func(_ context.Context, _ uuid.UUID) (*port.TenantProjectionLock, error) {
			return &port.TenantProjectionLock{Status: domain.StatusActive, Plan: domain.PlanStarter}, nil
		},
		applyLifecyclePatchFn: func(_ context.Context, _ uuid.UUID, _ port.TenantLifecyclePatch) (int64, error) {
			return 1, nil // transitions to StatusOffboarded
		},
	}

	txRunner := &htSyncTxRunner{publisher: pub}
	c := NewMembershipEventConsumer(txRunner, tenants, idemp, nil, nil, 5*time.Minute, nil)

	env := events.Envelope[json.RawMessage]{
		ID:        uuid.New().String(),
		Type:      "TenantOffboarded",
		TenantID:  tenantID.String(),
		Timestamp: time.Now().UTC().Add(-1 * time.Second),
	}

	err := c.Handle(context.Background(), env)
	require.NoError(t, err)

	eventTypes := make([]string, len(pub.events))
	for i, e := range pub.events {
		eventTypes[i] = e.Type
	}

	assert.Contains(t, eventTypes, domain.EventTenantStateChanged,
		"offboard must emit TenantStateChanged (EVT-16)")
	assert.Contains(t, eventTypes, domain.EventTenantMembershipsPurged,
		"offboard must emit TenantMembershipsPurged (ADR-0008 cascade signal)")
}

// TestHandle_HappyPath_MarkProcessedCalled verifies the nominal path for a
// non-offboard event: applyProjection runs, SetLastEventAt is called,
// and MarkProcessed is called to commit the dedup record.
func TestHandle_HappyPath_MarkProcessedCalled(t *testing.T) {
	tenantID := uuid.New()
	idemp := &htIdempotency{}
	lastEventAtCalled := false

	tenants := &htTenantRepo{
		lockForProjectionFn: func(_ context.Context, _ uuid.UUID) (*port.TenantProjectionLock, error) {
			return &port.TenantProjectionLock{Status: domain.StatusTrial, Plan: domain.PlanStarter}, nil
		},
		applyLifecyclePatchFn: func(_ context.Context, _ uuid.UUID, _ port.TenantLifecyclePatch) (int64, error) {
			return 1, nil
		},
		setLastEventAtFn: func(_ context.Context, _ uuid.UUID, _ time.Time) error {
			lastEventAtCalled = true
			return nil
		},
	}

	c := NewMembershipEventConsumer(&htSyncTxRunner{}, tenants, idemp, nil, nil, 5*time.Minute, nil)

	// "TrialExpired" is a known event type with no required payload.
	env := events.Envelope[json.RawMessage]{
		ID:        uuid.New().String(),
		Type:      "TrialExpired",
		TenantID:  tenantID.String(),
		Timestamp: time.Now().UTC().Add(-1 * time.Second),
	}

	err := c.Handle(context.Background(), env)
	require.NoError(t, err)

	assert.True(t, lastEventAtCalled, "SetLastEventAt must be called after applyProjection runs")
	assert.Contains(t, idemp.marked, env.ID, "MarkProcessed must be called at the end of a successful projection")
}

// TestHandle_IsProcessedError_PropagatesError verifies that an error from
// IsProcessed (e.g., DB connection failure) propagates and Handle returns it.
func TestHandle_IsProcessedError_PropagatesError(t *testing.T) {
	dbErr := errSentinel("idempotency_db_error")
	idemp := &htIdempotency{
		isProcessedFn: func(_ context.Context, _, _ string) (bool, error) {
			return false, dbErr
		},
	}
	// Use a known event type so the code reaches the IsProcessed probe.
	c := NewMembershipEventConsumer(&htSyncTxRunner{}, nil, idemp, nil, nil, 5*time.Minute, nil)

	env := mkHandleEnv("TrialExpired", uuid.New().String())
	err := c.Handle(context.Background(), env)

	require.Error(t, err)
	assert.ErrorIs(t, err, dbErr)
}

// errSentinel is a simple error value for tests.
type errSentinel string

func (e errSentinel) Error() string { return string(e) }
