//go:build integration

// 000009_outbox_sequence_grants: the runtime roles can draw from
// platform-events' outbox_events_ordering_seq when they do not own it.
// Every keyed enqueue (outbox.EnqueueOrdered) calls nextval() on it, so
// without the grant I-1 signup and every keyed event path answered 500
// (permission denied for sequence, SQLSTATE 42501).
package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/test/dbseed"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOutboxSequenceGrants_RuntimeRolesNotOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, rawPool, _ := setupTestDB(t) // migrations ran as the postgres superuser

	var owner string
	require.NoError(t, rawPool.QueryRow(ctx, `
		SELECT pg_get_userbyid(c.relowner) FROM pg_class c
		WHERE c.relname = 'outbox_events_ordering_seq' AND c.relkind = 'S'`).Scan(&owner))
	require.Equal(t, "postgres", owner, "precondition: neither runtime role owns the sequence")

	superDSN := rawPool.Config().ConnString()
	for _, role := range []struct{ name, password string }{
		{"org_membership_app", appRolePassword},
		{"org_membership_migrator", migratorPassword},
	} {
		dsn := strings.Replace(superDSN, "postgres:testpassword@", role.name+":"+role.password+"@", 1)
		require.NotEqual(t, superDSN, dsn)
		pool, err := dbseed.New(ctx, dsn)
		require.NoError(t, err)
		t.Cleanup(pool.Close)

		var v int64
		err = pool.QueryRow(ctx, `SELECT nextval('outbox_events_ordering_seq')`).Scan(&v)
		require.NoError(t, err, "%s must be able to draw ordering_seq (keyed enqueue)", role.name)
		assert.Positive(t, v)

		// setval is not granted: nothing in the runtime resets the sequence.
		_, err = pool.Exec(ctx, `SELECT setval('outbox_events_ordering_seq', 1)`)
		require.ErrorContains(t, err, "permission denied", "%s must not reset the sequence", role.name)
	}

	// A sequence the migrating role creates later inherits the grant.
	_, err := rawPool.Exec(ctx, `CREATE SEQUENCE public.grants_probe_seq`)
	require.NoError(t, err)
	var granted bool
	require.NoError(t, rawPool.QueryRow(ctx, `
		SELECT has_sequence_privilege('org_membership_app', 'public.grants_probe_seq', 'USAGE')
		   AND has_sequence_privilege('org_membership_migrator', 'public.grants_probe_seq', 'USAGE')`).Scan(&granted))
	assert.True(t, granted, "default privileges cover sequences created after 000009")
}
