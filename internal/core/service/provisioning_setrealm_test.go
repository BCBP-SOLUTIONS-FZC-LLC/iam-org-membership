package service

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

type srTenantRepo struct {
	port.TenantRepositoryNoop
	setRealmFieldsFn func(ctx context.Context, id uuid.UUID, realmID string, realmType domain.RealmType, shard string, recordVersion int64) error
}

func (r *srTenantRepo) SetRealmFields(ctx context.Context, id uuid.UUID, realmID string, realmType domain.RealmType, shard string, recordVersion int64) error {
	if r.setRealmFieldsFn != nil {
		return r.setRealmFieldsFn(ctx, id, realmID, realmType, shard, recordVersion)
	}
	return nil
}

func buildProvisioningWithTenants(tenants port.TenantRepository) *ProvisioningService {
	return &ProvisioningService{tenants: tenants, txRunner: callThruTxRunner{}}
}

func TestProvisioning_SetRealmFields_UpdatesRealmColumns(t *testing.T) {
	tenantID := uuid.New()
	realmID, shard := "acme-realm", "shard-1"
	var gotID uuid.UUID
	var gotRealm, gotShard string
	var gotType domain.RealmType
	var gotVer int64
	tenants := &srTenantRepo{
		setRealmFieldsFn: func(_ context.Context, id uuid.UUID, rid string, rt domain.RealmType, sh string, ver int64) error {
			gotID, gotRealm, gotType, gotShard, gotVer = id, rid, rt, sh, ver
			return nil
		},
	}
	svc := buildProvisioningWithTenants(tenants)

	err := svc.SetRealmFields(context.Background(), tenantID, realmID, domain.RealmType("dedicated"), shard, 1)
	require.NoError(t, err)
	assert.Equal(t, tenantID, gotID)
	assert.Equal(t, realmID, gotRealm)
	assert.Equal(t, domain.RealmType("dedicated"), gotType)
	assert.Equal(t, shard, gotShard)
	assert.Equal(t, int64(1), gotVer)
}

func TestProvisioning_SetRealmFields_NoRowsAffected_TenantNotFound(t *testing.T) {
	tenants := &srTenantRepo{
		setRealmFieldsFn: func(context.Context, uuid.UUID, string, domain.RealmType, string, int64) error {
			return domain.NewError(domain.ErrTenantNotFound, "tenant not found")
		},
	}
	svc := buildProvisioningWithTenants(tenants)
	err := svc.SetRealmFields(context.Background(), uuid.New(), "acme", domain.RealmType("dedicated"), "shard-1", 1)
	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "tenant_not_found", de.Code)
}

func TestProvisioning_SetRealmFields_ExecErrorPropagates(t *testing.T) {
	execErr := errors.New("update failed")
	tenants := &srTenantRepo{
		setRealmFieldsFn: func(context.Context, uuid.UUID, string, domain.RealmType, string, int64) error {
			return execErr
		},
	}
	svc := buildProvisioningWithTenants(tenants)
	err := svc.SetRealmFields(context.Background(), uuid.New(), "r", domain.RealmType("shared"), "s", 1)
	assert.ErrorIs(t, err, execErr)
}
