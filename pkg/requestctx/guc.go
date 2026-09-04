package requestctx

import (
	"context"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// systemPrincipal is the reserved iam-system GUC user-id stamped on
// internal provisioning, SQS consumers, and reconciler writes (RLS-5).
const systemPrincipal = "iam-system"

// WithSystemTenant binds app.tenant_id and app.user_id=iam-system into ctx
// via pgcommon's GUCSet so the next pool checkout issues
// `SET LOCAL app.tenant_id = ...` (RLS-6). Same helper iam-realm-provisioner
// uses at the inbound edge (consumer/guc.go) — core/service never imports
// pgcommon.
func WithSystemTenant(ctx context.Context, tenantID uuid.UUID) context.Context {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.TenantID = tenantID.String()
	g.UserID = systemPrincipal
	return pgcommon.WithGUCSet(ctx, g)
}

// WithTenant overrides only app.tenant_id, leaving the caller's user_id
// in place. Operator routes (O-4/O-7) use this so the path tenant is
// scoped without impersonating iam-system.
func WithTenant(ctx context.Context, tenantID uuid.UUID) context.Context {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.TenantID = tenantID.String()
	return pgcommon.WithGUCSet(ctx, g)
}
