// Package requestctx carries the validated caller identity extracted from
// trusted gateway headers (x-user-id, x-tenant-id, x-tenant-roles) through
// the request scope. Handlers and services read this struct rather than
// reaching back into gin.Context, keeping core packages framework-free.
package requestctx

import (
	"context"

	"github.com/google/uuid"
)

type contextKey struct{}

// RequestContext holds the validated caller identity for a single request.
type RequestContext struct {
	UserID    uuid.UUID
	TenantID  uuid.UUID
	Roles     []string
	ClientIP  string
	UserAgent string
}

// WithContext returns a new context carrying rc.
func WithContext(ctx context.Context, rc *RequestContext) context.Context {
	return context.WithValue(ctx, contextKey{}, rc)
}

// FromContext retrieves the RequestContext set by the auth middleware.
func FromContext(ctx context.Context) (*RequestContext, bool) {
	rc, ok := ctx.Value(contextKey{}).(*RequestContext)
	return rc, ok && rc != nil
}

// HasRole reports whether the caller was granted the given role by the gateway.
func (rc *RequestContext) HasRole(role string) bool {
	for _, r := range rc.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// IsAdmin is a convenience for the two role codes that gate tenant-admin
// mutations (§10.4, AUTH-2). Does NOT include tender_admin — that role gates
// tender ACL management (§10.4, AUTH-3), checked separately.
func (rc *RequestContext) IsAdmin() bool {
	return rc.HasRole("tenant_owner") || rc.HasRole("tenant_admin")
}

// IsOperator reports whether the caller carries the platform-level operator
// role (§10.4, AUTH-6). Operator endpoints re-check this before any DB access;
// gateway header hygiene (AUTH-7) prevents client-supplied elevation.
func (rc *RequestContext) IsOperator() bool {
	return rc.HasRole("platform_operator")
}

// IsSystem reports whether the caller is the reserved iam-system principal
// (RLS-5, IAPI-2) — accepted only on /api/v1/internal/* routes.
func (rc *RequestContext) IsSystem() bool {
	return rc.HasRole("iam-system")
}
