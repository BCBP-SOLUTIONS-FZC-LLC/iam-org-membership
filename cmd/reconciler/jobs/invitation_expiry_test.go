package jobs

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// expiryRepo implements only ExpireOverdue; the embedded nil interface
// panics on any other call.
type expiryRepo struct {
	port.InvitationRepository
	rows []domain.ExpiredInvitation
	err  error
	ctx  context.Context
}

func (r *expiryRepo) ExpireOverdue(ctx context.Context, _ int) ([]domain.ExpiredInvitation, error) {
	r.ctx = ctx
	return r.rows, r.err
}

type batchRecorder struct {
	batches [][]port.AuditEntry
	err     error
}

func (r *batchRecorder) Record(context.Context, port.AuditEntry) error {
	return errors.New("unexpected")
}
func (r *batchRecorder) RecordBatch(_ context.Context, es []port.AuditEntry) error {
	r.batches = append(r.batches, es)
	return r.err
}

type recTxRunner struct {
	rec  port.AuditRecorder
	runs int
}

func (r *recTxRunner) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	r.runs++
	return fn(port.WithAuditRecorder(ctx, r.rec))
}

func TestInvitationExpiry_AuditsEachExpiredInvitationInTheSweepTx(t *testing.T) {
	tenant := uuid.New()
	exp := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	rows := []domain.ExpiredInvitation{{ID: uuid.New(), TenantID: tenant, ExpiresAt: exp}, {ID: uuid.New(), TenantID: uuid.New(), ExpiresAt: exp.Add(time.Minute)}}
	rec := &batchRecorder{}
	tx := &recTxRunner{rec: rec}
	repo := &expiryRepo{rows: rows}

	res, err := InvitationExpiry(context.Background(), &Context{Invitations: repo, SysTxRunner: tx, Logger: pruneLogger(), BatchLimit: 10})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Attempted)
	assert.Equal(t, 2, res.Succeeded)
	assert.Equal(t, 1, tx.runs)
	_, inTx := port.AuditRecorderFromContext(repo.ctx)
	assert.True(t, inTx, "ExpireOverdue runs on the sweep transaction")

	require.Len(t, rec.batches, 1)
	require.Len(t, rec.batches[0], 2)
	for i, e := range rec.batches[0] {
		assert.Equal(t, port.AuditEntryInvitationExpired, e.EntryType)
		assert.Equal(t, fmt.Sprintf("invitation.expired:%s:%s", rows[i].TenantID, rows[i].ID), e.IdempotencyKey)
		assert.Equal(t, rows[i].TenantID, e.TenantID, "per-tenant entry, never uuid.Nil")
		assert.Equal(t, "expire", e.Action)
		assert.Equal(t, port.SystemAuditActor(), e.Actor)
		assert.Equal(t, &port.AuditTarget{Type: "invitation", ID: rows[i].ID.String()}, e.Target)
		assert.Equal(t, rows[i].ExpiresAt, e.OccurredAt, "OccurredAt = expires_at")
		assert.Equal(t, "InvitationExpired", e.SourceEventType)
	}
}

func TestInvitationExpiry_NothingExpiredRecordsNothing(t *testing.T) {
	rec := &batchRecorder{}
	res, err := InvitationExpiry(context.Background(), &Context{Invitations: &expiryRepo{}, SysTxRunner: &recTxRunner{rec: rec}, Logger: pruneLogger()})
	require.NoError(t, err)
	assert.Zero(t, res.Attempted)
	assert.Empty(t, rec.batches)
}

func TestInvitationExpiry_ErrorsFailTheRun(t *testing.T) {
	repoErr := errors.New("db down")
	_, err := InvitationExpiry(context.Background(), &Context{Invitations: &expiryRepo{err: repoErr}, SysTxRunner: &recTxRunner{rec: &batchRecorder{}}, Logger: pruneLogger()})
	assert.ErrorIs(t, err, repoErr)

	recErr := errors.New("enqueue failed")
	rows := []domain.ExpiredInvitation{{ID: uuid.New(), TenantID: uuid.New(), ExpiresAt: time.Now()}}
	_, err = InvitationExpiry(context.Background(), &Context{Invitations: &expiryRepo{rows: rows}, SysTxRunner: &recTxRunner{rec: &batchRecorder{err: recErr}}, Logger: pruneLogger()})
	assert.ErrorIs(t, err, recErr, "an enqueue failure rolls the expiry back")
}

func TestInvitationExpiry_WithoutTxRunnerRunsInline(t *testing.T) {
	rows := []domain.ExpiredInvitation{{ID: uuid.New(), TenantID: uuid.New(), ExpiresAt: time.Now()}}
	res, err := InvitationExpiry(context.Background(), &Context{Invitations: &expiryRepo{rows: rows}, Logger: pruneLogger()})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Succeeded)
}
