// platform-audit adoption: P-2 and P-31 enqueue their AL-5 entries on the
// business transaction through port.AuditRecorder (injected by TxRunner),
// with keys from the natural key of the change and OccurredAt from the row.
package unit_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type captureRecorder struct {
	entries []port.AuditEntry
	err     error
}

func (r *captureRecorder) Record(_ context.Context, e port.AuditEntry) error {
	if r.err != nil {
		return r.err
	}
	r.entries = append(r.entries, e)
	return nil
}

func (r *captureRecorder) RecordBatch(_ context.Context, es []port.AuditEntry) error {
	if r.err != nil {
		return r.err
	}
	r.entries = append(r.entries, es...)
	return nil
}

var _ port.AuditRecorder = (*captureRecorder)(nil)

// auditTxRunner mirrors postgres.TxRunner: it injects the publisher and the
// recorder, and reports whether fn's error would have rolled back.
type auditTxRunner struct {
	pub      port.EventPublisher
	rec      port.AuditRecorder
	runs     int
	rollback bool
}

func (r *auditTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	r.runs++
	if r.pub != nil {
		ctx = port.WithEventPublisher(ctx, r.pub)
	}
	if r.rec != nil {
		ctx = port.WithAuditRecorder(ctx, r.rec)
	}
	err := fn(ctx)
	r.rollback = err != nil
	return err
}

func TestAudit_TenantPatch_RecordsSettingChangeInTx(t *testing.T) {
	tenant := evTenant()
	tenant.RecordVersion = 7
	tenant.UpdatedAt = time.Date(2026, 11, 17, 8, 30, 0, 0, time.UTC)
	rec := &captureRecorder{}
	tx := &auditTxRunner{pub: &capturePublisher{}, rec: rec}
	svc := newEvService(&evTenantRepo{tenant: tenant}, newTSCache(), tx)

	actor := uuid.New()
	ctx := requestctx.WithContext(context.Background(), &requestctx.RequestContext{
		UserID: actor, TenantID: tenant.ID, ClientIP: "10.1.2.3", UserAgent: "browser",
	})
	name := "Acme Two"
	_, _, err := svc.Patch(ctx, tenant.ID, &domain.TenantPatch{Name: &name, RecordVersion: 6})
	require.NoError(t, err)

	require.Equal(t, 1, tx.runs, "the update and the audit entry share one transaction")
	require.Len(t, rec.entries, 1)
	e := rec.entries[0]
	assert.Equal(t, port.AuditEntryTenantSettingChanged, e.EntryType)
	assert.Equal(t, fmt.Sprintf("config.tenant_setting.changed:%s:7", tenant.ID), e.IdempotencyKey,
		"key = tenant + record_version after the update")
	assert.Equal(t, tenant.ID, e.TenantID)
	assert.Equal(t, "update", e.Action)
	assert.Equal(t, port.UserAuditActor(actor), e.Actor)
	assert.Equal(t, tenant.UpdatedAt, e.OccurredAt, "OccurredAt comes from the row, not the clock")
	assert.Equal(t, "10.1.2.3", e.IPAddress)
	assert.Equal(t, "browser", e.UserAgent)
	assert.Equal(t, map[string]any{"name": name}, e.Metadata)
}

func TestAudit_TenantPatch_ResidencyModeCarriesOldAndNew(t *testing.T) {
	tenant := evTenant()
	tenant.RecordVersion = 2 // the fake Update returns the stored version: 1 → 2
	rec := &captureRecorder{}
	svc := newEvService(&evTenantRepo{tenant: tenant}, newTSCache(), &auditTxRunner{pub: &capturePublisher{}, rec: rec})
	strict := domain.ResidencyModeStrict
	_, _, err := svc.Patch(context.Background(), tenant.ID, &domain.TenantPatch{ResidencyMode: &strict, RecordVersion: 1})
	require.NoError(t, err)
	require.Len(t, rec.entries, 1)
	assert.Equal(t, map[string]any{"old": "standard", "new": "strict"}, rec.entries[0].Metadata["residency_mode"])
	assert.Equal(t, port.SystemAuditActor(), rec.entries[0].Actor, "no caller identity → iam_system …00a1, never an empty id")
}

func TestAudit_TenantPatch_RecorderErrorRollsBack(t *testing.T) {
	tenant := evTenant()
	tenant.RecordVersion = 2
	dbErr := errors.New("connection lost")
	tx := &auditTxRunner{rec: &captureRecorder{err: dbErr}}
	svc := newEvService(&evTenantRepo{tenant: tenant}, newTSCache(), tx)
	name := "x"
	_, _, err := svc.Patch(context.Background(), tenant.ID, &domain.TenantPatch{Name: &name, RecordVersion: 1})
	assert.ErrorIs(t, err, dbErr)
	assert.True(t, tx.rollback)
}

func TestAudit_TenantPatch_NoRecorderNoEntry(t *testing.T) {
	tenant := evTenant()
	svc := newEvService(&evTenantRepo{tenant: tenant}, newTSCache(), &auditTxRunner{})
	name := "x"
	_, _, err := svc.Patch(context.Background(), tenant.ID, &domain.TenantPatch{Name: &name, RecordVersion: 1})
	require.NoError(t, err)
}

func TestAudit_InvitationRevoke_RecordsInTx(t *testing.T) {
	tenantID, id, actor := uuid.New(), uuid.New(), uuid.New()
	revokedAt := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	repo := &fakeInviteRepo{
		setStatusFn: func(_ context.Context, tt, ii uuid.UUID, _ domain.InvitationStatus, ver int64) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: ii, TenantID: tt, RecordVersion: ver + 1, UpdatedAt: revokedAt,
				Email: "invitee@example.com", FullName: "Invitee"}, nil
		},
		setKCCleanupPendingFn: func(context.Context, uuid.UUID, uuid.UUID, bool, int64) error { return nil },
	}
	rec := &captureRecorder{}
	tx := &auditTxRunner{rec: rec}
	svc := service.NewInvitationService(repo, nil, nil, nil, nil, nil, nil, tx, nil, 7)
	ctx := requestctx.WithContext(context.Background(), &requestctx.RequestContext{UserID: actor, TenantID: tenantID})

	_, err := svc.Revoke(ctx, tenantID, id, 3)
	require.NoError(t, err)
	require.Equal(t, 1, tx.runs)
	require.Len(t, rec.entries, 1)
	e := rec.entries[0]
	assert.Equal(t, port.AuditEntryInvitationRevoked, e.EntryType)
	assert.Equal(t, fmt.Sprintf("invitation.revoked:%s:%s", tenantID, id), e.IdempotencyKey)
	assert.Equal(t, "revoke", e.Action)
	assert.Equal(t, port.UserAuditActor(actor), e.Actor)
	assert.Equal(t, &port.AuditTarget{Type: "invitation", ID: id.String()}, e.Target)
	assert.Equal(t, revokedAt, e.OccurredAt)
	assert.Nil(t, e.Metadata, "no invitee data (email, name) in the entry")
}

func TestAudit_InvitationRevoke_CleanupFailureRecordsNothing(t *testing.T) {
	boom := errors.New("lock conflict")
	repo := &fakeInviteRepo{
		setStatusFn: func(_ context.Context, tt, ii uuid.UUID, _ domain.InvitationStatus, ver int64) (*domain.PendingInvitation, error) {
			return &domain.PendingInvitation{ID: ii, TenantID: tt, RecordVersion: ver + 1}, nil
		},
		setKCCleanupPendingFn: func(context.Context, uuid.UUID, uuid.UUID, bool, int64) error { return boom },
	}
	rec := &captureRecorder{}
	tx := &auditTxRunner{rec: rec}
	svc := service.NewInvitationService(repo, nil, nil, nil, nil, nil, nil, tx, nil, 7)
	_, err := svc.Revoke(context.Background(), uuid.New(), uuid.New(), 1)
	assert.ErrorIs(t, err, boom)
	assert.Empty(t, rec.entries)
	assert.True(t, tx.rollback)
}

func TestAudit_TenantPatch_NoOpIsNotAudited(t *testing.T) {
	tenant := evTenant() // the fake Update keeps record_version 1: a no-op (TRG-3)
	rec := &captureRecorder{}
	svc := newEvService(&evTenantRepo{tenant: tenant}, newTSCache(), &auditTxRunner{rec: rec})
	name := tenant.Name
	_, _, err := svc.Patch(context.Background(), tenant.ID, &domain.TenantPatch{Name: &name, RecordVersion: 1})
	require.NoError(t, err)
	assert.Empty(t, rec.entries, "a no-op PATCH would reuse the previous change's key")
}
