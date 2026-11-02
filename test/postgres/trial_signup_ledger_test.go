//go:build integration

// CR-4 trial_signup_ledger against real Postgres: the migration's own
// grants, I-1's claim (one trial per email even when two signups race, and
// a hash recorded on the replay after the backstop created the tenant), and
// the retention prune.
package postgres_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/migrate"
)

func ledgerHash(b string) string { return strings.Repeat(b, 64/len(b)) }

// The test harness grants the app role ALL TABLES, which would hide a
// missing GRANT. Revoke it, roll back to just before 000006 and re-apply,
// and check that the migration itself grants what CR-4, I-1 and the prune
// need.
func TestTrialLedger_MigrationGrantsAppRole(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	_, err := fx.rawPool.Exec(ctx, `REVOKE ALL ON trial_signup_ledger FROM org_membership_app`)
	require.NoError(t, err)

	runner := &pgmigrate.Runner{FS: loadMigrationDir(t), DSN: fx.rawPool.Config().ConnConfig.ConnString()}
	require.NoError(t, runner.Down(ctx, 2)) // 000007, then 000006
	require.NoError(t, runner.Up(ctx))

	for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
		var ok bool
		require.NoError(t, fx.rawPool.QueryRow(ctx,
			`SELECT has_table_privilege('org_membership_app', 'trial_signup_ledger', $1)`, priv).Scan(&ok))
		assert.True(t, ok, "org_membership_app must have %s", priv)
		require.NoError(t, fx.rawPool.QueryRow(ctx,
			`SELECT has_table_privilege('public', 'trial_signup_ledger', $1)`, priv).Scan(&ok))
		assert.False(t, ok, "PUBLIC must not have %s", priv)
	}
}

func TestTrialLedger_Claim(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	repo := pgadapter.NewTrialSignupLedgerRepository(fx.appPool)
	h, a, b := ledgerHash("a1"), uuid.New(), uuid.New()

	require.NoError(t, repo.ClaimEmailHash(ctx, h, a))
	require.NoError(t, repo.ClaimEmailHash(ctx, h, a), "same tenant: replay no-op")
	require.ErrorIs(t, repo.ClaimEmailHash(ctx, h, b), domain.ErrTrialAlreadyUsed)

	exists, err := repo.ExistsEmailHash(ctx, h)
	require.NoError(t, err)
	assert.True(t, exists)

	// A row from before tenant_id existed is held by "another tenant".
	legacy := ledgerHash("b2")
	_, err = fx.rawPool.Exec(ctx, `INSERT INTO trial_signup_ledger (email_hash) VALUES ($1)`, legacy)
	require.NoError(t, err)
	require.ErrorIs(t, repo.ClaimEmailHash(ctx, legacy, a), domain.ErrTrialAlreadyUsed)

	// The format CHECK rejects an upper-case or short hash at the DB too.
	_, err = fx.rawPool.Exec(ctx, `INSERT INTO trial_signup_ledger (email_hash) VALUES ('ABC')`)
	require.Error(t, err)
}

// Two signups for one email, racing past CR-4: exactly one I-1 commits.
func TestTrialLedger_ConcurrentI1_OneTrialPerEmail(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	fx.Provisioning.WithTrialLedger(pgadapter.NewTrialSignupLedgerRepository(fx.appPool))
	h := ledgerHash("c3")

	ids := []uuid.UUID{uuid.New(), uuid.New()}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = fx.Provisioning.TrialSignup(ctx, service.TrialSignupInput{
				TenantID: ids[i], Slug: "race-" + ids[i].String()[:8], Name: "Race",
				Plan: domain.PlanStarter, OwnerUserID: uuid.New(), EmailHash: h,
			})
		}(i)
	}
	wg.Wait()

	var ok, used int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case assert.ErrorIs(t, err, domain.ErrTrialAlreadyUsed):
			used++
		}
	}
	assert.Equal(t, 1, ok)
	assert.Equal(t, 1, used)
	var tenants int
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM tenants WHERE id = ANY($1)`, ids).Scan(&tenants))
	assert.Equal(t, 1, tenants, "the losing I-1 rolled back its tenant row")
}

// The backstop created the tenant without a hash; the BFF's I-1 replay
// records it, so the email cannot start another trial.
func TestTrialLedger_ReplayAfterBackstop_RecordsHash(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	repo := pgadapter.NewTrialSignupLedgerRepository(fx.appPool)
	fx.Provisioning.WithTrialLedger(repo)
	tenantID, owner, h := uuid.New(), uuid.New(), ledgerHash("d4")
	in := service.TrialSignupInput{
		TenantID: tenantID, Slug: "ledger-replay", Name: "Replay", Plan: domain.PlanStarter, OwnerUserID: owner,
	}

	_, created, err := fx.Provisioning.TrialSignup(ctx, in) // backstop: no hash
	require.NoError(t, err)
	require.True(t, created)

	in.EmailHash = h
	_, created, err = fx.Provisioning.TrialSignup(ctx, in) // BFF retry
	require.NoError(t, err)
	assert.False(t, created)

	require.ErrorIs(t, fx.Provisioning.CheckTrialEligibility(ctx, h), domain.ErrTrialAlreadyUsed)
}

func TestTrialLedger_PruneCreatedBefore(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	repo := pgadapter.NewTrialSignupLedgerRepository(fx.appPool)
	oldA, oldB, fresh := ledgerHash("e5"), ledgerHash("f6"), ledgerHash("07")
	for _, h := range []string{oldA, oldB} {
		_, err := fx.rawPool.Exec(ctx,
			`INSERT INTO trial_signup_ledger (email_hash, tenant_id, created_at) VALUES ($1, $2, now() - interval '800 days')`,
			h, uuid.New())
		require.NoError(t, err)
	}
	require.NoError(t, repo.ClaimEmailHash(ctx, fresh, uuid.New()))

	cutoff := time.Now().Add(-730 * 24 * time.Hour)
	n, err := repo.PruneCreatedBefore(ctx, cutoff, 1)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n, "batch limit honoured")
	n, err = repo.PruneCreatedBefore(ctx, cutoff, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)

	for h, want := range map[string]bool{oldA: false, oldB: false, fresh: true} {
		exists, err := repo.ExistsEmailHash(ctx, h)
		require.NoError(t, err)
		assert.Equal(t, want, exists, h)
	}
}
