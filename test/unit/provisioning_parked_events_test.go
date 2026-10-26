// Unit tests for I-1's half of EVT-18 (CD-50): TrialSignup takes the
// tenant's provisioning lock inside its transaction, then replays the events
// the consumer parked because they arrived before the tenant existed.
package unit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type ppeLocker struct {
	port.ParkedTenantEventStore
	locked []uuid.UUID
	err    error
}

func (l *ppeLocker) LockTenantProvisioning(_ context.Context, tenantID uuid.UUID) error {
	l.locked = append(l.locked, tenantID)
	return l.err
}

type ppeReplayer struct {
	calls []uuid.UUID
	err   error
}

func (r *ppeReplayer) ReplayParked(_ context.Context, tenantID uuid.UUID) (int, error) {
	r.calls = append(r.calls, tenantID)
	return 1, r.err
}

func allSystemDepts() *ptdCatalogDepts {
	return &ptdCatalogDepts{departmentsFn: func(context.Context) ([]domain.Department, error) {
		return []domain.Department{sysDept("ENGINEERING", true)}, nil
	}}
}

func TestTrialSignup_LocksThenReplaysParkedEvents(t *testing.T) {
	svc, _ := buildTrialProvisioningSvc(allSystemDepts())
	locker, replayer := &ppeLocker{}, &ppeReplayer{}
	svc.WithParkedEvents(locker, replayer)

	in := trialSignupInput()
	_, created, err := svc.TrialSignup(context.Background(), in)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, []uuid.UUID{in.TenantID}, locker.locked, "provisioning lock taken in I-1's transaction")
	assert.Equal(t, []uuid.UUID{in.TenantID}, replayer.calls, "parked events replayed after commit")
}

// A failed replay never fails I-1: the tenant exists, and the
// parked-events-replay CronJob retries.
func TestTrialSignup_ReplayFailure_DoesNotFailI1(t *testing.T) {
	svc, _ := buildTrialProvisioningSvc(allSystemDepts())
	svc.WithParkedEvents(&ppeLocker{}, &ppeReplayer{err: errors.New("replay failed")})

	tenant, created, err := svc.TrialSignup(context.Background(), trialSignupInput())
	require.NoError(t, err)
	assert.True(t, created)
	assert.NotNil(t, tenant)
}

func TestTrialSignup_LockFailure_FailsI1WithoutReplay(t *testing.T) {
	svc, _ := buildTrialProvisioningSvc(allSystemDepts())
	boom := errors.New("lock failed")
	replayer := &ppeReplayer{}
	svc.WithParkedEvents(&ppeLocker{err: boom}, replayer)

	_, _, err := svc.TrialSignup(context.Background(), trialSignupInput())
	require.ErrorIs(t, err, boom)
	assert.Empty(t, replayer.calls)
}

func TestTrialSignup_NoParkedEventsWiring_StillWorks(t *testing.T) {
	svc, _ := buildTrialProvisioningSvc(allSystemDepts())
	_, created, err := svc.TrialSignup(context.Background(), trialSignupInput())
	require.NoError(t, err)
	assert.True(t, created)
}
