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

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
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

func TestCatalogConsumer_Handle_MarksProcessedWithConsumerThenEventID(t *testing.T) {
	dedup := &recordingDedup{}
	cache := &stubCatalogCache{}
	c := NewCatalogConsumer(cache, dedup, stubTx{}, nil)

	env := catalogEnv(eventDepartmentCatalogChanged, "")
	require.NoError(t, c.Handle(context.Background(), env))

	require.Len(t, dedup.isCalls, 1)
	assert.Equal(t, [2]string{catalogConsumerName, env.ID}, dedup.isCalls[0],
		"IsProcessed must be (consumer, eventID) — the processed_events PK order")
	require.Len(t, dedup.markCalls, 1)
	assert.Equal(t, [2]string{catalogConsumerName, env.ID}, dedup.markCalls[0],
		"MarkProcessed must be (consumer, eventID)")
	require.Len(t, cache.deleted, 1)
	assert.Equal(t, []string{omDepartmentsKey, omDepartmentsStaleKey}, cache.deleted[0])
}

func TestCatalogConsumer_Handle_SkipDuplicate_DoesNotClearCache(t *testing.T) {
	env := catalogEnv(eventDepartmentCatalogChanged, uuid.New().String())
	dedup := &recordingDedup{processed: map[string]bool{
		catalogConsumerName + "\x00" + env.ID: true,
	}}
	cache := &stubCatalogCache{}
	c := NewCatalogConsumer(cache, dedup, stubTx{}, nil)

	require.NoError(t, c.Handle(context.Background(), env))
	assert.Empty(t, cache.deleted, "already-processed events must not hit Valkey")
	assert.Empty(t, dedup.markCalls, "skipDuplicate short-circuits before mark")
}

func TestCatalogConsumer_Handle_UnknownType_AckUnknownMarksProcessed(t *testing.T) {
	dedup := &recordingDedup{}
	c := NewCatalogConsumer(&stubCatalogCache{}, dedup, stubTx{}, nil)

	env := catalogEnv("NeverHeardOfThis", uuid.New().String())
	require.NoError(t, c.Handle(context.Background(), env))
	require.Len(t, dedup.markCalls, 1)
	assert.Equal(t, [2]string{catalogConsumerName, env.ID}, dedup.markCalls[0])
}

func TestCatalogConsumer_Handle_InvalidEventID_ErrorsWithoutMark(t *testing.T) {
	dedup := &recordingDedup{}
	c := NewCatalogConsumer(&stubCatalogCache{}, dedup, stubTx{}, nil)

	err := c.Handle(context.Background(), catalogEnv(eventDepartmentCatalogChanged, "not-a-uuid"))
	require.Error(t, err)
	assert.Empty(t, dedup.markCalls)
}

func TestCatalogConsumer_Handle_CacheDeleteError_StillMarksProcessed(t *testing.T) {
	dedup := &recordingDedup{}
	cache := &stubCatalogCache{err: errors.New("valkey down")}
	c := NewCatalogConsumer(cache, dedup, stubTx{}, nil)

	env := catalogEnv(eventDepartmentCatalogChanged, "")
	require.NoError(t, c.Handle(context.Background(), env))
	require.Len(t, dedup.markCalls, 1)
	assert.Equal(t, [2]string{catalogConsumerName, env.ID}, dedup.markCalls[0])
}
