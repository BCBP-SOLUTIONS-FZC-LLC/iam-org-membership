# Architecture diagrams

Standalone Mermaid source files. `ARCHITECTURE.md` embeds every one of these `.mmd` files verbatim as a fenced code block, each under a `> Source:` link back to the file here — so a diagram only needs to be correct in one place. Keep both in sync by hand when either changes: the embedded block must stay byte-identical to its `.mmd` file.

| File | Diagram | Related section |
|------|---------|------------|
| [`layer-model.mmd`](mermaid/layer-model.mmd) | Clean Architecture layer graph — the three binaries (`cmd/server`, `cmd/reconciler` + `jobs`, `cmd/migrate`), inbound adapters (`http`, `sqspipeline`, `consumer`), outbound adapters (incl. `httpx`, `metrics`), core, `pkg/requestctx`, tests | ARCHITECTURE.md § Layer model |
| [`package-dependencies.mmd`](mermaid/package-dependencies.mmd) | Go import graph (mirrors `.go-arch-lint.yml`, incl. the `inbound_pipeline`, `httptransport` and `reconciler_jobs` components) | `.go-arch-lint.yml` |
| [`request-flow.mmd`](mermaid/request-flow.mmd) | Full HTTP request lifecycle (I-8 hot path: `InternalHandler` → `AuthZService` → Valkey / `AuthZRepository`) | ARCHITECTURE.md § Request flow, LLD §8.3 |
| [`public-tenant-resolve-flow.mmd`](mermaid/public-tenant-resolve-flow.mmd) | PUB-1 pre-login tenant/realm resolution on the BYPASSRLS `sysPool` | ARCHITECTURE.md § PUB-1, LLD §8.11 |
| [`write-flow.mmd`](mermaid/write-flow.mmd) | Transactional write + validated, ordered outbox enqueue (P-10 example) and the runner's routed publish | LLD §8.4 |
| [`provisioning-flow.mmd`](mermaid/provisioning-flow.mmd) | I-1 `ProvisionTenant` → `ProvisioningService.TrialSignup` tenant bootstrap; the `TenantIdentityProvisioned` backstop runs the same flow when the BFF's call never completed | LLD §8.1 |
| [`ooo-delegate-flow.mmd`](mermaid/ooo-delegate-flow.mmd) | Core's only remaining delegation-adjacent touchpoints — the §8.8 tenant-wide delegate-impact gate on P-8 and the §8.8.4 dept removal/demotion gates via `DelegationCheckClient`. Core owns no `delegations` table or OOO-coordination flow any more (ADR-0008) — both moved to the standalone Delegation Service. | LLD §8.8, §8.8.4 |
| [`cache-strategy.mmd`](mermaid/cache-strategy.mmd) | Valkey key namespace, TTLs, read/write/invalidation paths, offboarding wipe | LLD §6 |
| [`data-model.mmd`](mermaid/data-model.mmd) | ER diagram of the 9 tables in `org_membership` | ARCHITECTURE.md § Data model, LLD §4 |
| [`event-outbox-flow.mmd`](mermaid/event-outbox-flow.mmd) | Enqueue validation → ordered plain-JSON outbox (`EnqueueOrdered` under an advisory lock) → `RoutingPublisher` → two SNS topics (GlueCodec per topic, version resolved by definition) → SQS fan-out, with `outbox_dead_letters`; plus this service's own inbound pipeline (`sqspipeline`: `events.GlueDecodeCodec` → DLQ router → consumer span → consumed-schema validation → `Handle` with the inbox claim, permanent rejects straight to the DLQ) | LLD §7.1, §7.3 |
| [`observability-stack.mmd`](mermaid/observability-stack.mmd) | OTel + Prometheus three-tier metrics (`gincommon.MetricsHandler`) + Zap structured logs + in-process exporters + reconciler Pushgateway + alert routing | ARCHITECTURE.md § Observability, [`docs/observability/`](../observability/README.md) |
| [`rls-guc-flow.mmd`](mermaid/rls-guc-flow.mmd) | `app.tenant_id` GUC injection per request (RLS-6) | LLD §4.3 |

To render locally, open any `.mmd` file in a Mermaid-aware IDE (VS Code + Mermaid Preview, IntelliJ + Mermaid plugin) or paste into [mermaid.live](https://mermaid.live).
