# Runbook: RLS Cross-Tenant Access Violation

**Alert:** `IAMRLSViolationsCrossTenant`
**SLO:** Zero tolerance — any cross-tenant leak pages immediately.

## Symptoms
- `iam_rls_violations_total{violation_type="cross_tenant_access"}` > 0
- Error logs showing rows from a different tenant_id than the GUC binding

## Immediate Actions
1. Identify which pool (app/sys) triggered the violation from structured logs
2. Grep logs: `violation_type=cross_tenant_access` for tenant_id and query context
3. If confirmed cross-tenant data leak: **page on-call immediately, consider pod restart**
4. Check recent deployments for GUC binding changes in `cmd/server/` or `pkg/requestctx/`

## Root Causes
- Missing `SET LOCAL app.tenant_id` before a query
- `GUCProvider` bypass in a reconciler path running against the wrong pool
- A `withPool` call using `sysPool` where `pool` was intended

## Resolution
- Short-term: restart affected pods to clear any cached connections
- Long-term: trace the query path in `internal/adapter/outbound/postgres/` and confirm `pgcommon.GUCSetFromContext` is wired
