package port

import (
	"context"
	"time"
)

// Cache is the advisory cache interface (§6, CACHE-2/9). Miss, timeout, and
// outage MUST fall through to Postgres — cache is never a correctness
// dependency. Get returns (nil, nil) on cache miss.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	MGet(ctx context.Context, keys []string) ([][]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	Delete(ctx context.Context, keys ...string) error
	Health(ctx context.Context) error
	Close() error
}
