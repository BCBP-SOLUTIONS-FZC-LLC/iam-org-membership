package port

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

// TestTenantRepositoryNoop exercises every TenantRepositoryNoop method so
// the compile-time stub (embedded by fakes elsewhere in the test suite that
// only override a subset of TenantRepository) is itself covered. All
// methods are documented no-ops returning nil/zero values.
func TestTenantRepositoryNoop(t *testing.T) {
	var repo TenantRepositoryNoop
	ctx := context.Background()
	id := uuid.New()

	if tenant, err := repo.FindByID(ctx, id); tenant != nil || err != nil {
		t.Fatalf("FindByID = (%v, %v), want (nil, nil)", tenant, err)
	}
	if tenant, err := repo.FindByIDIncludingDeleted(ctx, id); tenant != nil || err != nil {
		t.Fatalf("FindByIDIncludingDeleted = (%v, %v), want (nil, nil)", tenant, err)
	}
	if tenant, err := repo.Update(ctx, id, &domain.TenantPatch{}); tenant != nil || err != nil {
		t.Fatalf("Update = (%v, %v), want (nil, nil)", tenant, err)
	}
	if err := repo.SetRealmSyncPending(ctx, id); err != nil {
		t.Fatalf("SetRealmSyncPending = %v, want nil", err)
	}
	if tenant, created, err := repo.Insert(ctx, &domain.Tenant{}); tenant != nil || created || err != nil {
		t.Fatalf("Insert = (%v, %v, %v), want (nil, false, nil)", tenant, created, err)
	}
	if lapses, err := repo.ListSubscriptionLapses(ctx, 30); lapses != nil || err != nil {
		t.Fatalf("ListSubscriptionLapses = (%v, %v), want (nil, nil)", lapses, err)
	}
	if err := repo.LockByID(ctx, id); err != nil {
		t.Fatalf("LockByID = %v, want nil", err)
	}
	if seats, err := repo.LicensedSeatsForUpdate(ctx, id); seats != 0 || err != nil {
		t.Fatalf("LicensedSeatsForUpdate = (%v, %v), want (0, nil)", seats, err)
	}
	if err := repo.SetFeatureFlags(ctx, id, []byte("{}"), 1); err != nil {
		t.Fatalf("SetFeatureFlags = %v, want nil", err)
	}
	if err := repo.ClearOwnerlessSince(ctx, id); err != nil {
		t.Fatalf("ClearOwnerlessSince = %v, want nil", err)
	}
	if flipped, err := repo.MarkOwnerlessIfUnset(ctx, id); flipped || err != nil {
		t.Fatalf("MarkOwnerlessIfUnset = (%v, %v), want (false, nil)", flipped, err)
	}
	if err := repo.SetRealmFields(ctx, id, "realm-1", domain.RealmType("dedicated"), "shard-1", 1); err != nil {
		t.Fatalf("SetRealmFields = %v, want nil", err)
	}
	if occ, err := repo.LockSeatOccupancy(ctx, id); occ != (SeatOccupancy{}) || err != nil {
		t.Fatalf("LockSeatOccupancy = (%v, %v), want (%v, nil)", occ, err, SeatOccupancy{})
	}
	if err := repo.SetOverageSince(ctx, id, nil); err != nil {
		t.Fatalf("SetOverageSince = %v, want nil", err)
	}
	if lock, err := repo.LockForProjection(ctx, id); lock != nil || err != nil {
		t.Fatalf("LockForProjection = (%v, %v), want (nil, nil)", lock, err)
	}
	if err := repo.SetLastEventAt(ctx, id, time.Now()); err != nil {
		t.Fatalf("SetLastEventAt = %v, want nil", err)
	}
	if rows, err := repo.ApplyLifecyclePatch(ctx, id, TenantLifecyclePatch{Op: LifecycleSetRealm}); rows != 0 || err != nil {
		t.Fatalf("ApplyLifecyclePatch = (%v, %v), want (0, nil)", rows, err)
	}
	if err := repo.WipeTenantChildren(ctx, id); err != nil {
		t.Fatalf("WipeTenantChildren = %v, want nil", err)
	}

	// Compile-time interface satisfaction is also asserted in
	// tenant_repository.go; exercising it here just confirms the value
	// can be used through the interface.
	var _ TenantRepository = repo
}

// TestInvitationRepositoryNoop exercises every InvitationRepositoryNoop
// method (the subset it supplies so fakes elsewhere don't need to
// reimplement LockByID/ExpireOverdue/ClearKCCleanupPendingByID).
func TestInvitationRepositoryNoop(t *testing.T) {
	var repo InvitationRepositoryNoop
	ctx := context.Background()
	id := uuid.New()

	if inv, err := repo.LockByID(ctx, id); inv != nil || err != nil {
		t.Fatalf("LockByID = (%v, %v), want (nil, nil)", inv, err)
	}
	if count, err := repo.ExpireOverdue(ctx, 100); count != 0 || err != nil {
		t.Fatalf("ExpireOverdue = (%v, %v), want (0, nil)", count, err)
	}
	if err := repo.ClearKCCleanupPendingByID(ctx, id); err != nil {
		t.Fatalf("ClearKCCleanupPendingByID = %v, want nil", err)
	}
}

// fakeLogger records every call made through the Logger port so tests can
// assert both invocation (level routing) and the fields SlogStyleLogger
// derived from its slog-style variadic args.
type fakeLogger struct {
	calls []fakeLogCall
}

type fakeLogCall struct {
	level  string
	msg    string
	fields map[string]any
}

func (f *fakeLogger) Debug(msg string, fields map[string]any) {
	f.calls = append(f.calls, fakeLogCall{"debug", msg, fields})
}
func (f *fakeLogger) Info(msg string, fields map[string]any) {
	f.calls = append(f.calls, fakeLogCall{"info", msg, fields})
}
func (f *fakeLogger) Warn(msg string, fields map[string]any) {
	f.calls = append(f.calls, fakeLogCall{"warn", msg, fields})
}
func (f *fakeLogger) Error(msg string, fields map[string]any) {
	f.calls = append(f.calls, fakeLogCall{"error", msg, fields})
}

var _ Logger = &fakeLogger{}

// TestSlogStyleLogger_NilLogger covers the zero-value / nil-log no-op path:
// log4 must return before touching kvToFields or the underlying Logger.
func TestSlogStyleLogger_NilLogger(t *testing.T) {
	var zero SlogStyleLogger
	zero.Debug("no sink")
	zero.Info("no sink")
	zero.Warn("no sink")
	zero.Error("no sink")
	zero.DebugContext(context.Background(), "no sink")
	zero.InfoContext(context.Background(), "no sink")
	zero.WarnContext(context.Background(), "no sink")
	zero.ErrorContext(context.Background(), "no sink")

	wrapped := NewSlogStyleLogger(nil)
	wrapped.Error("still no sink", "key", "value")
	// No panics, no observable sink: nothing further to assert.
}

// TestSlogStyleLogger_Levels covers every level-routing branch (plain and
// *Context variants) plus the default branch in log4 reached only via an
// out-of-band level value.
func TestSlogStyleLogger_Levels(t *testing.T) {
	f := &fakeLogger{}
	log := NewSlogStyleLogger(f)
	ctx := context.Background()

	log.Debug("d")
	log.Info("i")
	log.Warn("w")
	log.Error("e")
	log.DebugContext(ctx, "dc")
	log.InfoContext(ctx, "ic")
	log.WarnContext(ctx, "wc")
	log.ErrorContext(ctx, "ec")

	wantLevels := []string{"debug", "info", "warn", "error", "debug", "info", "warn", "error"}
	if len(f.calls) != len(wantLevels) {
		t.Fatalf("got %d calls, want %d: %+v", len(f.calls), len(wantLevels), f.calls)
	}
	for i, want := range wantLevels {
		if f.calls[i].level != want {
			t.Errorf("call %d: level = %q, want %q", i, f.calls[i].level, want)
		}
	}

	// log4's default branch (falls back to Error) is unreachable through
	// the exported API — every exported method passes one of the four
	// known slog levels — so exercise it directly via the unexported
	// entry point from within the same package.
	f.calls = nil
	log.log4(ctx, slog.Level(99), "unknown-level", nil, false)
	if len(f.calls) != 1 || f.calls[0].level != "error" {
		t.Fatalf("log4 default branch = %+v, want single error call", f.calls)
	}
}

// TestSlogStyleLogger_ContextTraceID covers both the valid-span and
// no-span branches of log4's withCtx handling.
func TestSlogStyleLogger_ContextTraceID(t *testing.T) {
	f := &fakeLogger{}
	log := NewSlogStyleLogger(f)

	// No span in context: trace_id must not be attached.
	log.InfoContext(context.Background(), "no-span", "k", "v")
	if _, ok := f.calls[len(f.calls)-1].fields["trace_id"]; ok {
		t.Fatalf("fields = %+v, want no trace_id without a span", f.calls[len(f.calls)-1].fields)
	}

	// Valid span in context: trace_id must be attached alongside the
	// caller's own fields.
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	log.InfoContext(ctx, "with-span", "k", "v")
	last := f.calls[len(f.calls)-1]
	if last.fields["k"] != "v" {
		t.Errorf("fields[k] = %v, want v", last.fields["k"])
	}
	if last.fields["trace_id"] != sc.TraceID().String() {
		t.Errorf("fields[trace_id] = %v, want %v", last.fields["trace_id"], sc.TraceID().String())
	}
}

// TestKVToFields covers kvToFields' pairing, non-string-key drop, and
// trailing-unpaired-arg drop branches directly.
func TestKVToFields(t *testing.T) {
	cases := []struct {
		name string
		args []any
		want map[string]any
	}{
		{"empty", nil, map[string]any{}},
		{"paired", []any{"a", 1, "b", "two"}, map[string]any{"a": 1, "b": "two"}},
		{"trailing unpaired dropped", []any{"a", 1, "orphan"}, map[string]any{"a": 1}},
		{"non-string key dropped", []any{42, "value", "a", 1}, map[string]any{"a": 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := kvToFields(tc.args)
			if len(got) != len(tc.want) {
				t.Fatalf("kvToFields(%v) = %v, want %v", tc.args, got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("kvToFields(%v)[%q] = %v, want %v", tc.args, k, got[k], v)
				}
			}
		})
	}
}
