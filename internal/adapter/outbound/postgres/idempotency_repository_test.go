package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The behaviour against a real processed_events table (duplicate skip,
// rollback on fn error, concurrent claims, prune) is covered in
// test/postgres; these cover the paths that need no database.

func TestInboxRepository_NoPool_ReturnsErrorWithoutRunningFn(t *testing.T) {
	repo := NewInboxRepository(nil, nil)
	ran := false
	dup, err := repo.ProcessOnce(context.Background(), "membership", "0192f5e0-0000-7000-8000-000000000001", "TenantSuspended",
		func(context.Context) error { ran = true; return nil })
	require.Error(t, err)
	assert.False(t, dup)
	assert.False(t, ran)

	_, err = repo.Prune(context.Background(), "membership", time.Hour, 0)
	require.Error(t, err)
}

func TestInboxRepository_EmptyConsumer_ReturnsError(t *testing.T) {
	_, err := NewInboxRepository(nil, nil).ProcessOnce(context.Background(), "", "id", "T", func(context.Context) error { return nil })
	require.Error(t, err)
}
