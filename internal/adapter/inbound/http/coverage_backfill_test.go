// Additional handler-level coverage: P-2 tolerance of unknown JSON fields.
// The I-2 response-shape (I2-H-03) and iam-system cross-tenant (I2-CT-01)
// invariants are covered by the postgres integration harness in
// test/postgres/e2e_test.go which exercises the same happy path against
// a real DB — separating the unit-level HTTP contract from the service
// internals avoids replicating unexported wiring here.
package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// P2-V-08: Gin's default JSON binding tolerates unknown fields. A body
// containing "foo":1 alongside the schema-defined fields binds correctly
// and only the DTO-declared fields are consumed. This test guards against
// a future strict-binding regression that would surprise existing callers.
func TestTenantPatch_UnknownFieldTolerant(t *testing.T) {
	tenant := uuid.New()
	repo := &happyTenantRepo{
		updateFn: func(_ context.Context, id uuid.UUID, patch *domain.TenantPatch) (*domain.Tenant, error) {
			require.NotNil(t, patch.Name)
			assert.Equal(t, "Acme", *patch.Name)
			return &domain.Tenant{ID: id, Name: *patch.Name, RecordVersion: 2}, nil
		},
	}
	svc := service.NewTenantService(repo, happyCacheStub{}, &happyRPClient{})
	h := &TenantHandler{svc: svc}
	body := `{"name":"Acme","foo":1,"another_unknown":true,"record_version":1}`
	c, w := buildCtx(http.MethodPatch, "/", body, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.Patch(c)
	assert.Equal(t, http.StatusOK, w.Code,
		"unknown JSON fields are silently ignored by default Gin binding (Phase 2 tolerant)")
}
