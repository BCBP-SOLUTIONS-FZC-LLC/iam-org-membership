# Runbook: I-8 Latency SLO Breach

**Alert:** `IAMSLO1_I8LatencyBudgetBurn`, `IAMSLO1b_CacheHitRateLow`
**SLO:** P99 < 15ms cache hit / P99 < 30ms cache miss (LLD §11.1 SLO-1)

## Symptoms
- `iam:i8_latency_hit_sli:ratio_rate_5m` below 0.90
- `platform_http_request_duration_seconds` P99 for `route="/api/v1/internal/users/:id/memberships"` (I-8) above budget
- AuthZ Enrichment reporting elevated latency on its org-membership calls

## Immediate Actions
1. Check Valkey hit rate — `om:memberships:{tenant}:{user}` TTL is 300s ± 30s; check eviction stats
2. Check Postgres latency: `EXPLAIN ANALYZE` on the 3 queries in `authz_repository.go FindMembershipProjection`
3. Check PgBouncer pool saturation — connection wait time in PgBouncer stats
4. Check pod CPU throttling — I-8 is CPU-bound on cache hits

## Root Causes
- Valkey eviction under memory pressure (keys evicted before TTL)
- Missing index causing table scan (`idx_tm_tenant_user_active`, `idx_tenant_roles_tenant_user`)
- PgBouncer pool exhaustion (check `PG_MAX_CONNS`)

## Resolution
- Increase Valkey memory limit or reduce TTL jitter
- Verify indexes exist: `\d tenant_memberships`, `\d tenant_roles`, `\d dept_memberships`
- Scale pods (HPA) if CPU-bound
