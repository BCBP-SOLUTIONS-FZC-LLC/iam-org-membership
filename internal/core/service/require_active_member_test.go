package service

import (
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestRequireActiveMember_NilRC_ReturnsMissingIdentity(t *testing.T) {
	err := requireActiveMember(nil, uuid.New())
	assert.ErrorIs(t, err, domain.ErrMissingIdentity)
}

func TestRequireActiveMember_WrongTenant_ReturnsInsufficientRole(t *testing.T) {
	rc := &requestctx.RequestContext{TenantID: uuid.New()}
	err := requireActiveMember(rc, uuid.New()) // different tenantID
	assert.ErrorIs(t, err, domain.ErrInsufficientRole)
}

func TestRequireActiveMember_SameTenant_ReturnsNil(t *testing.T) {
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{TenantID: tenantID}
	assert.NoError(t, requireActiveMember(rc, tenantID))
}
