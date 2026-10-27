package port

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
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

// PlanGenerationKey builds om:plans:gen:{plan}, the per-plan generation token
// (LLD §6.3, rev 2.39). The catalog consumer sets it to the event id of every
// PlanCatalogChanged for that plan; I-8 reads it in the same MGET as
// om:memberships:{tenant}:{user} and treats an entry stamped with a different
// generation as a miss. One SET therefore retires every cached I-8 projection
// of every tenant on the plan, with no per-tenant or per-member enumeration.
// Lives in port so the writer (consumer adapter) and the reader (AuthZService)
// share one definition.
func PlanGenerationKey(plan domain.TenantPlan) string {
	return "om:plans:gen:" + string(plan)
}

// PlanGenerationTTL bounds the life of an om:plans:gen:{plan} key. It only has
// to outlive the longest om:memberships entry (300 s + 30 s jitter): once it
// expires, every entry stamped with its value has expired too.
const PlanGenerationTTL = 24 * time.Hour
