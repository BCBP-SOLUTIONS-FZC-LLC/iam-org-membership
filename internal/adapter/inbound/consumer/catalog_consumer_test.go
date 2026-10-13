package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

// CAT-GAP-004: PlanCatalogChanged handler tests.

func TestCatalogConsumer_Handle_PlanChanged_ClearsPlansCache(t *testing.T) {
	// PlanCatalogChanged must clear om:plans + om:plans:stale — no processed_events write.
	dedup := &recordingDedup{}
	cache := &stubCatalogCache{}
	c := NewCatalogConsumer(cache, newFakeInbox(stubTx{}, dedup), nil)

	env := catalogEnv(eventPlanCatalogChanged, "")
	require.NoError(t, c.Handle(context.Background(), env))

	assert.Empty(t, dedup.isCalls, "no IsProcessed call for PlanCatalogChanged")
	assert.Empty(t, dedup.markCalls, "no MarkProcessed call for PlanCatalogChanged")
	require.Len(t, cache.deleted, 1)
	assert.Equal(t, []string{omPlansKey, omPlansStaleKey}, cache.deleted[0])
}

func TestCatalogConsumer_Handle_PlanChanged_Redelivery_ClearsCacheAgain(t *testing.T) {
	// Redelivery of PlanCatalogChanged must still clear the cache — no dedup gate.
	cache := &stubCatalogCache{}
	c := NewCatalogConsumer(cache, newFakeInbox(stubTx{}, &recordingDedup{}), nil)

	env := catalogEnv(eventPlanCatalogChanged, uuid.New().String())
	require.NoError(t, c.Handle(context.Background(), env))
	require.NoError(t, c.Handle(context.Background(), env))

	assert.Len(t, cache.deleted, 2, "both deliveries must clear the plans cache")
}

func TestCatalogConsumer_Handle_PlanChanged_InvalidEventID_Errors(t *testing.T) {
	c := NewCatalogConsumer(&stubCatalogCache{}, newFakeInbox(stubTx{}, &recordingDedup{}), nil)

	err := c.Handle(context.Background(), catalogEnv(eventPlanCatalogChanged, "not-a-uuid"))
	require.Error(t, err)
}

func TestCatalogConsumer_Handle_PlanChanged_CacheDeleteError_ReturnsNil(t *testing.T) {
	// Cache DELETE failure is best-effort for plan invalidation too — ack the message.
	cache := &stubCatalogCache{err: errors.New("valkey down")}
	c := NewCatalogConsumer(cache, newFakeInbox(stubTx{}, &recordingDedup{}), nil)

	env := catalogEnv(eventPlanCatalogChanged, "")
	require.NoError(t, c.Handle(context.Background(), env))
}
