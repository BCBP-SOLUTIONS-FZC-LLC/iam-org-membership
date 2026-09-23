# Architecture diagrams

Standalone Mermaid source files. `ARCHITECTURE.md` embeds every one of these `.mmd` files verbatim as a fenced code block, each under a `> Source:` link back to the file here — so a diagram only needs to be correct in one place. Keep both in sync by hand when either changes.

| File | Diagram | Related section |
|------|---------|------------|
| [`layer-model.mmd`](mermaid/layer-model.mmd) | Clean Architecture layer graph | ARCHITECTURE.md § Layer model |
| [`package-dependencies.mmd`](mermaid/package-dependencies.mmd) | Go import graph (mirrors `.go-arch-lint.yml`) | `.go-arch-lint.yml` |
| [`request-flow.mmd`](mermaid/request-flow.mmd) | Full HTTP request lifecycle (I-8 hot path) | ARCHITECTURE.md § Observability, LLD §8.3 |
| [`write-flow.mmd`](mermaid/write-flow.mmd) | Transactional write + outbox enqueue | LLD §8.4 |
| [`provisioning-flow.mmd`](mermaid/provisioning-flow.mmd) | `TrialTenantProvisioned` → tenant bootstrap (§8.1) | LLD §8.1 |
| [`ooo-delegate-flow.mmd`](mermaid/ooo-delegate-flow.mmd) | Core's only two remaining delegation-adjacent touchpoints — the §8.8 tenant-wide delegate-impact gate and the §8.8.4 dept-scope precision lookup via `DelegationCheckClient`. Core owns no `delegations` table or OOO-coordination flow any more (ADR-0008) — both moved to the standalone Delegation Service. | LLD §8.8, §8.8.4 |
| [`cache-strategy.mmd`](mermaid/cache-strategy.mmd) | Valkey key namespace, TTLs, read/write/invalidation paths | LLD §6 |
| [`event-outbox-flow.mmd`](mermaid/event-outbox-flow.mmd) | Enqueue validation → plain-JSON outbox → `RoutingPublisher` → two SNS topics (GlueCodec per topic, version resolved by definition) → SQS fan-out; plus this service's own inbound pipeline (`GlueDecoder` → DLQ router → consumed-schema validation → `Handle`, permanent rejects straight to the DLQ) | LLD §7.1, §7.3 |
| [`observability-stack.mmd`](mermaid/observability-stack.mmd) | OTel + Prometheus + Zap structured logs + in-process exporters | ARCHITECTURE.md § Observability |
| [`rls-guc-flow.mmd`](mermaid/rls-guc-flow.mmd) | `app.tenant_id` GUC injection per request (RLS-6) | LLD §4.3 |

To render locally, open any `.mmd` file in a Mermaid-aware IDE (VS Code + Mermaid Preview, IntelliJ + Mermaid plugin) or paste into [mermaid.live](https://mermaid.live).
