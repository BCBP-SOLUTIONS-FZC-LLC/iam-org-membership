package service

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// Additional getCachedResolution branch coverage: gmCache's plain map-
// backed MGet can never itself error, and a corrupted cache entry never
// naturally occurs in these tests — so both fall-through-to-miss branches
// (MGet error, and any-of-three-corrupt-JSON) are exercised here directly.

// gmErrCache wraps gmCache, letting a test force MGet to error.
type gmErrCache struct {
	*gmCache
	mgetErr error
}

func newGMErrCache() *gmErrCache { return &gmErrCache{gmCache: newGMCache()} }

func (c *gmErrCache) MGet(ctx context.Context, keys []string) ([][]byte, error) {
	if c.mgetErr != nil {
		return nil, c.mgetErr
	}
	return c.gmCache.MGet(ctx, keys)
}

var _ port.Cache = (*gmErrCache)(nil)

func TestGetCachedResolution_MGetErrorIsTreatedAsMiss(t *testing.T) {
	tenantID := uuid.New()
	client := &fakeGroupMappingClient{
		resolveFn: func(context.Context, uuid.UUID, []string) (*port.GroupResolution, error) {
			return &port.GroupResolution{}, nil
		},
	}
	cache := newGMErrCache()
	cache.mgetErr = errors.New("cache unavailable")
	svc := buildResolveSvc(client, cache)

	svc.resolveMappings(context.Background(), tenantID, []string{"eng-team"})
	assert.Equal(t, 1, client.calls, "an MGet error must be treated as a full miss and fall through to the client")
}

func TestGetCachedResolution_CorruptJSONInAnyKeyIsTreatedAsMiss(t *testing.T) {
	tenantID := uuid.New()
	client := &fakeGroupMappingClient{
		resolveFn: func(context.Context, uuid.UUID, []string) (*port.GroupResolution, error) {
			return &port.GroupResolution{}, nil
		},
	}
	cache := newGMCache()
	// All three keys present (so the length/nil pre-check passes) but GRM's
	// payload is not valid JSON — must still be treated as a full miss.
	cache.values[cacheKeyGDM(tenantID)] = []byte(`[]`)
	cache.values[cacheKeyGRM(tenantID)] = []byte(`not-json`)
	cache.values[cacheKeyGTRM(tenantID)] = []byte(`[]`)
	svc := buildResolveSvc(client, cache)

	svc.resolveMappings(context.Background(), tenantID, []string{"eng-team"})
	assert.Equal(t, 1, client.calls, "corrupt JSON in any one of the three keys must fall through to the client")
}
