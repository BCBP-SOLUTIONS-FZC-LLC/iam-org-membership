//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/audittest"
	auditconfig "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/auditrecorder"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
)

// TestAudit_AdminTool runs the real server binary's `audit-admin` end to end
// as org_membership_migrator (BYPASSRLS, the audit runner's role) in
// PgBouncer mode, as production runs it (SystemPoolConfig forces it):
// entries Audit Log rejected in two tenants are parked and one waits on skew;
// `list` shows both tenants' rows, `requeue` replays one and enqueues its
// config.audit_delivery.requeued entry, `discard` gives up on the other and
// `resolve -waiting` closes the waiting row. Without SYSTEM_DATABASE_URL
// the tool refuses (the app role would see no rows).
func TestAudit_AdminTool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	bin := filepath.Join(t.TempDir(), "iam-org-membership")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", bin, "../../cmd/server").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	fx := buildTestFixtures(t)
	tenantA, ownerA := seedTenantWithOwner(t, ctx, fx, "audit-admin-a")
	tenantB, _ := seedTenantWithOwner(t, ctx, fx, "audit-admin-b")
	v := 700
	for _, tenant := range []uuid.UUID{tenantA, tenantB} {
		_, _, err := fx.Tenant.Patch(withSystemAndTenant(ctx, tenant), tenant, &domain.TenantPatch{MFAFreshnessSeconds: &v, RecordVersion: 1})
		require.NoError(t, err)
	}
	_, err := fx.Invitation.Invite(withSystemAndTenant(ctx, tenantA), tenantA, service.InvitationInput{Email: "admin-tool@acme.com", FullName: "A"}, ownerA)
	require.NoError(t, err)

	// Audit Log rejects the setting changes (parked) and does not know the
	// invitation type yet (skew: waits, never parks).
	srv := audittest.NewServer(t)
	srv.RespondFor(port.AuditEntryTenantSettingChanged, audittest.Response{Status: 422, Code: "INVALID_ENTRY"})
	srv.RespondFor(port.AuditEntryInvitationCreated, audittest.UnknownEntryType)
	cfg, err := auditconfig.FromLookup("", func(n string) (string, bool) {
		if n == auditconfig.EnvSourceService {
			return auditrecorder.SourceService, true
		}
		return "", false
	})
	require.NoError(t, err)
	runner, err := auditoutbox.NewRunner(cfg.RunnerConfig(fx.sysPool, srv.Client(auditrecorder.SourceService), fx.auditEnq, nil))
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner.Stop(context.Background()) })
	_, err = runner.DrainOnce(ctx)
	require.NoError(t, err)

	rowOf := func(tenant uuid.UUID, entryType string) string {
		t.Helper()
		var id string
		require.NoError(t, fx.rawPool.QueryRow(ctx, `
			SELECT id::text FROM pending_audit_entries WHERE tenant_id = $1 AND entry->>'entry_type' = $2`,
			tenant, entryType).Scan(&id))
		return id
	}
	parkedA, parkedB := rowOf(tenantA, port.AuditEntryTenantSettingChanged), rowOf(tenantB, port.AuditEntryTenantSettingChanged)
	waitingRow := rowOf(tenantA, port.AuditEntryInvitationCreated)
	statusOf := func(id string) (status, disposition string) {
		t.Helper()
		require.NoError(t, fx.rawPool.QueryRow(ctx,
			`SELECT status, coalesce(disposition, '') FROM pending_audit_entries WHERE id = $1`, id).Scan(&status, &disposition))
		return status, disposition
	}
	s, _ := statusOf(parkedA)
	require.Equal(t, "failed", s, "INVALID_ENTRY parks the row")
	s, _ = statusOf(waitingRow)
	require.Equal(t, "pending", s, "UNKNOWN_ENTRY_TYPE waits")

	superDSN := fx.rawPool.Config().ConnString()
	appDSN := strings.Replace(superDSN, "postgres:testpassword@", "org_membership_app:"+appRolePassword+"@", 1)
	migratorDSN := strings.Replace(superDSN, "postgres:testpassword@", "org_membership_migrator:"+migratorPassword+"@", 1)
	run := func(env []string, wantCode int, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, bin, append([]string{"audit-admin"}, args...)...)
		cmd.Env = append(os.Environ(), env...)
		out, runErr := cmd.CombinedOutput()
		code := 0
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			code = exitErr.ExitCode()
		} else if runErr != nil {
			t.Fatalf("audit-admin %v: %v", args, runErr)
		}
		require.Equal(t, wantCode, code, "audit-admin %v:\n%s", args, out)
		return string(out)
	}
	asMigrator := []string{"DATABASE_URL=" + appDSN, "SYSTEM_DATABASE_URL=" + migratorDSN}

	// The app role sees no rows outside a tenant scope: refused.
	out := run([]string{"DATABASE_URL=" + appDSN, "SYSTEM_DATABASE_URL="}, 2, "list")
	assert.Contains(t, out, "SYSTEM_DATABASE_URL is not set")

	out = run(asMigrator, 0, "list", "-entry-type", port.AuditEntryTenantSettingChanged)
	for _, want := range []string{parkedA, parkedB, tenantA.String(), tenantB.String(), "422 INVALID_ENTRY", "2 row(s)"} {
		assert.Contains(t, out, want)
	}
	out = run(asMigrator, 0, "list-waiting")
	assert.Contains(t, out, waitingRow)
	assert.Contains(t, out, "skew")
	assert.Contains(t, out, "1 row(s)")

	operator := uuid.NewString()
	out = run(asMigrator, 0, "requeue", "-id", parkedA, "-operator", operator, "-reason", "fixed upstream")
	assert.Contains(t, out, "requeue: 1 row(s)")
	s, _ = statusOf(parkedA)
	assert.Equal(t, "pending", s, "requeue puts the row back to pending")

	out = run(asMigrator, 0, "discard", "-id", parkedB, "-operator", operator, "-reason", "duplicate of a delivered entry")
	assert.Contains(t, out, "discard: 1 row(s)")
	s, d := statusOf(parkedB)
	assert.Equal(t, []string{"disposed", "discarded"}, []string{s, d})

	out = run(asMigrator, 2, "resolve", "-waiting", "-operator", operator, "-reason", "x")
	assert.Contains(t, out, "refusing to change every parked row")
	out = run(asMigrator, 0, "resolve", "-waiting", "-entry-type", port.AuditEntryInvitationCreated, "-operator", operator, "-reason", "re-emitted under a new key")
	assert.Contains(t, out, "resolve: 1 row(s)")
	s, d = statusOf(waitingRow)
	assert.Equal(t, []string{"disposed", "resolved"}, []string{s, d})

	// Each change wrote its own audited entry, attributed to the operator.
	for _, et := range []string{auditoutbox.EntryTypeRequeued, auditoutbox.EntryTypeDiscarded, auditoutbox.EntryTypeResolved} {
		got := queuedAudit(t, fx, et)
		require.Len(t, got, 1, et)
		assert.Equal(t, map[string]string{"type": "user", "id": operator}, got[0].Entry.Actor, et)
		assert.Equal(t, auditrecorder.SourceService, got[0].Entry.SourceService, et)
	}
}
