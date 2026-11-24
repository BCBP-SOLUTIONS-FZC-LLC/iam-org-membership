package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
)

// isAdminRole reports whether code is one of the two tenant-administration
// roles that only a tenant_owner may grant or revoke (HLD §6.4, web-shell
// OM-6 / IAM-FE-Q14).
func isAdminRole(code domain.TenantRoleCode) bool {
	return code == domain.RoleTenantOwner || code == domain.RoleTenantAdmin
}

// requireOwnerForAdminRoles refuses with 403 insufficient_role when the
// caller is not a tenant_owner and the change grants or revokes
// tenant_owner or tenant_admin. touched lists the role codes the change
// adds or removes.
//
// It applies only to a request context carrying gateway roles (the
// tenant-facing P-6/P-28 paths). With no request context — internal and
// test callers — it does nothing; those paths authorize elsewhere.
func requireOwnerForAdminRoles(ctx context.Context, touched []domain.TenantRoleCode) error {
	return requireOwnerIfAdminRole(ctx, touched,
		"only a tenant_owner may grant or revoke tenant_owner or tenant_admin")
}

// requireOwnerForPrivilegedTarget extends the same rule to membership
// lifecycle actions (P-7 suspend/reactivate, P-8 remove, P-26 removal
// resolution, P-34 MFA reset):
// when the target currently holds tenant_owner or tenant_admin, only a
// tenant_owner may act on them, so a tenant_admin cannot strip a privileged
// role by suspending or removing its holder. Same request-context rule as
// requireOwnerForAdminRoles. TM-8 last-owner protection still applies on
// top of this.
func requireOwnerForPrivilegedTarget(ctx context.Context, targetRoles []domain.TenantRole) error {
	codes := make([]domain.TenantRoleCode, len(targetRoles))
	for i, tr := range targetRoles {
		codes[i] = tr.RoleCode
	}
	return requireOwnerIfAdminRole(ctx, codes,
		"only a tenant_owner may suspend, reactivate, remove, reset MFA for or resolve removal of a tenant_owner or tenant_admin")
}

// callerNeedsOwnerCheck reports whether the request carries gateway roles
// without tenant_owner — the only case the owner-only rules can refuse.
func callerNeedsOwnerCheck(ctx context.Context) bool {
	rc, ok := requestctx.FromContext(ctx)
	return ok && !rc.HasRole(string(domain.RoleTenantOwner))
}

// touchesAdminRole reports whether any code is tenant_owner or tenant_admin.
func touchesAdminRole(codes []domain.TenantRoleCode) bool {
	for _, c := range codes {
		if isAdminRole(c) {
			return true
		}
	}
	return false
}

func requireOwnerIfAdminRole(ctx context.Context, codes []domain.TenantRoleCode, msg string) error {
	rc, ok := requestctx.FromContext(ctx)
	if !ok || rc.HasRole(string(domain.RoleTenantOwner)) {
		return nil
	}
	for _, code := range codes {
		if isAdminRole(code) {
			return domain.NewError(domain.ErrInsufficientRole, msg)
		}
	}
	return nil
}

// targetPrivilegeGuard lists the target's current roles and applies
// requireOwnerForPrivilegedTarget. It skips the lookup entirely for a
// tenant_owner caller or a call with no request context.
func (s *MembershipService) targetPrivilegeGuard(ctx context.Context, tenantID, userID uuid.UUID) error {
	if !callerNeedsOwnerCheck(ctx) {
		return nil
	}
	trs, err := s.roles.ListByUser(ctx, tenantID, userID)
	if err != nil {
		return err
	}
	return requireOwnerForPrivilegedTarget(ctx, trs)
}
