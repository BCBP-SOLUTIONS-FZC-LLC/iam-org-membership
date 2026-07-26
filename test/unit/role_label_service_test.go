// Unit tests for internal/core/service/role_label_service.go.
// Hand-rolled port stubs — no testcontainers.
package unit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Stubs ──────────────────────────────────────────────────────────────

type fakeLabelRepo struct {
	listFn   func(ctx context.Context, tenantID uuid.UUID) ([]domain.DeptRoleLabel, error)
	updateFn func(ctx context.Context, tenantID uuid.UUID, code domain.DeptRole, displayName string, expectedVersion int64) (*domain.DeptRoleLabel, error)
	seedFn   func(ctx context.Context, tenantID uuid.UUID) ([]domain.DeptRoleLabel, error)
}

func (f *fakeLabelRepo) List(ctx context.Context, tenantID uuid.UUID) ([]domain.DeptRoleLabel, error) {
	return f.listFn(ctx, tenantID)
}
func (f *fakeLabelRepo) Update(ctx context.Context, tenantID uuid.UUID, code domain.DeptRole, displayName string, expectedVersion int64) (*domain.DeptRoleLabel, error) {
	return f.updateFn(ctx, tenantID, code, displayName, expectedVersion)
}
func (f *fakeLabelRepo) Seed(ctx context.Context, tenantID uuid.UUID) ([]domain.DeptRoleLabel, error) {
	return f.seedFn(ctx, tenantID)
}

var _ port.DeptRoleLabelRepository = (*fakeLabelRepo)(nil)

// spyCache records Delete calls so we can assert cache invalidation happens
// on Update but NOT on validation failures.
type spyCache struct {
	deleteCalls []string
	deleteErr   error
}

func (c *spyCache) Get(context.Context, string) ([]byte, error)      { return nil, nil }
func (c *spyCache) MGet(context.Context, []string) ([][]byte, error) { return nil, nil }
func (c *spyCache) Set(context.Context, string, []byte, time.Duration) error {
	return nil
}
func (c *spyCache) SetNX(context.Context, string, []byte, time.Duration) (bool, error) {
	return false, nil
}
func (c *spyCache) Delete(_ context.Context, keys ...string) error {
	c.deleteCalls = append(c.deleteCalls, keys...)
	return c.deleteErr
}
func (c *spyCache) Health(context.Context) error { return nil }
func (c *spyCache) Close() error                 { return nil }

var _ port.Cache = (*spyCache)(nil)

// ── List (P-12) ────────────────────────────────────────────────────────

func TestRoleLabelService_List_DelegatesToRepo(t *testing.T) {
	tenantID := uuid.New()
	want := []domain.DeptRoleLabel{{ID: uuid.New(), TenantID: tenantID, RoleCode: domain.DeptPreparator}}
	labels := &fakeLabelRepo{
		listFn: func(_ context.Context, tt uuid.UUID) ([]domain.DeptRoleLabel, error) {
			assert.Equal(t, tenantID, tt)
			return want, nil
		},
	}
	svc := service.NewRoleLabelService(labels, nil)

	got, err := svc.List(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// ── Update (P-13) — validation ─────────────────────────────────────────

func TestRoleLabelService_Update_RejectsInvalidRoleCode(t *testing.T) {
	svc := service.NewRoleLabelService(&fakeLabelRepo{}, nil)

	_, err := svc.Update(context.Background(), uuid.New(), "wizard", "Wizard", 1)

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
	assert.Equal(t, "invalid_role", de.Details["code"])
}

func TestRoleLabelService_Update_RejectsEmptyDisplayName(t *testing.T) {
	svc := service.NewRoleLabelService(&fakeLabelRepo{}, nil)

	_, err := svc.Update(context.Background(), uuid.New(), "preparator", "", 1)

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
}

func TestRoleLabelService_Update_AcceptsAllThreeRoleCodes(t *testing.T) {
	for _, code := range []string{"preparator", "reviewer", "approver"} {
		t.Run(code, func(t *testing.T) {
			labels := &fakeLabelRepo{
				updateFn: func(_ context.Context, _ uuid.UUID, c domain.DeptRole, name string, ver int64) (*domain.DeptRoleLabel, error) {
					assert.Equal(t, domain.DeptRole(code), c)
					assert.Equal(t, "Custom Label", name)
					assert.EqualValues(t, 3, ver)
					return &domain.DeptRoleLabel{ID: uuid.New(), RoleCode: c, DisplayName: name}, nil
				},
			}
			svc := service.NewRoleLabelService(labels, nil)

			got, err := svc.Update(context.Background(), uuid.New(), code, "Custom Label", 3)
			require.NoError(t, err)
			assert.Equal(t, "Custom Label", got.DisplayName)
		})
	}
}

// ── Update — repo error propagates ─────────────────────────────────────

func TestRoleLabelService_Update_RepoErrorPropagates(t *testing.T) {
	repoErr := errors.New("optimistic lock conflict")
	labels := &fakeLabelRepo{
		updateFn: func(context.Context, uuid.UUID, domain.DeptRole, string, int64) (*domain.DeptRoleLabel, error) {
			return nil, repoErr
		},
	}
	svc := service.NewRoleLabelService(labels, nil)

	_, err := svc.Update(context.Background(), uuid.New(), "preparator", "X", 1)
	assert.ErrorIs(t, err, repoErr)
}

// ── Update — cache invalidation semantics ──────────────────────────────

func TestRoleLabelService_Update_InvalidatesRoleCacheOnSuccess(t *testing.T) {
	tenantID := uuid.New()
	labels := &fakeLabelRepo{
		updateFn: func(context.Context, uuid.UUID, domain.DeptRole, string, int64) (*domain.DeptRoleLabel, error) {
			return &domain.DeptRoleLabel{ID: uuid.New()}, nil
		},
	}
	cache := &spyCache{}
	svc := service.NewRoleLabelService(labels, cache)

	_, err := svc.Update(context.Background(), tenantID, "reviewer", "Approver v2", 1)
	require.NoError(t, err)

	require.Len(t, cache.deleteCalls, 1, "successful update must invalidate the roles cache once")
	assert.Contains(t, cache.deleteCalls[0], tenantID.String(),
		"deleted key must include the tenant id (om:roles:<tenant>)")
}

func TestRoleLabelService_Update_NoCacheInvalidationOnRepoFailure(t *testing.T) {
	labels := &fakeLabelRepo{
		updateFn: func(context.Context, uuid.UUID, domain.DeptRole, string, int64) (*domain.DeptRoleLabel, error) {
			return nil, errors.New("fk violation")
		},
	}
	cache := &spyCache{}
	svc := service.NewRoleLabelService(labels, cache)

	_, err := svc.Update(context.Background(), uuid.New(), "approver", "Owner", 1)
	assert.Error(t, err)
	assert.Empty(t, cache.deleteCalls, "must not invalidate cache when repo update failed")
}

func TestRoleLabelService_Update_CacheDeleteErrorSwallowed(t *testing.T) {
	// Cache is advisory (CACHE-2/9) — a Delete failure must NOT surface to
	// the caller. The label update already committed.
	labels := &fakeLabelRepo{
		updateFn: func(context.Context, uuid.UUID, domain.DeptRole, string, int64) (*domain.DeptRoleLabel, error) {
			return &domain.DeptRoleLabel{ID: uuid.New()}, nil
		},
	}
	cache := &spyCache{deleteErr: errors.New("cache timeout")}
	svc := service.NewRoleLabelService(labels, cache)

	got, err := svc.Update(context.Background(), uuid.New(), "preparator", "X", 1)
	require.NoError(t, err, "cache errors must be swallowed per CACHE-2 advisory-only invariant")
	assert.NotNil(t, got)
}
