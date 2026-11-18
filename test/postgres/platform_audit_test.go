//go:build integration

// platform-audit adoption (outbox mode): every audited action enqueues its
// AL-5 entry into pending_audit_entries on the business transaction, so an
// entry exists if and only if the change committed (PA-INV-3); the runner
// on the BYPASSRLS sysPool delivers it.
package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/audittest"
	auditconfig "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/cmd/reconciler/jobs"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/auditrecorder"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
)

type queuedEntry struct {
	TenantID uuid.UUID
	Key      string
	Status   string
	Entry    struct {
		TenantID        uuid.UUID         `json:"tenant_id"`
		EntryType       string            `json:"entry_type"`
		Action          string            `json:"action"`
		Actor           map[string]string `json:"actor"`
		Target          map[string]string `json:"target"`
		OccurredAt      time.Time         `json:"occurred_at"`
		SourceService   string            `json:"source_service"`
		SourceEventType string            `json:"source_event_type"`
		Metadata        map[string]any    `json:"metadata"`
	}
}

// queuedAudit reads the outbox as the superuser (no RLS), filtered by type.
func queuedAudit(t *testing.T, fx *testFixtures, entryType string) []queuedEntry {
	t.Helper()
	rows, err := fx.rawPool.Query(context.Background(), `
		SELECT tenant_id, idempotency_key, status, entry FROM pending_audit_entries
		WHERE entry->>'entry_type' = $1 ORDER BY created_at, idempotency_key`, entryType)
	require.NoError(t, err)
	defer rows.Close()
	var out []queuedEntry
	for rows.Next() {
		var q queuedEntry
		var raw []byte
		require.NoError(t, rows.Scan(&q.TenantID, &q.Key, &q.Status, &raw))
		require.NoError(t, json.Unmarshal(raw, &q.Entry))
		out = append(out, q)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestAudit_SchemaHasRLSAndGrants(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	var rls, force bool
	require.NoError(t, fx.rawPool.QueryRow(ctx, `
		SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = 'public.pending_audit_entries'::regclass`).Scan(&rls, &force))
	assert.True(t, rls)
	assert.True(t, force)

	var version int64
	require.NoError(t, fx.rawPool.QueryRow(ctx, `SELECT version FROM platform_audit_schema_migrations`).Scan(&version))
	assert.GreaterOrEqual(t, version, int64(1), "the library tracks its own schema version")

	// The startup check cmd/server runs: the app role can INSERT.
	require.NoError(t, auditoutbox.CheckEnqueueAccess(ctx, fx.appPool))
}

func TestAudit_P2_TenantPatchEnqueuesOnCommit(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "audit-p2")
	tctx := withSystemAndTenant(ctx, tenantID)

	v := 600
	updated, _, err := fx.Tenant.Patch(tctx, tenantID, &domain.TenantPatch{MFAFreshnessSeconds: &v, RecordVersion: 1})
	require.NoError(t, err)

	got := queuedAudit(t, fx, port.AuditEntryTenantSettingChanged)
	require.Len(t, got, 1)
	q := got[0]
	assert.Equal(t, tenantID, q.TenantID)
	assert.Equal(t, fmt.Sprintf("config.tenant_setting.changed:%s:%d", tenantID, updated.RecordVersion), q.Key)
	assert.Equal(t, "pending", q.Status)
	assert.Equal(t, "update", q.Entry.Action)
	assert.Equal(t, map[string]string{"type": "iam_system", "id": port.IAMSystemAuditActorID.String()}, q.Entry.Actor)
	assert.Equal(t, auditrecorder.SourceService, q.Entry.SourceService)
	assert.WithinDuration(t, updated.UpdatedAt, q.Entry.OccurredAt, time.Millisecond)
	assert.EqualValues(t, 600, q.Entry.Metadata["mfa_freshness_seconds"])

	// A stale record_version conflicts and rolls back: no entry.
	_, _, err = fx.Tenant.Patch(tctx, tenantID, &domain.TenantPatch{MFAFreshnessSeconds: &v, RecordVersion: 1})
	require.Error(t, err)
	assert.Len(t, queuedAudit(t, fx, port.AuditEntryTenantSettingChanged), 1, "a rolled-back update leaves no entry")

	// Setting the current value again does not bump record_version (TRG-3)
	// and is not audited: its key would be the previous change's.
	_, _, err = fx.Tenant.Patch(tctx, tenantID, &domain.TenantPatch{MFAFreshnessSeconds: &v, RecordVersion: updated.RecordVersion})
	require.NoError(t, err)
	assert.Len(t, queuedAudit(t, fx, port.AuditEntryTenantSettingChanged), 1, "a no-op PATCH is not audited")
}

func TestAudit_ExpirySweepSkipsInvitationRevokedMeanwhile(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerID := seedTenantWithOwner(t, ctx, fx, "audit-exp-race")
	var id uuid.UUID
	require.NoError(t, fx.rawPool.QueryRow(ctx, `
		INSERT INTO pending_invitations (id, tenant_id, email, full_name, invited_by, status, expires_at)
		VALUES (gen_random_uuid(), $1, 'r@x.com', 'R', $2, 'pending', now() + interval '1 day') RETURNING id`,
		tenantID, ownerID).Scan(&id))
	_, err := fx.rawPool.Exec(ctx, `UPDATE pending_invitations SET expires_at = now() - interval '1 hour' WHERE id = $1`, id)
	require.NoError(t, err)

	jctx := &jobs.Context{
		Invitations: pgadapter.NewInvitationRepository(fx.sysPool),
		SysTxRunner: pgadapter.NewTxRunner(fx.sysPool, nil).WithAuditRecorder(auditrecorder.New(fx.auditEnq)),
		Logger:      port.NewSlogStyleLogger(nil),
		BatchLimit:  100,
	}

	// A revoke holds the row while the sweep runs; the sweep skips it
	// instead of overwriting it.
	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- fx.rawPool.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE pending_invitations SET status = 'revoked' WHERE id = $1`, id); err != nil {
				close(locked)
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	res, err := jobs.InvitationExpiry(ctx, jctx)
	require.NoError(t, err)
	assert.Zero(t, res.Succeeded)
	close(release)
	require.NoError(t, <-done)

	res, err = jobs.InvitationExpiry(ctx, jctx)
	require.NoError(t, err)
	assert.Zero(t, res.Succeeded, "a revoked invitation is never expired")
	assert.Empty(t, queuedAudit(t, fx, port.AuditEntryInvitationExpired))
	var status string
	require.NoError(t, fx.rawPool.QueryRow(ctx, `SELECT status FROM pending_invitations WHERE id = $1`, id).Scan(&status))
	assert.Equal(t, "revoked", status)
}

func TestAudit_P6_P31_InviteAndRevokeEnqueueOnCommit(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerID := seedTenantWithOwner(t, ctx, fx, "audit-p6")
	tctx := withSystemAndTenant(ctx, tenantID)

	inv, err := fx.Invitation.Invite(tctx, tenantID, service.InvitationInput{
		Email: "invitee-audit@acme.com", FullName: "Invitee Audit",
	}, ownerID)
	require.NoError(t, err)

	created := queuedAudit(t, fx, port.AuditEntryInvitationCreated)
	require.Len(t, created, 1)
	c := created[0]
	assert.Equal(t, fmt.Sprintf("invitation.created:%s:%s", tenantID, inv.ID), c.Key)
	assert.Equal(t, map[string]string{"type": "user", "id": ownerID.String()}, c.Entry.Actor)
	assert.Equal(t, map[string]string{"type": "invitation", "id": inv.ID.String()}, c.Entry.Target)
	assert.WithinDuration(t, inv.CreatedAt, c.Entry.OccurredAt, time.Millisecond)
	assert.Nil(t, c.Entry.Metadata, "no invitee email or name in the entry")

	revoked, err := fx.Invitation.Revoke(tctx, tenantID, inv.ID, inv.RecordVersion)
	require.NoError(t, err)
	rv := queuedAudit(t, fx, port.AuditEntryInvitationRevoked)
	require.Len(t, rv, 1)
	assert.Equal(t, fmt.Sprintf("invitation.revoked:%s:%s", tenantID, inv.ID), rv[0].Key)
	assert.Equal(t, "revoke", rv[0].Entry.Action)
	assert.WithinDuration(t, revoked.UpdatedAt, rv[0].Entry.OccurredAt, time.Millisecond)

	// A second revoke conflicts (record_version moved on): no new entry.
	_, err = fx.Invitation.Revoke(tctx, tenantID, inv.ID, inv.RecordVersion)
	require.Error(t, err)
	assert.Len(t, queuedAudit(t, fx, port.AuditEntryInvitationRevoked), 1)
}

func TestAudit_RLS_TenantCannotSeeAnotherTenantsEntries(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantA, _ := seedTenantWithOwner(t, ctx, fx, "audit-rls-a")
	tenantB, _ := seedTenantWithOwner(t, ctx, fx, "audit-rls-b")
	v := 900
	_, _, err := fx.Tenant.Patch(withSystemAndTenant(ctx, tenantA), tenantA, &domain.TenantPatch{MFAFreshnessSeconds: &v, RecordVersion: 1})
	require.NoError(t, err)

	countAs := func(tenant uuid.UUID) int {
		var n int
		require.NoError(t, pgadapter.NewTxRunner(fx.appPool, nil).RunInTx(withSystemAndTenant(ctx, tenant), func(txCtx context.Context) error {
			tx, _ := port.TxFromContext(txCtx)
			return tx.QueryRow(txCtx, `SELECT count(*) FROM pending_audit_entries`).Scan(&n)
		}))
		return n
	}
	assert.Equal(t, 1, countAs(tenantA))
	assert.Equal(t, 0, countAs(tenantB), "tenant_isolation hides tenant A's audit entries")
}

func TestAudit_InvitationExpirySweepEnqueuesPerInvitation(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantA, ownerA := seedTenantWithOwner(t, ctx, fx, "audit-exp-a")
	tenantB, ownerB := seedTenantWithOwner(t, ctx, fx, "audit-exp-b")
	// expires_at must be in the future at insert time; backdate afterwards,
	// as the other expiry tests do.
	seed := func(tenant, owner uuid.UUID, email string, ago time.Duration) (uuid.UUID, time.Time) {
		var id uuid.UUID
		require.NoError(t, fx.rawPool.QueryRow(ctx, `
			INSERT INTO pending_invitations (id, tenant_id, email, full_name, invited_by, status, expires_at)
			VALUES (gen_random_uuid(), $1, $2, 'X', $3, 'pending', now() + interval '1 day') RETURNING id`,
			tenant, email, owner).Scan(&id))
		var exp time.Time
		require.NoError(t, fx.rawPool.QueryRow(ctx,
			`UPDATE pending_invitations SET expires_at = now() - $2::interval WHERE id = $1 RETURNING expires_at`,
			id, fmt.Sprintf("%d seconds", int(ago.Seconds()))).Scan(&exp))
		return id, exp
	}
	idA, expA := seed(tenantA, ownerA, "a@x.com", 2*time.Hour)
	idB, expB := seed(tenantB, ownerB, "b@x.com", time.Hour)

	jctx := &jobs.Context{
		Invitations: pgadapter.NewInvitationRepository(fx.sysPool),
		SysTxRunner: pgadapter.NewTxRunner(fx.sysPool, nil).WithAuditRecorder(auditrecorder.New(fx.auditEnq)),
		Logger:      port.NewSlogStyleLogger(nil),
		BatchLimit:  100,
	}
	res, err := jobs.InvitationExpiry(ctx, jctx)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Succeeded)

	got := queuedAudit(t, fx, port.AuditEntryInvitationExpired)
	require.Len(t, got, 2)
	byKey := map[string]queuedEntry{}
	for _, q := range got {
		byKey[q.Key] = q
	}
	for _, c := range []struct {
		tenant, id uuid.UUID
		exp        time.Time
	}{{tenantA, idA, expA}, {tenantB, idB, expB}} {
		q, ok := byKey[fmt.Sprintf("invitation.expired:%s:%s", c.tenant, c.id)]
		require.True(t, ok)
		assert.Equal(t, c.tenant, q.TenantID, "one entry per tenant's invitation, never uuid.Nil")
		assert.Equal(t, map[string]string{"type": "iam_system", "id": port.IAMSystemAuditActorID.String()}, q.Entry.Actor)
		assert.Equal(t, map[string]string{"type": "invitation", "id": c.id.String()}, q.Entry.Target)
		assert.True(t, c.exp.Equal(q.Entry.OccurredAt), "OccurredAt = expires_at")
	}

	// Re-running finds nothing more to expire and enqueues nothing.
	_, err = jobs.InvitationExpiry(ctx, jctx)
	require.NoError(t, err)
	assert.Len(t, queuedAudit(t, fx, port.AuditEntryInvitationExpired), 2)
}

// TestAudit_InvitationExpirySweepOnPgBouncerModePool runs the sweep on a
// PgBouncer-mode sysPool, as production does (SystemPoolConfig forces
// PGBouncerMode). Its EnqueueBatch passes uuid arrays, which platform-audit
// v0.1.0 could not encode on the simple protocol ("unable to encode
// []uuid.UUID"), so on v0.1.0 every production sweep failed; v0.1.1 fixes
// it. setupTestDB's sysPool is a direct connection and never showed it.
func TestAudit_InvitationExpirySweepOnPgBouncerModePool(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	bouncerSys, err := newPoolWithRetry(ctx, pgcommon.Config{DSN: fx.rawPool.Config().ConnString(), PGBouncerMode: true})
	require.NoError(t, err)
	t.Cleanup(bouncerSys.Close)

	tenant, owner := seedTenantWithOwner(t, ctx, fx, "audit-exp-bouncer")
	var id uuid.UUID
	require.NoError(t, fx.rawPool.QueryRow(ctx, `
		INSERT INTO pending_invitations (id, tenant_id, email, full_name, invited_by, status, expires_at)
		VALUES (gen_random_uuid(), $1, 'bouncer@x.com', 'X', $2, 'pending', now() + interval '1 day') RETURNING id`,
		tenant, owner).Scan(&id))
	_, err = fx.rawPool.Exec(ctx, `UPDATE pending_invitations SET expires_at = now() - interval '1 hour' WHERE id = $1`, id)
	require.NoError(t, err)

	jctx := &jobs.Context{
		Invitations: pgadapter.NewInvitationRepository(bouncerSys),
		SysTxRunner: pgadapter.NewTxRunner(bouncerSys, nil).WithAuditRecorder(auditrecorder.New(fx.auditEnq)),
		Logger:      port.NewSlogStyleLogger(nil),
		BatchLimit:  100,
	}
	res, err := jobs.InvitationExpiry(ctx, jctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Succeeded)
	found := false
	for _, q := range queuedAudit(t, fx, port.AuditEntryInvitationExpired) {
		if q.Key == fmt.Sprintf("invitation.expired:%s:%s", tenant, id) {
			found = true
		}
	}
	assert.True(t, found, "the PgBouncer-mode sweep enqueued its invitation.expired entry")
}

func TestAudit_RunnerDeliversToAuditLog(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerID := seedTenantWithOwner(t, ctx, fx, "audit-deliver")
	tctx := withSystemAndTenant(ctx, tenantID)
	_, err := fx.Invitation.Invite(tctx, tenantID, service.InvitationInput{Email: "deliver@acme.com", FullName: "D"}, ownerID)
	require.NoError(t, err)
	v := 800
	_, _, err = fx.Tenant.Patch(tctx, tenantID, &domain.TenantPatch{MFAFreshnessSeconds: &v, RecordVersion: 1})
	require.NoError(t, err)

	srv := audittest.NewServer(t)
	cfg, err := auditconfig.FromLookup("", func(n string) (string, bool) {
		if n == auditconfig.EnvSourceService {
			return auditrecorder.SourceService, true
		}
		return "", false
	})
	require.NoError(t, err)
	runner, err := auditoutbox.NewRunner(cfg.RunnerConfig(fx.sysPool, srv.Client(auditrecorder.SourceService), fx.auditEnq, nil))
	require.NoError(t, err, "the sysPool (BYPASSRLS) passes the cross-tenant access check")
	t.Cleanup(func() { _ = runner.Stop(context.Background()) })

	res, err := runner.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Delivered)
	srv.AssertRecorded(t, audittest.Match{EntryType: port.AuditEntryInvitationCreated, TenantID: tenantID, Action: "create"})
	srv.AssertRecorded(t, audittest.Match{EntryType: port.AuditEntryTenantSettingChanged, TenantID: tenantID, Action: "update"})
	srv.AssertNoDuplicates(t)
	srv.AssertStableKeys(t)

	var left int
	require.NoError(t, fx.rawPool.QueryRow(ctx, `SELECT count(*) FROM pending_audit_entries`).Scan(&left))
	assert.Zero(t, left, "delivered rows are deleted")
}

func TestAudit_O4_FeatureFlagsEnqueueOnCommit(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "audit-o4")
	operatorID := uuid.New()
	// The O-4 handler binds the path tenant as the RLS tenant (RLS-6).
	octx := requestctx.WithContext(withSystemAndTenant(ctx, tenantID),
		&requestctx.RequestContext{UserID: operatorID, Roles: []string{"platform_operator"}})

	updated, err := fx.Operator.SetFeatureFlags(octx, tenantID, map[string]any{"sso_enabled": true}, 1)
	require.NoError(t, err)

	got := queuedAudit(t, fx, port.AuditEntryTenantSettingChanged)
	require.Len(t, got, 1)
	q := got[0]
	assert.Equal(t, tenantID, q.TenantID)
	assert.Equal(t, fmt.Sprintf("config.tenant_setting.changed:%s:%d", tenantID, updated.RecordVersion), q.Key)
	assert.Equal(t, map[string]string{"type": "user", "id": operatorID.String()}, q.Entry.Actor)
	assert.Equal(t, "TenantSettingChanged", q.Entry.SourceEventType)
	assert.WithinDuration(t, updated.UpdatedAt, q.Entry.OccurredAt, time.Millisecond)
	assert.Equal(t, map[string]any{"sso_enabled": map[string]any{"change": "created", "new": true}}, q.Entry.Metadata["feature_flags"])

	// Disabling it is a second, distinct entry.
	updated2, err := fx.Operator.SetFeatureFlags(octx, tenantID, map[string]any{"sso_enabled": false}, updated.RecordVersion)
	require.NoError(t, err)
	got = queuedAudit(t, fx, port.AuditEntryTenantSettingChanged)
	require.Len(t, got, 2)
	assert.Equal(t, fmt.Sprintf("config.tenant_setting.changed:%s:%d", tenantID, updated2.RecordVersion), got[1].Key)
	assert.Equal(t, map[string]any{"sso_enabled": map[string]any{"change": "disabled", "old": true, "new": false}}, got[1].Entry.Metadata["feature_flags"])

	// A stale version conflicts and rolls back: no entry.
	_, err = fx.Operator.SetFeatureFlags(octx, tenantID, map[string]any{"sso_enabled": true}, updated.RecordVersion)
	require.Error(t, err)
	assert.Len(t, queuedAudit(t, fx, port.AuditEntryTenantSettingChanged), 2)
}
