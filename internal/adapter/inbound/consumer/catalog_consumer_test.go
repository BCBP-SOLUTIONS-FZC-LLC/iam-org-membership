package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// recordingDedup keys processed_events by (consumer, eventID) so swapped
// argument order fails the test instead of silently writing the wrong PK.
type recordingDedup struct {
	processed          map[string]bool
	isCalls, markCalls [][2]string
	isErr, markErr     error
}

func (d *recordingDedup) key(consumer, eventID string) string {
	return consumer + "\x00" + eventID
}

func (d *recordingDedup) IsProcessed(_ context.Context, consumer, eventID string) (bool, error) {
	d.isCalls = append(d.isCalls, [2]string{consumer, eventID})
	if d.isErr != nil {
		return false, d.isErr
	}
	return d.processed[d.key(consumer, eventID)], nil
}

func (d *recordingDedup) MarkProcessed(_ context.Context, consumer, eventID string) error {
	d.markCalls = append(d.markCalls, [2]string{consumer, eventID})
	if d.markErr != nil {
		return d.markErr
	}
	if d.processed == nil {
		d.processed = map[string]bool{}
	}
	d.processed[d.key(consumer, eventID)] = true
	return nil
}

type stubCatalogCache struct {
	deleted [][]string
	err     error
}

func (s *stubCatalogCache) Get(context.Context, string) ([]byte, error) { return nil, nil }
func (s *stubCatalogCache) MGet(context.Context, []string) ([][]byte, error) {
	return nil, nil
}
func (s *stubCatalogCache) Set(context.Context, string, []byte, time.Duration) error { return nil }
func (s *stubCatalogCache) SetNX(context.Context, string, []byte, time.Duration) (bool, error) {
	return false, nil
}
func (s *stubCatalogCache) Delete(_ context.Context, keys ...string) error {
	copied := append([]string(nil), keys...)
	s.deleted = append(s.deleted, copied)
	return s.err
}
func (s *stubCatalogCache) Health(context.Context) error { return nil }
func (s *stubCatalogCache) Close() error                 { return nil }

func catalogEnv(eventType, id string) events.Envelope[json.RawMessage] {
	if id == "" {
		id = uuid.New().String()
	}
	return events.Envelope[json.RawMessage]{ID: id, Type: eventType}
}

func TestCatalogConsumer_Handle_ClearsCache_NoDedup(t *testing.T) {
	// GAP-L: DepartmentCatalogChanged no longer writes processed_events.
	// Clearing the cache twice is always safe; the DB round-trip is unnecessary
	// overhead for a cache-invalidation-only event.
	dedup := &recordingDedup{}
	cache := &stubCatalogCache{}
	c := NewCatalogConsumer(cache, newFakeInbox(stubTx{}, dedup), nil)

	env := catalogEnv(eventDepartmentCatalogChanged, "")
	require.NoError(t, c.Handle(context.Background(), env))

	assert.Empty(t, dedup.isCalls, "no IsProcessed call for DepartmentCatalogChanged")
	assert.Empty(t, dedup.markCalls, "no MarkProcessed call for DepartmentCatalogChanged")
	require.Len(t, cache.deleted, 1)
	assert.Equal(t, []string{omDepartmentsKey, omDepartmentsStaleKey}, cache.deleted[0])
}

func TestCatalogConsumer_Handle_Redelivery_ClearsCacheAgain(t *testing.T) {
	// Redelivery of the same event must still clear the cache — no dedup gate.
	dedup := &recordingDedup{}
	cache := &stubCatalogCache{}
	c := NewCatalogConsumer(cache, newFakeInbox(stubTx{}, dedup), nil)

	env := catalogEnv(eventDepartmentCatalogChanged, uuid.New().String())
	require.NoError(t, c.Handle(context.Background(), env))
	require.NoError(t, c.Handle(context.Background(), env)) // simulate redelivery

	assert.Len(t, cache.deleted, 2, "both deliveries must clear the cache")
	assert.Empty(t, dedup.markCalls, "redelivery path must not touch processed_events")
}

func TestCatalogConsumer_Handle_UnknownType_AckUnknownMarksProcessed(t *testing.T) {
	dedup := &recordingDedup{}
	c := NewCatalogConsumer(&stubCatalogCache{}, newFakeInbox(stubTx{}, dedup), nil)

	env := catalogEnv("NeverHeardOfThis", uuid.New().String())
	require.NoError(t, c.Handle(context.Background(), env))
	require.Len(t, dedup.markCalls, 1)
	assert.Equal(t, [2]string{catalogConsumerName, env.ID}, dedup.markCalls[0])
}

func TestCatalogConsumer_Handle_InvalidEventID_ErrorsWithoutMark(t *testing.T) {
	dedup := &recordingDedup{}
	c := NewCatalogConsumer(&stubCatalogCache{}, newFakeInbox(stubTx{}, dedup), nil)

	err := c.Handle(context.Background(), catalogEnv(eventDepartmentCatalogChanged, "not-a-uuid"))
	require.Error(t, err)
	assert.Empty(t, dedup.markCalls)
}

func TestCatalogConsumer_Handle_CacheDeleteError_ReturnsNil(t *testing.T) {
	// Cache DELETE failure is best-effort — Handle still returns nil so the SQS
	// message is acked and the 600s TTL self-heals. No processed_events write.
	dedup := &recordingDedup{}
	cache := &stubCatalogCache{err: errors.New("valkey down")}
	c := NewCatalogConsumer(cache, newFakeInbox(stubTx{}, dedup), nil)

	env := catalogEnv(eventDepartmentCatalogChanged, "")
	require.NoError(t, c.Handle(context.Background(), env))
	assert.Empty(t, dedup.markCalls, "cache error must not trigger a processed_events write")
}

// PlanCatalogChanged handler tests (LLD §6.3 / §7.1, rev 2.39): clear om:plans,
// bump om:plans:gen:{plan}, dedup through processed_events, never touch
// tenants.feature_flags (T-9).

// planCache records Delete and Set calls in order.
type planCache struct {
	stubCatalogCache
	ops    []string // "del:<keys>" / "set:<key>=<value>"
	sets   map[string]string
	setErr error
}

func (c *planCache) Delete(_ context.Context, keys ...string) error {
	c.ops = append(c.ops, "del:"+strings.Join(keys, ","))
	return c.err
}

func (c *planCache) Set(_ context.Context, key string, v []byte, ttl time.Duration) error {
	if c.setErr != nil {
		return c.setErr
	}
	if c.sets == nil {
		c.sets = map[string]string{}
	}
	c.sets[key] = string(v)
	c.ops = append(c.ops, "set:"+key+"="+string(v))
	if ttl != port.PlanGenerationTTL {
		panic("generation key written with the wrong TTL")
	}
	return nil
}

// planEnv builds a PlanCatalogChanged envelope with Catalog's payload shape.
func planEnv(code string) events.Envelope[json.RawMessage] {
	payload, _ := json.Marshal(map[string]any{"code": code, "change_type": "updated", "feature_set": map[string]any{"sso_enabled": true}})
	return events.Envelope[json.RawMessage]{ID: uuid.New().String(), Type: eventPlanCatalogChanged, Payload: payload}
}

// The consumer holds no TenantRepository at all any more; this guards against
// one being re-introduced for a feature_flags write (T-9).
func TestCatalogConsumer_HasNoTenantRepository(t *testing.T) {
	typ := reflect.TypeOf(CatalogConsumer{})
	for i := 0; i < typ.NumField(); i++ {
		assert.NotEqual(t, reflect.TypeOf((*port.TenantRepository)(nil)).Elem(), typ.Field(i).Type,
			"T-9: the catalog consumer must not hold a TenantRepository (no feature_flags write)")
	}
}

func TestCatalogConsumer_PlanChanged_ClearsPlansThenBumpsThatPlan(t *testing.T) {
	dedup := &recordingDedup{}
	cache := &planCache{}
	c := NewCatalogConsumer(cache, newFakeInbox(stubTx{}, dedup), nil)

	env := planEnv("enterprise")
	require.NoError(t, c.Handle(context.Background(), env))

	assert.Equal(t, []string{
		"del:" + omPlansKey + "," + omPlansStaleKey,
		"set:" + port.PlanGenerationKey(domain.PlanEnterprise) + "=" + env.ID,
	}, cache.ops, "om:plans is cleared before the generation bump, and only the changed plan is bumped")
	require.Len(t, dedup.markCalls, 1, "PlanCatalogChanged is inbox-deduped")
	assert.Equal(t, [2]string{catalogConsumerName, env.ID}, dedup.markCalls[0])
}

func TestCatalogConsumer_PlanChanged_Redelivery_IsDeduped(t *testing.T) {
	dedup := &recordingDedup{}
	cache := &planCache{}
	c := NewCatalogConsumer(cache, newFakeInbox(stubTx{}, dedup), nil)

	env := planEnv("pro")
	require.NoError(t, c.Handle(context.Background(), env))
	require.NoError(t, c.Handle(context.Background(), env))

	assert.Len(t, cache.ops, 2, "the redelivery is skipped by processed_events")
}

func TestCatalogConsumer_PlanChanged_UnknownOrMissingCode_BumpsEveryPlan(t *testing.T) {
	for _, env := range []events.Envelope[json.RawMessage]{
		planEnv("platinum"),
		catalogEnv(eventPlanCatalogChanged, ""), // no payload at all
		{ID: uuid.New().String(), Type: eventPlanCatalogChanged, Payload: json.RawMessage(`not json`)},
	} {
		cache := &planCache{}
		c := NewCatalogConsumer(cache, newFakeInbox(stubTx{}, &recordingDedup{}), nil)
		require.NoError(t, c.Handle(context.Background(), env))
		for _, p := range domain.KnownPlans() {
			assert.Equal(t, env.ID, cache.sets[port.PlanGenerationKey(p)], "plan %s must be bumped", p)
		}
	}
}

func TestCatalogConsumer_PlanChanged_InvalidEventID_Errors(t *testing.T) {
	dedup := &recordingDedup{}
	cache := &planCache{}
	c := NewCatalogConsumer(cache, newFakeInbox(stubTx{}, dedup), nil)

	env := planEnv("starter")
	env.ID = "not-a-uuid"
	require.Error(t, c.Handle(context.Background(), env))
	assert.Empty(t, cache.ops)
	assert.Empty(t, dedup.markCalls)
}

func TestCatalogConsumer_PlanChanged_CacheErrors_RetryWithoutMark(t *testing.T) {
	// A Valkey failure must not be acked: I-8 would keep the old flags until
	// the TTL. The error rolls back the processed_events claim, SQS redelivers.
	for name, cache := range map[string]*planCache{
		"delete": {stubCatalogCache: stubCatalogCache{err: errors.New("valkey down")}},
		"set":    {setErr: errors.New("valkey down")},
	} {
		t.Run(name, func(t *testing.T) {
			dedup := &recordingDedup{}
			c := NewCatalogConsumer(cache, newFakeInbox(stubTx{}, dedup), nil)
			require.Error(t, c.Handle(context.Background(), planEnv("pro")))
			assert.Empty(t, dedup.markCalls)
		})
	}
}

func TestProcessedEventsConsumers_OneCatalogBucket(t *testing.T) {
	assert.Equal(t, []string{consumerName, catalogConsumerName}, ProcessedEventsConsumers())
}
