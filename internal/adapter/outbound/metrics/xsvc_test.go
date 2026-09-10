package metrics

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

// ObserveDependencyLatency / IncDependencyError / IncMembershipExistsCheck
// are nil-safe no-ops before Register() runs (see business_test.go's
// TestMetricNamesStable family for the nil-branch coverage); this file
// exercises the recording branch once Register() has populated the
// package vars.

func TestObserveDependencyLatency_RecordsAfterRegister(t *testing.T) {
	ensureRegistered(t)
	ObserveDependencyLatency("catalog", "departments", 0.042)
	count := testutil.CollectAndCount(DependencyRequestSeconds)
	assert.Positive(t, count)
}

func TestIncDependencyError_RecordsAfterRegister(t *testing.T) {
	ensureRegistered(t)
	before := testutil.ToFloat64(DependencyErrors.WithLabelValues("delegation", "dept-delegate", "timeout"))
	IncDependencyError("delegation", "dept-delegate", "timeout")
	after := testutil.ToFloat64(DependencyErrors.WithLabelValues("delegation", "dept-delegate", "timeout"))
	assert.Equal(t, before+1, after)
}

func TestIncMembershipExistsCheck_RecordsAfterRegister(t *testing.T) {
	ensureRegistered(t)
	before := testutil.ToFloat64(MembershipExistsCheck.WithLabelValues("tender-acl", "found"))
	IncMembershipExistsCheck("tender-acl", "found")
	after := testutil.ToFloat64(MembershipExistsCheck.WithLabelValues("tender-acl", "found"))
	assert.Equal(t, before+1, after)
}

func TestIncRealmSyncFailed_RecordsAfterRegister(t *testing.T) {
	ensureRegistered(t)
	before := testutil.ToFloat64(RealmSyncFailed.WithLabelValues("patch_realm_config"))
	IncRealmSyncFailed("patch_realm_config")
	after := testutil.ToFloat64(RealmSyncFailed.WithLabelValues("patch_realm_config"))
	assert.Equal(t, before+1, after)
}

// Recorder is the jobs.Metrics seam cmd/reconciler/main.go wires into
// jobs.Context — verify it actually delegates to the package-level counter
// rather than silently no-oping.
func TestRecorder_IncRealmSyncFailed_RecordsAfterRegister(t *testing.T) {
	ensureRegistered(t)
	before := testutil.ToFloat64(RealmSyncFailed.WithLabelValues("clear_marker"))
	Recorder{}.IncRealmSyncFailed("clear_marker")
	after := testutil.ToFloat64(RealmSyncFailed.WithLabelValues("clear_marker"))
	assert.Equal(t, before+1, after)
}

// IncMessagesReceived/Processed/Failed back platform_messages_*_total —
// verify each records against the right collector and label.
func TestIncMessages_RecordAfterRegister(t *testing.T) {
	ensureRegistered(t)

	before := testutil.ToFloat64(MessagesReceived.WithLabelValues("tenant-orgm-q"))
	IncMessagesReceived("tenant-orgm-q")
	assert.Equal(t, before+1, testutil.ToFloat64(MessagesReceived.WithLabelValues("tenant-orgm-q")))

	before = testutil.ToFloat64(MessagesProcessed.WithLabelValues("tenant-orgm-q"))
	IncMessagesProcessed("tenant-orgm-q")
	assert.Equal(t, before+1, testutil.ToFloat64(MessagesProcessed.WithLabelValues("tenant-orgm-q")))

	before = testutil.ToFloat64(MessagesFailed.WithLabelValues("billing-orgm-q"))
	IncMessagesFailed("billing-orgm-q")
	assert.Equal(t, before+1, testutil.ToFloat64(MessagesFailed.WithLabelValues("billing-orgm-q")))
}

type timeoutNetError struct{}

func (timeoutNetError) Error() string   { return "i/o timeout" }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return true }

var _ net.Error = timeoutNetError{}

func TestDependencyOutcome_NetTimeoutError(t *testing.T) {
	assert.Equal(t, "timeout", DependencyOutcome(timeoutNetError{}))
}

func TestDependencyOutcome_ContextDeadlineExceeded(t *testing.T) {
	assert.Equal(t, "timeout", DependencyOutcome(context.DeadlineExceeded))
}

func TestDependencyOutcome_WrappedContextDeadlineExceeded(t *testing.T) {
	wrapped := errors.Join(errors.New("call failed"), context.DeadlineExceeded)
	assert.Equal(t, "timeout", DependencyOutcome(wrapped))
}

func TestDependencyOutcome_GenericErrorFallsBackTo5xx(t *testing.T) {
	assert.Equal(t, "5xx", DependencyOutcome(errors.New("connection refused")))
}
