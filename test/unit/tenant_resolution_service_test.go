// Unit tests for TenantResolutionService (PUB-1, LLD §5.4). The service is
// a pass-through; these pin that it forwards the identifier unchanged and
// never turns a no-match into an error. DB-level query correctness is in
// test/postgres/public_tenant_resolve_test.go.
package unit_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
)

type trsRepo struct {
	port.TenantRepositoryNoop
	res  domain.TenantResolution
	err  error
	seen string
}

func (r *trsRepo) ResolveByIdentifier(_ context.Context, identifier string) (domain.TenantResolution, error) {
	r.seen = identifier
	return r.res, r.err
}

var _ port.TenantRepository = (*trsRepo)(nil)

func TestTenantResolution_FoundDedicated(t *testing.T) {
	repo := &trsRepo{res: domain.TenantResolution{Found: true, Realm: "tenant-acme", RealmType: domain.RealmDedicated}}
	got, err := service.NewTenantResolutionService(repo).Resolve(context.Background(), "acme")
	require.NoError(t, err)
	assert.Equal(t, "acme", repo.seen)
	assert.Equal(t, repo.res, got)
	assert.Nil(t, got.TenantID)
}

func TestTenantResolution_FoundShared_CarriesTenantID(t *testing.T) {
	id := uuid.New()
	repo := &trsRepo{res: domain.TenantResolution{Found: true, Realm: "trial", RealmType: domain.RealmShared, TenantID: &id}}
	got, err := service.NewTenantResolutionService(repo).Resolve(context.Background(), "new-co")
	require.NoError(t, err)
	require.NotNil(t, got.TenantID)
	assert.Equal(t, id, *got.TenantID)
}

func TestTenantResolution_NotFound_IsNotAnError(t *testing.T) {
	got, err := service.NewTenantResolutionService(&trsRepo{}).Resolve(context.Background(), "nobody@example.com")
	require.NoError(t, err)
	assert.False(t, got.Found)
}

func TestTenantResolution_RepoErrorPassesThrough(t *testing.T) {
	want := domain.NewError(domain.ErrDBUnavailable, "db down")
	_, err := service.NewTenantResolutionService(&trsRepo{err: want}).Resolve(context.Background(), "acme")
	assert.ErrorIs(t, err, want)
}

func TestMetrics_IncPublicTenantResolve_NilMetric_NoPanic(t *testing.T) {
	saved := metrics.PublicTenantResolve
	metrics.PublicTenantResolve = nil
	defer func() { metrics.PublicTenantResolve = saved }()
	assert.NotPanics(t, func() { metrics.IncPublicTenantResolve("found") })
}

// OBS-8: both label values exist at 0 from startup, then count.
func TestMetrics_IncPublicTenantResolve_PreWarmedAndCounts(t *testing.T) {
	ensureMetricsRegisteredForUnit()
	before := testutil.ToFloat64(metrics.PublicTenantResolve.WithLabelValues("not_found"))
	metrics.IncPublicTenantResolve("not_found")
	assert.Equal(t, before+1, testutil.ToFloat64(metrics.PublicTenantResolve.WithLabelValues("not_found")))
	assert.GreaterOrEqual(t, testutil.CollectAndCount(metrics.PublicTenantResolve), 2)
}
