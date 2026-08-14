package service

import (
	"fmt"

	"github.com/google/uuid"
)

// Cache key builders — mirror the om:* keyspace in §6.1. Duplicated from
// valkey.Cache (which imports uuid) so the service layer stays free of the
// adapter package (Clean Architecture — services depend only on port + domain).
//
// If the two sides ever diverge, valkey.Cache is the source of truth and
// this file must be regenerated.

func cacheKeyTenant(tenantID uuid.UUID) string {
	return fmt.Sprintf("om:tenant:%s", tenantID)
}

func cacheKeyLocale(tenantID uuid.UUID) string {
	return fmt.Sprintf("om:locale:%s", tenantID)
}

func cacheKeyMembers(tenantID uuid.UUID, limit int) string {
	return fmt.Sprintf("om:members:%s:%d", tenantID, limit)
}

func cacheKeyRoles(tenantID uuid.UUID) string {
	return fmt.Sprintf("om:roles:%s", tenantID)
}

func cacheKeyGRM(tenantID uuid.UUID) string {
	return fmt.Sprintf("om:grm:%s", tenantID)
}

func cacheKeyGDM(tenantID uuid.UUID) string {
	return fmt.Sprintf("om:gdm:%s", tenantID)
}

// cacheKeyGTRM builds om:gtrm:{tenant} — closes a pre-existing gap
// (Document 3 §6.4): group_tenant_role_mappings was queried on every JIT
// resolution with no cache entry at all before Stage 2 of the Group
// Mapping Service cutover.
func cacheKeyGTRM(tenantID uuid.UUID) string {
	return fmt.Sprintf("om:gtrm:%s", tenantID)
}

// cacheKeyGRMStale/GDMStale/GTRMStale build the 24h stale-if-error
// fallbacks for the three group-mapping resolution keys above — same
// posture as cacheKeyDepartmentsStale/cacheKeyPlansStale: populated on
// every successful cache-miss refresh, read only when the primary key
// has expired and the live GroupMappingClient call also fails.
func cacheKeyGRMStale(tenantID uuid.UUID) string {
	return fmt.Sprintf("om:grm:stale:%s", tenantID)
}

func cacheKeyGDMStale(tenantID uuid.UUID) string {
	return fmt.Sprintf("om:gdm:stale:%s", tenantID)
}

func cacheKeyGTRMStale(tenantID uuid.UUID) string {
	return fmt.Sprintf("om:gtrm:stale:%s", tenantID)
}

func cacheKeyDeptMembers(tenantID, deptID uuid.UUID) string {
	return fmt.Sprintf("om:dept_members:%s:%s", tenantID, deptID)
}

func cacheKeySeatUsage(tenantID uuid.UUID) string {
	return fmt.Sprintf("om:seat_usage:%s", tenantID)
}

// ── catalog-admin-config read-cutover keys (migration-runbook Phase 2) ──
//
// Global, not tenant-scoped — mirror valkey.Cache.PlansKey()'s existing
// no-arg global-key shape. cacheKeyPlans() returns the same string
// operator_service.go's PatchPlan already hardcodes ("om:plans") — that
// write-side literal is intentionally left as-is (out of scope for this
// read-cutover pass); both resolve to the identical key.

func cacheKeyDepartments() string { return "om:departments" }

func cacheKeyDepartmentsStale() string { return "om:departments:stale" }

func cacheKeyPlans() string { return "om:plans" }

func cacheKeyPlansStale() string { return "om:plans:stale" }

// Reference the Phase 2b+ helpers so `golangci-lint unused` doesn't flag
// them until their consuming service lands. Named individually so a
// future rename fails at compile time.
var (
	_ = cacheKeyMembers
	_ = cacheKeyRoles
	_ = cacheKeyGRM
	_ = cacheKeyGDM
	_ = cacheKeyDeptMembers
	_ = cacheKeySeatUsage
)
