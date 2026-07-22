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

func cacheKeyDeptMembers(tenantID, deptID uuid.UUID) string {
	return fmt.Sprintf("om:dept_members:%s:%s", tenantID, deptID)
}

func cacheKeySeatUsage(tenantID uuid.UUID) string {
	return fmt.Sprintf("om:seat_usage:%s", tenantID)
}

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
