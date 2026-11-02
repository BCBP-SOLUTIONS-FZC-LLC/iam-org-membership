package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeTrialLedger struct {
	port.TrialSignupLedgerRepository
	batches []int64
	cutoffs []time.Time
	limits  []int
	err     error
}

func (f *fakeTrialLedger) ClaimEmailHash(context.Context, string, uuid.UUID) error { return nil }

func (f *fakeTrialLedger) PruneCreatedBefore(_ context.Context, cutoff time.Time, limit int) (int64, error) {
	f.cutoffs = append(f.cutoffs, cutoff)
	f.limits = append(f.limits, limit)
	if f.err != nil {
		return 0, f.err
	}
	n := f.batches[0]
	f.batches = f.batches[1:]
	return n, nil
}

func pruneLogger() port.SlogStyleLogger { return port.NewSlogStyleLogger(&warnCapture{}) }

func TestTrialLedgerPrune_LoopsUntilShortBatch(t *testing.T) {
	ledger := &fakeTrialLedger{batches: []int64{10, 10, 3}}
	res, err := TrialLedgerPrune(context.Background(), &Context{
		TrialLedger: ledger, BatchLimit: 10, TrialLedgerRetentionDays: 30, Logger: pruneLogger(),
	})
	require.NoError(t, err)
	assert.Equal(t, 23, res.Succeeded)
	assert.Len(t, ledger.cutoffs, 3)
	assert.WithinDuration(t, time.Now().Add(-30*24*time.Hour), ledger.cutoffs[0], time.Minute)
}

func TestTrialLedgerPrune_Defaults(t *testing.T) {
	ledger := &fakeTrialLedger{batches: []int64{0}}
	_, err := TrialLedgerPrune(context.Background(), &Context{TrialLedger: ledger, Logger: pruneLogger()})
	require.NoError(t, err)
	assert.Equal(t, []int{defaultPruneBatchLimit}, ledger.limits)
	assert.WithinDuration(t, time.Now().Add(-defaultTrialLedgerRetentionDays*24*time.Hour), ledger.cutoffs[0], time.Minute)
}

func TestTrialLedgerPrune_ErrorStops(t *testing.T) {
	boom := errors.New("db down")
	_, err := TrialLedgerPrune(context.Background(), &Context{
		TrialLedger: &fakeTrialLedger{err: boom}, Logger: pruneLogger(),
	})
	require.ErrorIs(t, err, boom)
}

func TestTrialLedgerPrune_NilLedger_Skips(t *testing.T) {
	res, err := TrialLedgerPrune(context.Background(), &Context{Logger: pruneLogger()})
	require.NoError(t, err)
	assert.Zero(t, res.Succeeded)
}
