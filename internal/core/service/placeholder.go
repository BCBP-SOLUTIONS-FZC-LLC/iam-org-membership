// Package service holds the use-case layer (LLD §3, Clean Architecture
// service layer). Phase 2 lands the concrete services (TenantService,
// MembershipService, DepartmentService, etc.); Phase 1 ships this
// placeholder so the go-arch-lint component definition resolves and
// `go test ./internal/core/service/...` matches a package.
package service
