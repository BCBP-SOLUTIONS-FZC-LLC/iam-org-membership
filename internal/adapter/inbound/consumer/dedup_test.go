package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubDedup struct {
	seen    map[string]bool
	marked  []string
	err     error // returned by IsProcessed
	markErr error // returned by MarkProcessed
}

func (d *stubDedup) IsProcessed(_ context.Context, _, eventID string) (bool, error) {
	if d.err != nil {
		return false, d.err
	}
	return d.seen[eventID], nil
}

func (d *stubDedup) MarkProcessed(_ context.Context, _, eventID string) error {
	if d.markErr != nil {
		return d.markErr
	}
	d.marked = append(d.marked, eventID)
	if d.seen == nil {
		d.seen = map[string]bool{}
	}
	d.seen[eventID] = true
	return nil
}

type stubTx struct{}

func (stubTx) RunInTx(_ context.Context, fn func(context.Context) error) error {
	return fn(context.Background())
}

func TestSkipDuplicate_IncrementsMetricOnHit(t *testing.T) {
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
		ServiceName: "iam-org-membership", BuildVersion: "test",
	})
	metrics.Register("test")
	metrics.DuplicateMessages.Reset()
	metrics.ProcessedEventsDuplicatesTotal.Reset()
	dedup := &stubDedup{seen: map[string]bool{"evt-1": true}}

	hit, err := skipDuplicate(context.Background(), dedup, consumerName, "evt-1")
	require.NoError(t, err)
	assert.True(t, hit)
	assert.InDelta(t, 1, testutil.ToFloat64(metrics.DuplicateMessages.WithLabelValues(consumerName)), 0.01,
		"Tier-1 candidate platform_duplicate_messages_total must record the hit")
	assert.InDelta(t, 1, testutil.ToFloat64(metrics.ProcessedEventsDuplicatesTotal.WithLabelValues(consumerName)), 0.01,
		"Tier-3 predecessor iam_org_membership_processed_events_duplicates_total must record the same hit")

	hit, err = skipDuplicate(context.Background(), dedup, consumerName, "evt-new")
	require.NoError(t, err)
	assert.False(t, hit)
}

func TestAckUnknown_MarksProcessed(t *testing.T) {
	dedup := &stubDedup{}
	env := events.Envelope[json.RawMessage]{ID: "evt-unknown", Type: "NeverHeardOfThis"}
	err := ackUnknown(context.Background(), stubTx{}, dedup, port.NewSlogStyleLogger(nil), consumerName, env)
	require.NoError(t, err)
	assert.Equal(t, []string{"evt-unknown"}, dedup.marked)
}

// TestSkipDuplicate_IsProcessedErrorPropagates covers skipDuplicate's
// early-return branch when the IsProcessed probe itself fails — distinct
// from the hit/miss branches already covered above.
func TestSkipDuplicate_IsProcessedErrorPropagates(t *testing.T) {
	wantErr := errors.New("dedup store unavailable")
	dedup := &stubDedup{err: wantErr}

	hit, err := skipDuplicate(context.Background(), dedup, consumerName, "evt-x")
	require.ErrorIs(t, err, wantErr)
	assert.False(t, hit)
}

// TestMarkProcessedInTx_WrapsMarkProcessedError covers markProcessedInTx's
// error-wrapping branch (dedup.MarkProcessed failing inside RunInTx).
func TestMarkProcessedInTx_WrapsMarkProcessedError(t *testing.T) {
	wantErr := errors.New("insert conflict")
	dedup := &stubDedup{markErr: wantErr}

	err := markProcessedInTx(context.Background(), stubTx{}, dedup, consumerName, "evt-y")
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Contains(t, err.Error(), "mark processed")
}
