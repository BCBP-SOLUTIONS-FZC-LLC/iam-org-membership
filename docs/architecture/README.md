# Architecture diagrams

Standalone Mermaid source files. `ARCHITECTURE.md` carries its own independent (and more detailed) versions of the layer-model and cross-service-dependency diagrams inline as fenced code blocks — these `.mmd` files are a separate, narrower diagram set covering specific flows, not embedded in or generated from `ARCHITECTURE.md`. Keep both in sync by hand when either changes.

| File | Diagram | Related section |
|------|---------|------------|
| [`layer-model.mmd`](mermaid/layer-model.mmd) | Clean Architecture layer graph | ARCHITECTURE.md § Layer model |
| [`package-dependencies.mmd`](mermaid/package-dependencies.mmd) | Go import graph (mirrors `.go-arch-lint.yml`) | `.go-arch-lint.yml` |
| [`request-flow.mmd`](mermaid/request-flow.mmd) | Full HTTP request lifecycle (I-8 hot path) | ARCHITECTURE.md § Observability, LLD §8.3 |
| [`write-flow.mmd`](mermaid/write-flow.mmd) | Transactional write + outbox enqueue | LLD §8.4 |
| [`provisioning-flow.mmd`](mermaid/provisioning-flow.mmd) | `TrialTenantProvisioned` → tenant bootstrap (§8.1) | LLD §8.1 |
| [`ooo-delegate-flow.mmd`](mermaid/ooo-delegate-flow.mmd) | Core's only two remaining delegation-adjacent touchpoints — the §8.8 tenant-wide delegate-impact gate and the §8.8.4 dept-scope precision lookup via `DelegationCheckClient`. Core owns no `delegations` table or OOO-coordination flow any more (ADR-0008) — both moved to the standalone Delegation Service. | LLD §8.8, §8.8.4 |
| [`cache-strategy.mmd`](mermaid/cache-strategy.mmd) | Valkey key namespace, TTLs, read/write/invalidation paths | LLD §6 |
| [`event-outbox-flow.mmd`](mermaid/event-outbox-flow.mmd) | Outbox → `RoutingPublisher` → two SNS topics (each with its own GlueCodec) → SQS fan-out | LLD §7 |
| [`observability-stack.mmd`](mermaid/observability-stack.mmd) | OTel + Prometheus + Zap structured logs + in-process exporters | ARCHITECTURE.md § Observability |
| [`rls-guc-flow.mmd`](mermaid/rls-guc-flow.mmd) | `app.tenant_id` GUC injection per request (RLS-6) | LLD §4.3 |

To render locally, open any `.mmd` file in a Mermaid-aware IDE (VS Code + Mermaid Preview, IntelliJ + Mermaid plugin) or paste into [mermaid.live](https://mermaid.live).
