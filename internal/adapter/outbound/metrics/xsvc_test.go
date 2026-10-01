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
	count := testutil.CollectAndCount(DependencyCallDurationSeconds)
	assert.Positive(t, count)
}

func TestIncDependencyError_RecordsAfterRegister(t *testing.T) {
	ensureRegistered(t)
	before := testutil.ToFloat64(DependencyCallFailuresTotal.WithLabelValues("delegation", "dept-delegate", "timeout"))
	IncDependencyError("delegation", "dept-delegate", "timeout")
	after := testutil.ToFloat64(DependencyCallFailuresTotal.WithLabelValues("delegation", "dept-delegate", "timeout"))
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

// IncDLQ / IncDuplicate back the registry-shaped platform_dlq_messages_
// total{operation,event_type,reason} and platform_duplicate_messages_total
// {queue,event_type} (queue from WithQueue).
func TestIncDLQAndDuplicate_RecordAfterRegister(t *testing.T) {
	ensureRegistered(t)

	before := testutil.ToFloat64(DLQMessages.WithLabelValues("consume", "TenantSuspended", "future_time_clamp"))
	IncDLQ("TenantSuspended", "future_time_clamp")
	assert.Equal(t, before+1, testutil.ToFloat64(DLQMessages.WithLabelValues("consume", "TenantSuspended", "future_time_clamp")))

	ctx := WithQueue(context.Background(), "tenant-orgm-q")
	before = testutil.ToFloat64(DuplicateMessages.WithLabelValues("tenant-orgm-q", "TenantSuspended"))
	tier3 := testutil.ToFloat64(ProcessedEventsDuplicatesTotal.WithLabelValues("membership"))
	IncDuplicate(ctx, "membership", "TenantSuspended")
	assert.Equal(t, before+1, testutil.ToFloat64(DuplicateMessages.WithLabelValues("tenant-orgm-q", "TenantSuspended")))
	assert.Equal(t, tier3+1, testutil.ToFloat64(ProcessedEventsDuplicatesTotal.WithLabelValues("membership")))

	assert.Equal(t, "unknown", QueueFromContext(context.Background()))
	IncDuplicate(context.Background(), "membership", "")
	assert.Equal(t, 1.0, testutil.ToFloat64(DuplicateMessages.WithLabelValues("unknown", "unknown")))
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
