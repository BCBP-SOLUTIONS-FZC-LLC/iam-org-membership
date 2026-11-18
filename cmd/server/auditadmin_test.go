package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
)

func TestRunAuditAdmin_UsageErrors(t *testing.T) {
	op := uuid.NewString()
	appDSN := "postgres://org_membership_app:x@localhost:5432/org_membership?sslmode=disable"
	for name, tc := range map[string]struct {
		args   []string
		sysDSN string
		want   string
	}{
		"no command":                 {nil, "", "usage: iam-org-membership audit-admin"},
		"help":                       {[]string{"-h"}, "", "usage: iam-org-membership audit-admin"},
		"unknown command":            {[]string{"purge"}, "", `unknown command "purge"`},
		"bad flag":                   {[]string{"list", "-nope"}, "", "flag provided but not defined"},
		"bad id":                     {[]string{"list", "-id", "nope"}, "", `-id "nope" is not a UUID`},
		"bad failed-before":          {[]string{"list", "-failed-before", "yesterday"}, "", "-failed-before must be RFC 3339"},
		"missing operator":           {[]string{"requeue", "-reason", "x", "-entry-type", "t"}, "", "-operator must be your user UUID"},
		"nil operator":               {[]string{"discard", "-operator", uuid.Nil.String(), "-reason", "x", "-entry-type", "t"}, "", "-operator must be your user UUID"},
		"missing reason":             {[]string{"resolve", "-operator", op, "-entry-type", "t"}, "", "-reason is required"},
		"no selector":                {[]string{"requeue", "-operator", op, "-reason", "x"}, "", "refusing to change every parked row"},
		"waiting on requeue":         {[]string{"requeue", "-waiting", "-operator", op, "-reason", "x", "-entry-type", "t"}, "", "-waiting applies to discard and resolve only"},
		"waiting on list":            {[]string{"list", "-waiting"}, "", "-waiting applies to discard and resolve only"},
		"waiting with failed-before": {[]string{"discard", "-waiting", "-failed-before", "2026-11-01T00:00:00Z", "-operator", op, "-reason", "x"}, "", "-failed-before applies to parked rows only"},
		"no system DSN":              {[]string{"list"}, "", "SYSTEM_DATABASE_URL is not set"},
		"no system DSN for a change": {[]string{"requeue", "-operator", op, "-reason", "x", "-id", uuid.NewString()}, "", "SYSTEM_DATABASE_URL is not set"},
		"system DSN is the app DSN":  {[]string{"list-waiting"}, appDSN, "equals the app DSN"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", appDSN)
			t.Setenv("SYSTEM_DATABASE_URL", tc.sysDSN)
			var stdout, stderr bytes.Buffer
			if code := runAuditAdmin(context.Background(), tc.args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit code %d, want 2 (stderr: %s)", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr %q does not contain %q", stderr.String(), tc.want)
			}
		})
	}
}

// An unreachable database is an operation failure (1), not a usage error.
func TestRunAuditAdmin_ConnectFailure(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://org_membership_app:x@127.0.0.1:1/org_membership?sslmode=disable")
	t.Setenv("SYSTEM_DATABASE_URL", "postgres://org_membership_migrator:x@127.0.0.1:1/org_membership?sslmode=disable&connect_timeout=1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if code := runAuditAdmin(ctx, []string{"list"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code %d, want 1 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "audit-admin:") {
		t.Fatalf("stderr %q names no error", stderr.String())
	}
}

func TestAuditAdminFilter(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	f, err := auditAdminFilter(a.String()+", "+b.String(), "invitation.created", "rejected", "2026-11-01T00:00:00Z")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.IDs) != 2 || f.IDs[0] != a || f.IDs[1] != b {
		t.Fatalf("ids = %v", f.IDs)
	}
	if f.EntryType != "invitation.created" || f.LastOutcome != "rejected" || f.FailedBefore.IsZero() {
		t.Fatalf("filter = %+v", f)
	}
	if f, err = auditAdminFilter("", "", "", ""); err != nil || f.IDs != nil {
		t.Fatalf("empty flags: %+v, %v", f, err)
	}
}

func TestPrintAuditRows(t *testing.T) {
	failed := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	rows := []auditoutbox.FailedRow{
		{ID: uuid.New(), TenantID: uuid.New(), EntryType: "invitation.created", LastOutcome: "rejected", LastError: "422 INVALID_ENTRY", Attempts: 1, FailedAt: &failed},
		{ID: uuid.New(), TenantID: uuid.New(), EntryType: "invitation.revoked", LastOutcome: "skew", Attempts: 0},
	}
	var out bytes.Buffer
	printAuditRows(&out, rows)
	for _, want := range []string{"TENANT", rows[0].TenantID.String(), rows[1].TenantID.String(), "2026-11-18T10:00:00Z", "422 INVALID_ENTRY", "2 row(s)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	printAuditRows(failingWriter{}, rows) // a broken terminal is not a panic
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }
