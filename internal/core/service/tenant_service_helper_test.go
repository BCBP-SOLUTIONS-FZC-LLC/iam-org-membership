package service

import (
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireActiveMember is an unexported defense-in-depth helper. Handlers
// already gate via AUTH-1 in requestctx; this test locks the semantics for
// future service-layer callers that opt in.

func TestRequireActiveMember_NilContextIsMissingIdentity(t *testing.T) {
	err := requireActiveMember(nil, uuid.New())

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "missing_identity_headers", de.Code)
}

func TestRequireActiveMember_DifferentTenantIsInsufficientRole(t *testing.T) {
	rc := &requestctx.RequestContext{TenantID: uuid.New()}
	err := requireActiveMember(rc, uuid.New())

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "insufficient_role", de.Code,
		"cross-tenant access must map to insufficient_role, not missing_identity_headers")
}

func TestRequireActiveMember_SameTenantIsOK(t *testing.T) {
	tid := uuid.New()
	rc := &requestctx.RequestContext{TenantID: tid}
	assert.NoError(t, requireActiveMember(rc, tid))
}
