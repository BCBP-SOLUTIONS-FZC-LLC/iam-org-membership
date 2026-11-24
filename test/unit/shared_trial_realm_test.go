// SHARED_TRIAL_REALM (LLD rev 2.64): the shared trial realm name is
// configured, defaulting to "trial", on the I-1 producer path.
package unit_test

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSharedTrialRealm_Resolution(t *testing.T) {
	assert.Equal(t, "trial", domain.SharedTrialRealm(""))
	assert.Equal(t, "trial", domain.SharedTrialRealm("   "))
	assert.Equal(t, "trial-eu", domain.SharedTrialRealm(" trial-eu "))
	assert.Equal(t, "trial", domain.DefaultSharedTrialRealm)
}

func TestTrialSignup_RecordsConfiguredSharedTrialRealm(t *testing.T) {
	for name, tc := range map[string]struct{ configured, want string }{
		"unset uses default": {"", "trial"},
		"configured":         {"trial-eu", "trial-eu"},
	} {
		t.Run(name, func(t *testing.T) {
			svc, _ := buildTrialProvisioningSvc(allSystemDepts())
			svc.WithSharedTrialRealm(tc.configured)
			tenant, created, err := svc.TrialSignup(context.Background(), trialSignupInput())
			require.NoError(t, err)
			require.True(t, created)
			require.NotNil(t, tenant)
			assert.Equal(t, tc.want, tenant.RealmID)
			assert.Equal(t, domain.RealmShared, tenant.RealmType)
			assert.Equal(t, domain.SharedTrialKeycloakShard, tenant.KeycloakShard, "shard stays shard-0")
		})
	}
}
