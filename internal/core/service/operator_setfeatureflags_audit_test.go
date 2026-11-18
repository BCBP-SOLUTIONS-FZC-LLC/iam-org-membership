package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type ffRecorder struct {
	entries []port.AuditEntry
	err     error
}

func (r *ffRecorder) Record(_ context.Context, e port.AuditEntry) error {
	if r.err != nil {
		return r.err
	}
	r.entries = append(r.entries, e)
	return nil
}
func (r *ffRecorder) RecordBatch(context.Context, []port.AuditEntry) error { return nil }

type recTxRunner struct{ rec port.AuditRecorder }

func (r recTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(port.WithAuditRecorder(ctx, r.rec))
}

// ffVersionedRepo returns oldFlags at version 3 before the update and the
// new flags at newVersion after it.
func ffVersionedRepo(oldFlags map[string]any, newVersion int64, updatedAt time.Time) *ffTenantRepo {
	updated := false
	var newFlags map[string]any
	return &ffTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			if !updated {
				return &domain.Tenant{ID: id, RecordVersion: 3, FeatureFlags: oldFlags}, nil
			}
			return &domain.Tenant{ID: id, RecordVersion: newVersion, FeatureFlags: newFlags, UpdatedAt: updatedAt}, nil
		},
		setFeatureFlagsFn: func(context.Context, uuid.UUID, []byte, int64) error {
			updated = true
			return nil
		},
	}
}

func TestOperator_SetFeatureFlags_AuditsChangesInTx(t *testing.T) {
	tenantID, operator := uuid.New(), uuid.New()
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	repo := ffVersionedRepo(map[string]any{"sso_enabled": true, "custom_branding": false, "require_mfa_all_users": "x"}, 4, at)
	rec := &ffRecorder{}
	svc := buildOperatorWithPool(repo, recTxRunner{rec: rec}, nil)
	ctx := requestctx.WithContext(context.Background(), &requestctx.RequestContext{UserID: operator, ClientIP: "10.0.0.9", UserAgent: "ops"})

	_, err := svc.SetFeatureFlags(ctx, tenantID, map[string]any{"sso_enabled": false, "custom_branding": true}, 3)
	require.NoError(t, err)
	require.Len(t, rec.entries, 1)
	e := rec.entries[0]
	assert.Equal(t, port.AuditEntryTenantSettingChanged, e.EntryType)
	assert.Equal(t, fmt.Sprintf("config.tenant_setting.changed:%s:4", tenantID), e.IdempotencyKey)
	assert.Equal(t, tenantID, e.TenantID)
	assert.Equal(t, "update", e.Action)
	assert.Equal(t, port.UserAuditActor(operator), e.Actor)
	assert.Equal(t, at, e.OccurredAt)
	assert.Equal(t, "TenantSettingChanged", e.SourceEventType)
	assert.Equal(t, "10.0.0.9", e.IPAddress)
	assert.Equal(t, map[string]any{"feature_flags": map[string]any{
		"sso_enabled":           map[string]any{"change": "disabled", "old": true, "new": false},
		"custom_branding":       map[string]any{"change": "enabled", "old": false, "new": true},
		"require_mfa_all_users": map[string]any{"change": "removed", "old": "x"},
	}}, e.Metadata)
}

func TestFeatureFlagChanges_Kinds(t *testing.T) {
	got := featureFlagChanges(
		map[string]any{"a": "x", "b": float64(1), "c": true},
		map[string]any{"a": "y", "b": 1, "c": true, "d": false},
	)
	assert.Equal(t, map[string]any{
		"a": map[string]any{"change": "modified", "old": "x", "new": "y"},
		"d": map[string]any{"change": "created", "new": false},
	}, got, "b (1 == 1.0) and c are unchanged")
}

func TestOperator_SetFeatureFlags_NoChangeNotAudited(t *testing.T) {
	rec := &ffRecorder{}
	repo := ffVersionedRepo(map[string]any{"sso_enabled": true}, 3, time.Now())
	svc := buildOperatorWithPool(repo, recTxRunner{rec: rec}, nil)
	_, err := svc.SetFeatureFlags(context.Background(), uuid.New(), map[string]any{"sso_enabled": true}, 3)
	require.NoError(t, err)
	assert.Empty(t, rec.entries)
}

func TestOperator_SetFeatureFlags_SystemActorWithoutCaller(t *testing.T) {
	rec := &ffRecorder{}
	updatedAt := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	svc := buildOperatorWithPool(ffVersionedRepo(nil, 4, updatedAt), recTxRunner{rec: rec}, nil)
	_, err := svc.SetFeatureFlags(context.Background(), uuid.New(), map[string]any{"sso_enabled": true}, 3)
	require.NoError(t, err)
	require.Len(t, rec.entries, 1)
	assert.Equal(t, port.SystemAuditActor(), rec.entries[0].Actor)
	assert.Equal(t, updatedAt, rec.entries[0].OccurredAt)
}

// OccurredAt comes from the written row (platform-audit LLD §5.3 rule 2): a
// row without a timestamp fails the write instead of keying the entry on the
// call time.
func TestOperator_SetFeatureFlags_ZeroRowTimestampFailsTheWrite(t *testing.T) {
	rec := &ffRecorder{}
	svc := buildOperatorWithPool(ffVersionedRepo(nil, 4, time.Time{}), recTxRunner{rec: rec}, nil)
	_, err := svc.SetFeatureFlags(context.Background(), uuid.New(), map[string]any{"sso_enabled": true}, 3)
	require.ErrorContains(t, err, "zero OccurredAt")
	assert.Empty(t, rec.entries)
}

func TestOperator_SetFeatureFlags_RecorderErrorFailsTheWrite(t *testing.T) {
	boom := errors.New("connection lost")
	svc := buildOperatorWithPool(ffVersionedRepo(nil, 4, time.Now()), recTxRunner{rec: &ffRecorder{err: boom}}, nil)
	_, err := svc.SetFeatureFlags(context.Background(), uuid.New(), map[string]any{"sso_enabled": true}, 3)
	assert.ErrorIs(t, err, boom)
}

func TestOperator_SetFeatureFlags_InTxReadErrorFailsTheWrite(t *testing.T) {
	boom := errors.New("read failed")
	calls := 0
	repo := &ffTenantRepo{findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
		calls++
		if calls == 2 {
			return nil, boom
		}
		return &domain.Tenant{ID: id, RecordVersion: 3}, nil
	}}
	svc := buildOperatorWithPool(repo, recTxRunner{rec: &ffRecorder{}}, nil)
	_, err := svc.SetFeatureFlags(context.Background(), uuid.New(), map[string]any{"sso_enabled": true}, 3)
	assert.ErrorIs(t, err, boom)
}
