// Package main global Swagger annotations. `swag init` reads this file
// (via `-g swagger_info.go`) to build the top-level OpenAPI/Swagger
// specification — title, version, host, base path, security schemes,
// and tag descriptions. Per-handler `// @…` annotations live next to
// each handler function under internal/adapter/inbound/http/.
//
// Mirrors the sibling iam-user-profile2 pattern so developers moving
// between services see the same layout. Regenerate the spec with:
//
//	make swag
//
// The generated files under docs/swagger/ are checked into the repo;
// CI's make swag-check fails a PR whose annotations diverge from them.
//
// @title           IAM Org & Membership API
// @version         1.0
// @description     Organizational layer of the IAM subsystem — owns tenants, departments, memberships, tenant-level roles, delegations, invitations, ACL overlays, and group→role mappings.
// @description
// @description     **Tenant isolation.** Every resource is scoped by `x-tenant-id`; cross-tenant reads are blocked by row-level security (RLS-1..RLS-6). The hot path `GET /internal/users/:id/memberships` is the enrichment source for AuthZ (SLO 15 ms cache-hit / 30 ms cache-miss).
// @description
// @description     **Route prefixes.**  `/api/v1/*` — authenticated tenant users (JWT via gateway).  `/api/v1/internal/*` — in-mesh services (mTLS, `iam-system` role).  `/api/v1/operator/*` — platform operators (`platform_operator` role).
// @description
// @description     Every mutation echoes `record_version` for optimistic-locking round-trip (CONC-4).
//
// @contact.name   BCBP Solutions
// @contact.email  sharmila.dayalan@bcbpsolutions.com
//
// @license.name   Proprietary
//
// @host      localhost:8080
// @BasePath  /api/v1
//
// @securityDefinitions.apikey UserID
// @in                         header
// @name                       x-user-id
// @description                Authenticated user UUID injected by the API gateway. Use `iam-system` for in-mesh service calls hitting `/api/v1/internal/*`.
//
// @securityDefinitions.apikey TenantID
// @in                         header
// @name                       x-tenant-id
// @description                Tenant UUID injected by the API gateway. Establishes the RLS scope for the request (RLS-6).
//
// @securityDefinitions.apikey TenantRoles
// @in                         header
// @name                       x-tenant-roles
// @description                Comma-separated tenant-role list injected by the API gateway. Operator routes require `platform_operator`; internal routes require `iam-system`.
//
// @tag.name         infra
// @tag.description  Health & metrics (unauthenticated)
//
// @tag.name         tenant
// @tag.description  Tenant CRUD (P-1, P-2)
//
// @tag.name         departments
// @tag.description  Department activation (P-3, P-9, P-10, P-11, P-24, P-25)
//
// @tag.name         members
// @tag.description  Membership management (P-4..P-8, P-27, P-28)
//
// @tag.name         roles
// @tag.description  Dept-role labels (P-12, P-13)
//
// @tag.name         groups
// @tag.description  SAML group mappings (P-14..P-17, P-29)
//
// @tag.name         delegations
// @tag.description  OOO delegations (P-18, P-19, P-20)
//
// @tag.name         acl
// @tag.description  Tender ACL overlays (P-21, P-22, P-23)
//
// @tag.name         invitations
// @tag.description  Two-step invite→accept (P-6, P-30, P-31)
//
// @tag.name         resolution
// @tag.description  Removal-resolution (P-26)
//
// @tag.name         internal
// @tag.description  In-mesh service-to-service (I-1..I-13)
//
// @tag.name         operator
// @tag.description  Platform operator (O-1..O-7)
package main
