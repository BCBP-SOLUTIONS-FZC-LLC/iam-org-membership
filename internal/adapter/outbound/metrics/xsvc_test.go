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
	before := testutil.ToFloat64(DependencyCallFailuresTotal.WithLabelValues("delegation", "dept_delegate", "timeout"))
	IncDependencyError("delegation", "dept-delegate", "timeout")
	after := testutil.ToFloat64(DependencyCallFailuresTotal.WithLabelValues("delegation", "dept_delegate", "timeout"))
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

// IncConsumedEventRejected backs iam_org_membership_consumed_event_rejected_
// total{event_type,reason}; IncDuplicate the Tier-3
// iam_org_membership_processed_events_duplicates_total{consumer}.
// The service no longer writes platform_dlq_messages_total itself:
// platform-events counts each DLQ forward exactly once.
func TestIncConsumedEventRejectedAndDuplicate_RecordAfterRegister(t *testing.T) {
	ensureRegistered(t)

	before := testutil.ToFloat64(ConsumedEventRejected.WithLabelValues("TenantSuspended", "future_time_clamp"))
	dlqBefore := testutil.ToFloat64(DLQMessages.WithLabelValues("consume", "TenantSuspended", "future_time_clamp"))
	IncConsumedEventRejected("TenantSuspended", "future_time_clamp")
	assert.Equal(t, before+1, testutil.ToFloat64(ConsumedEventRejected.WithLabelValues("TenantSuspended", "future_time_clamp")))
	assert.Equal(t, dlqBefore, testutil.ToFloat64(DLQMessages.WithLabelValues("consume", "TenantSuspended", "future_time_clamp")),
		"a reject must not also be counted on the shared DLQ collector")
	IncConsumedEventRejected("", "schema_violation")
	assert.Equal(t, 1.0, testutil.ToFloat64(ConsumedEventRejected.WithLabelValues("unknown", "schema_violation")))

	// IncDuplicate records only the Tier-3 counter: platform-events' inbox
	// records platform_duplicate_messages_total for the same duplicate.
	dupBefore := testutil.ToFloat64(DuplicateMessages.WithLabelValues("tenant-orgm-q", "TenantSuspended"))
	tier3 := testutil.ToFloat64(ProcessedEventsDuplicatesTotal.WithLabelValues("membership"))
	IncDuplicate(context.Background(), "membership", "TenantSuspended")
	assert.Equal(t, tier3+1, testutil.ToFloat64(ProcessedEventsDuplicatesTotal.WithLabelValues("membership")))
	assert.Equal(t, dupBefore, testutil.ToFloat64(DuplicateMessages.WithLabelValues("tenant-orgm-q", "TenantSuspended")),
		"the Tier-1 duplicate counter is the library's, not counted twice")
}

type timeoutNetError struct{}

func (timeoutNetError) Error() string   { return "i/o timeout" }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return true }

var _ net.Error = timeoutNetError{}

func TestDependencyErrorReason_NetTimeoutError(t *testing.T) {
	assert.Equal(t, "timeout", DependencyErrorReason(timeoutNetError{}))
}

func TestDependencyErrorReason_ContextDeadlineExceeded(t *testing.T) {
	assert.Equal(t, "timeout", DependencyErrorReason(context.DeadlineExceeded))
}

func TestDependencyErrorReason_WrappedContextDeadlineExceeded(t *testing.T) {
	wrapped := errors.Join(errors.New("call failed"), context.DeadlineExceeded)
	assert.Equal(t, "timeout", DependencyErrorReason(wrapped))
}

func TestDependencyErrorReason_GenericErrorIsTransport(t *testing.T) {
	assert.Equal(t, "transport", DependencyErrorReason(errors.New("connection refused")))
}

func TestHTTPStatusReason_MapsStatusClasses(t *testing.T) {
	for status, want := range map[int]string{400: "http_4xx", 404: "http_4xx", 499: "http_4xx", 500: "http_5xx", 503: "http_5xx", 599: "http_5xx", 302: "http_other", 600: "http_other"} {
		assert.Equal(t, want, HTTPStatusReason(status), "status %d", status)
	}
}

// iam_rls_violations_total is built from the registry, whose violation_type
// is a fixed set: anything else is recorded as "other".
func TestAddRLSViolations_MapsUnknownTypeToOther(t *testing.T) {
	ensureRegistered(t)
	before := testutil.ToFloat64(RLSViolations.WithLabelValues("other"))
	AddRLSViolations("something_new", 2)
	assert.Equal(t, before+2, testutil.ToFloat64(RLSViolations.WithLabelValues("other")))

	cross := testutil.ToFloat64(RLSViolations.WithLabelValues("cross_tenant_access"))
	AddRLSViolations("cross_tenant_access", 1)
	assert.Equal(t, cross+1, testutil.ToFloat64(RLSViolations.WithLabelValues("cross_tenant_access")))
}
