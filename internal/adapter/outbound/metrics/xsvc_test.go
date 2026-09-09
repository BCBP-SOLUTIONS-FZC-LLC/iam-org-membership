package metrics

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

// ObserveXsvcLatency / IncXsvcError / IncMembershipExistsCheck are nil-safe
// no-ops before Register() runs (see business_test.go's TestMetricNamesStable
// family for the nil-branch coverage); this file exercises the recording
// branch once Register() has populated the package vars.

func TestObserveXsvcLatency_RecordsAfterRegister(t *testing.T) {
	ensureRegistered(t)
	ObserveXsvcLatency("catalog", "departments", 0.042)
	count := testutil.CollectAndCount(XsvcCallLatencySeconds)
	assert.Positive(t, count)
}

func TestIncXsvcError_RecordsAfterRegister(t *testing.T) {
	ensureRegistered(t)
	before := testutil.ToFloat64(XsvcCallErrors.WithLabelValues("delegation", "dept-delegate", "timeout"))
	IncXsvcError("delegation", "dept-delegate", "timeout")
	after := testutil.ToFloat64(XsvcCallErrors.WithLabelValues("delegation", "dept-delegate", "timeout"))
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

type timeoutNetError struct{}

func (timeoutNetError) Error() string   { return "i/o timeout" }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return true }

var _ net.Error = timeoutNetError{}

func TestXsvcOutcome_NetTimeoutError(t *testing.T) {
	assert.Equal(t, "timeout", XsvcOutcome(timeoutNetError{}))
}

func TestXsvcOutcome_ContextDeadlineExceeded(t *testing.T) {
	assert.Equal(t, "timeout", XsvcOutcome(context.DeadlineExceeded))
}

func TestXsvcOutcome_WrappedContextDeadlineExceeded(t *testing.T) {
	wrapped := errors.Join(errors.New("call failed"), context.DeadlineExceeded)
	assert.Equal(t, "timeout", XsvcOutcome(wrapped))
}

func TestXsvcOutcome_GenericErrorFallsBackTo5xx(t *testing.T) {
	assert.Equal(t, "5xx", XsvcOutcome(errors.New("connection refused")))
}
