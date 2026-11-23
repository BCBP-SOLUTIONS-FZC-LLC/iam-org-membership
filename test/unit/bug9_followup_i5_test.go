// BUG-9 follow-up, I-5 (TM-12 L885): when Keycloak deletes the last ACTIVE
// owner, DeleteMember escalates (ownerless_since, metric, ERROR log) even if
// suspended owners remain. Uses the b9Roles/b9Members fakes from
// bug9_followup_test.go.
package unit_test

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// b9Log records the messages logged at ERROR.
type b9Log struct{ errors []string }

func (l *b9Log) Debug(string, map[string]any)       {}
func (l *b9Log) Info(string, map[string]any)        {}
func (l *b9Log) Warn(string, map[string]any)        {}
func (l *b9Log) Error(msg string, _ map[string]any) { l.errors = append(l.errors, msg) }

// ── I-5 DeleteMember (TM-12 escalation) ─────────────────────────────────

type b9I5Result struct {
	err       error
	marked    bool
	metricInc float64
	errorLogs []string
}

// b9RunI5 runs DeleteMember with the counts as seen after the cascade and
// reports whether ownerless_since was stamped, the metric delta and the ERROR logs.
func b9RunI5(t *testing.T, m *b9Members, r *b9Roles, flips bool) b9I5Result {
	t.Helper()
	require.NotNil(t, metrics.TenantOwnerlessEscalated)
	var res b9I5Result
	tenants := &invTenantRepo{markOwnerlessIfUnsetFn: func(context.Context, uuid.UUID) (bool, error) {
		res.marked = true
		return flips, nil
	}}
	log := &b9Log{}
	svc := buildDeleteMemberSvcFull(m, r, b9NoDepts(), tenants, &arTxRunner{}).WithLogger(log)
	before := testutil.ToFloat64(metrics.TenantOwnerlessEscalated.WithLabelValues("user_removed"))
	res.err = svc.DeleteMember(context.Background(), uuid.New(), uuid.New())
	res.metricInc = testutil.ToFloat64(metrics.TenantOwnerlessEscalated.WithLabelValues("user_removed")) - before
	res.errorLogs = log.errors
	return res
}

// RED before the fix: Keycloak deletes active owner A; only the suspended owner B remains.
func TestBUG9F_I5_DeleteActiveOwner_OnlySuspendedRemains_Escalates(t *testing.T) {
	res := b9RunI5(t, &b9Members{statuses: b9Statuses(b9Active)},
		&b9Roles{targetIsOwner: true, total: 1, active: []int{0}}, true)
	require.NoError(t, res.err, "TM-12: never refused")
	assert.True(t, res.marked, "ownerless_since stamped (MarkOwnerlessIfUnset)")
	assert.Equal(t, 1.0, res.metricInc, "escalation metric +1")
	assert.Equal(t, []string{"tenant_ownerless_escalation"}, res.errorLogs)
}

// Control: Keycloak deletes the suspended owner B while A is active.
func TestBUG9F_I5_DeleteSuspendedOwner_OtherActive_NoEscalation(t *testing.T) {
	res := b9RunI5(t, &b9Members{statuses: b9Statuses(b9Suspended)},
		&b9Roles{targetIsOwner: true, total: 1, active: []int{1}}, true)
	require.NoError(t, res.err)
	assert.False(t, res.marked)
	assert.Zero(t, res.metricInc)
	assert.Empty(t, res.errorLogs)
}

// Control: two active owners, one deleted.
func TestBUG9F_I5_DeleteOneOfTwoActiveOwners_NoEscalation(t *testing.T) {
	res := b9RunI5(t, &b9Members{statuses: b9Statuses(b9Active)},
		&b9Roles{targetIsOwner: true, total: 1, active: []int{1}}, true)
	require.NoError(t, res.err)
	assert.False(t, res.marked)
	assert.Zero(t, res.metricInc)
	assert.Empty(t, res.errorLogs)
}

// Control (idempotency, I5-EDGE-02): a replayed USER_DELETE for A finds no
// membership and returns success with no second stamp, metric or log; and a
// removal whose MarkOwnerlessIfUnset finds the flag already set does not page again.
func TestBUG9F_I5_Replay_NoDoubleEscalation(t *testing.T) {
	replay := b9RunI5(t, &b9Members{notFound: true},
		&b9Roles{targetIsOwner: true, total: 1, active: []int{0}}, true)
	require.NoError(t, replay.err)
	assert.False(t, replay.marked, "replay stops at the membership lookup")
	assert.Zero(t, replay.metricInc)
	assert.Empty(t, replay.errorLogs)

	already := b9RunI5(t, &b9Members{statuses: b9Statuses(b9Active)},
		&b9Roles{targetIsOwner: true, total: 0, active: []int{0}}, false)
	require.NoError(t, already.err)
	assert.True(t, already.marked)
	assert.Zero(t, already.metricInc, "flag already set: not flipped, no second page")
	assert.Empty(t, already.errorLogs)
}
