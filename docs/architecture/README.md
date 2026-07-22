# Architecture diagrams

Standalone Mermaid source files for `ARCHITECTURE.md`. Each `.mmd` file is embedded as a fenced code block in the parent document with a `> Source:` back-reference.

| File | Diagram | Embedded in |
|------|---------|------------|
| [`layer-model.mmd`](mermaid/layer-model.mmd) | Clean Architecture layer graph | ARCHITECTURE.md §Layer model |
| [`package-dependencies.mmd`](mermaid/package-dependencies.mmd) | Go import graph (mirrors `.go-arch-lint.yml`) | ARCHITECTURE.md §Package dependency graph |
| [`request-flow.mmd`](mermaid/request-flow.mmd) | Full HTTP request lifecycle (I-8 hot path) | ARCHITECTURE.md §Request flow |
| [`write-flow.mmd`](mermaid/write-flow.mmd) | Transactional write + outbox enqueue | ARCHITECTURE.md §Write flow |
| [`provisioning-flow.mmd`](mermaid/provisioning-flow.mmd) | `TrialTenantProvisioned` → tenant bootstrap (§8.1) | ARCHITECTURE.md §Trial provisioning flow |
| [`ooo-delegate-flow.mmd`](mermaid/ooo-delegate-flow.mmd) | OOO delegation with User Profile coordination (§8.6 CONS-2) | ARCHITECTURE.md §OOO with delegation |
| [`cache-strategy.mmd`](mermaid/cache-strategy.mmd) | Valkey key namespace, TTLs, read/write/invalidation paths | ARCHITECTURE.md §Cache strategy |
| [`event-outbox-flow.mmd`](mermaid/event-outbox-flow.mmd) | Outbox → `RoutingPublisher` → two SNS topics → SQS fan-out | ARCHITECTURE.md §Event and outbox flow |
| [`observability-stack.mmd`](mermaid/observability-stack.mmd) | OTel + Prometheus + Zap structured logs + in-process exporters | ARCHITECTURE.md §Observability stack |
| [`rls-guc-flow.mmd`](mermaid/rls-guc-flow.mmd) | `app.tenant_id` GUC injection per request (RLS-6) | ARCHITECTURE.md §Row-Level Security |

To render locally, open any `.mmd` file in a Mermaid-aware IDE (VS Code + Mermaid Preview, IntelliJ + Mermaid plugin) or paste into [mermaid.live](https://mermaid.live).
