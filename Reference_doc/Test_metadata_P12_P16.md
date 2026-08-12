# Test Metadata Registry — Phases 12–16

Per [`Test_prompt.md`](./Test_prompt.md) §Test Case Documentation Format and [`Test_cover.md`](./Test_cover.md) Rule 3, every generated test carries the mandatory metadata (Test Case ID · Module · Feature · API · Scenario · Preconditions · Test Steps · Expected Result · Priority · Severity · Automation Status).

**Where each field lives**
- **Test Case ID** → the `P##-*-###` slug in the function name (e.g., `TestP12_RT_001_OutboxRoundTrip`).
- **Scenario** → the descriptive tail of the function name + the leading test docstring.
- **Preconditions** → the setup block at the top of each test body (seed helpers, `newE2EEnv`, `newPhase12Env`, `buildTestFixtures`, etc.).
- **Test Steps** → the sequence of `e.do(...)` / `Enqueue(...)` / repo/service calls inside each test.
- **Expected Result** → the `require.*` and `assert.*` calls.
- **Automation Status** → all rows are `AUTOMATED` (Go test funcs run in CI).

The remaining fields (**Module · Feature · API · Priority · Severity**) are the tabulated columns below, one row per test.

**Priority legend**: `P0` = ships blocker · `P1` = release blocker · `P2` = important · `P3` = nice-to-have.
**Severity legend**: `BLOCKER` = platform-breaking · `CRITICAL` = business-critical path · `MAJOR` = incorrect behaviour · `MINOR` = cosmetic.

---

## Phase 12 — LocalStack integration pipeline (`test/integration/`)

| Test Case ID | Module | Feature | API / Trigger | Priority | Severity |
|---|---|---|---|---|---|
| P12-RT-001 | EventBus | Outbox → SNS → SQS round-trip | `iam.membership.events` publish + subscribe | P1 | BLOCKER |
| P12-ROUTE-001 | EventBus | Two-topic routing — tenant lane isolation | `TenantCreated` → `iam.tenant.events` | P1 | BLOCKER |
| P12-ROUTE-002 | EventBus | Two-topic routing — membership lane isolation | `DepartmentMembershipGranted` → `iam.membership.events` | P1 | BLOCKER |
| P12-FILTER-001 | EventBus | SNS FilterPolicy — billing queue (§7.3.2) | Subscription attr `EventType` filter | P1 | CRITICAL |
| P12-FILTER-002 | EventBus | SNS FilterPolicy — workflow queue | Subscription attr `EventType` filter | P1 | CRITICAL |
| P12-FILTER-003 | EventBus | SNS FilterPolicy — authz queue | Subscription attr `EventType` filter | P1 | CRITICAL |
| P12-FILTER-004 | EventBus | SNS FilterPolicy — realm queue | Subscription attr `EventType` filter | P1 | CRITICAL |
| P12-ATTR-001 | EventBus | Publisher stamps `EventType` MessageAttribute | SNS Publish | P1 | BLOCKER |
| P12-DLQ-001 | EventBus | RedrivePolicy(maxReceiveCount=5) → DLQ | SQS DLQ | P1 | CRITICAL |
| P12-IDEMP-001 | Consumer | `processed_events` dedup (IDEMP-4) | SQS at-least-once redelivery | P1 | CRITICAL |
| P12-IDEMP-002 | Consumer | Multi-consumer PK (PE-1) | `processed_events(event_id, consumer)` | P2 | MAJOR |
| P12-EVT16-001 | Consumer | §16 A61 wire relay end-to-end | `TrialExpired` → `TenantStateChanged` | P1 | CRITICAL |
| P12-MULTITEN-001 | Consumer | Multi-tenant routing independence | 2 envelopes / 2 tenants | P1 | CRITICAL |
| P12-OUTBOX-RETRY-001 | EventBus | Transient publisher failure retried, `published_at` marked | Outbox runner MaxAttempts=5 | P1 | CRITICAL |

---

## Phase 13 — HTTP e2e (`test/e2e/`)

| Test Case ID | Module | Feature | API / Trigger | Priority | Severity |
|---|---|---|---|---|---|
| P13-INFRA-001 | HTTP | Unauthenticated `/healthz` | `GET /healthz` | P0 | BLOCKER |
| P13-INFRA-002 | HTTP | `/readyz` reflects DB health | `GET /readyz` | P0 | BLOCKER |
| P13-MW-001 | Middleware | Gateway headers required | Any protected route | P0 | BLOCKER |
| P13-MW-002 | Middleware | `X-Request-ID` echoed | Any route | P2 | MINOR |
| P13-MW-003 | Middleware | `X-Request-ID` auto-generated | Any route | P2 | MINOR |
| P13-MW-004 | Middleware | 1 MB body cap | Any POST/PATCH/PUT | P1 | CRITICAL |
| P13-MW-005 | Middleware | `RequireSystemRole` (AUTH-5) | `/api/v1/internal/*` | P0 | BLOCKER |
| P13-MW-006 | Middleware | `RequireOperatorRole` (AUTH-6) | `/api/v1/operator/*` | P0 | BLOCKER |
| P13-MW-007 | Middleware | `RequireJSONContentType` (415) | POST/PATCH/PUT | P1 | MAJOR |
| P13-ERR-001 | HTTP | 404 error envelope shape (§17) | `GET /tenants/:id` | P1 | MAJOR |
| P13-ERR-002 | HTTP | Non-UUID path param → 4xx envelope | `GET /tenants/not-a-uuid` | P1 | MAJOR |
| P13-ERR-003 | HTTP | Malformed JSON → 400 envelope | `POST /members` | P1 | MAJOR |
| P13-ERR-004 | HTTP | Cross-tenant read → 403 envelope | `GET /tenants/:otherId` | P0 | BLOCKER |
| P13-FLOW-001 | HTTP | GET tenant happy path | P-2 | P1 | CRITICAL |
| P13-FLOW-002 | HTTP | Invite → list-invitations flow | P-6 → P-30 | P1 | CRITICAL |
| P13-FLOW-003 | HTTP | Delegation create → list → cancel | P-19 → P-18 → P-20 | P1 | CRITICAL |
| P13-FLOW-004 | HTTP | Seat-usage shape | P-27 | P2 | MAJOR |
| P13-FLOW-005 | HTTP | I-8 hot path returns MembershipProjection | I-8 | P0 | BLOCKER |
| P13-FLOW-006 | HTTP | I-1 trial signup — 5 depts + 3 labels + owner + role + 2 events | I-1 | P0 | BLOCKER |
| P13-FLOW-007 | HTTP | PUT roles reconcile applies exact target set | P-28 | P1 | CRITICAL |

---

## Phase 14 — Security (`test/e2e/`)

| Test Case ID | Module | Feature | API / Trigger | Priority | Severity |
|---|---|---|---|---|---|
| P14-SQLI-001 | Security | SQL injection in invite email — parameterization proof | P-6 | P0 | BLOCKER |
| P14-SQLI-002 | Security | SQL injection in role-label display_name | P-13 | P0 | BLOCKER |
| P14-XSS-001 | Security | `<script>` in full_name — JSON escapes `<`, DB stores raw | P-6 | P1 | CRITICAL |
| P14-XSS-002 | Security | HTML in delegation reason — JSON escapes `<`, DB stores raw | P-19 | P1 | CRITICAL |
| P14-AUTH-001 | Security | Empty `x-user-id` → 401 | Any protected route | P0 | BLOCKER |
| P14-AUTH-002 | Security | Empty `x-tenant-id` → 401 | Any protected route | P0 | BLOCKER |
| P14-AUTH-003 | Security | Non-UUID `x-tenant-id` → 4xx | Any protected route | P1 | CRITICAL |
| P14-AUTH-004 | Security | Bogus roles cannot open operator lane | `/operator/*` | P0 | BLOCKER |
| P14-TAMPER-001 | Security | Fabricated `record_version` → 409 | PATCH /tenants/:id | P1 | CRITICAL |
| P14-TAMPER-002 | Security | Non-owner cannot invite with `tenant_owner` | P-6 | P0 | BLOCKER |
| P14-TAMPER-003 | Security | Body-supplied `actor_id` ignored; `granted_by` = gateway | P-28 | P1 | CRITICAL |
| P14-PRIV-001 | Security | `tender_admin` cannot grant `tenant_owner` | P-28 | P0 | BLOCKER |
| P14-PRIV-002 | Security | Unprivileged member cannot self-elevate | P-28 | P0 | BLOCKER |
| P14-CT-001 | Security | Cross-tenant invite → 403 | P-6 | P0 | BLOCKER |
| P14-CT-002 | Security | Cross-tenant list members → 403 | P-4 | P0 | BLOCKER |
| P14-CT-003 | Security | Cross-tenant dept-assign → 403 | P-11 | P0 | BLOCKER |
| P14-CT-004 | Security | Cross-tenant role reconcile → 403 | P-28 | P0 | BLOCKER |
| P14-CT-005 | Security | RLS pool proof — tenant B invisible under tenant A GUC | RLS-6 | P0 | BLOCKER |
| P14-REPLAY-001 | Security | Duplicate invite → 409 (PI-1) | P-6 | P1 | CRITICAL |
| P14-REPLAY-002 | Security | Double cancel with stale record_version → 4xx | P-20 | P1 | CRITICAL |

---

## Phase 15 — Concurrency stress (`test/postgres/`)

| Test Case ID | Module | Feature | Trigger | Priority | Severity |
|---|---|---|---|---|---|
| P15-JIT-001 | Concurrency | 4 concurrent JIT membership adds → uq_tm_active_user permits 1 | I-3 concurrent | P1 | CRITICAL |
| P15-ACCEPT-001 | Concurrency | 3 concurrent accepts → status-transition guard permits 1 | I-3 concurrent | P1 | CRITICAL |
| P15-DEL-CREATE-001 | Concurrency | 2 concurrent creates → advisory-lock permits 1 (DEL-1) | P-19 concurrent | P1 | CRITICAL |
| P15-DEL-CANCEL-001 | Concurrency | 3 concurrent cancels → CONC-1 optimistic lock permits 1 | P-20 concurrent | P1 | CRITICAL |
| P15-B15-EXT-001 | Concurrency | 3 concurrent dept-assigns → uq_dm_active_membership permits 1 | P-11 concurrent | P1 | CRITICAL |
| P15-REC-001 | Concurrency | Reconciler + live inserts — no misclassification | invitation-expiry CronJob | P1 | CRITICAL |
| P15-OUTBOX-001 | Concurrency | `SKIP LOCKED` guarantees disjoint claim batches | outbox runner horizontal scale | P0 | BLOCKER |

---

## Phase 16 — Performance benches (`test/postgres/`)

| Test Case ID | Module | Feature | Target | Priority | Severity |
|---|---|---|---|---|---|
| P16-SLO-I8 | Perf | GetMembership P99 SLO | I-8 hot path < 30 ms (LLD §11.1) | P0 | BLOCKER |
| P16-SLO-SEAT-PREFLIGHT | Perf | 100 concurrent Invites converge under seat cap | SEAT-1 | P1 | CRITICAL |
| P16-RLS-OVERHEAD | Perf | RLS-pool vs raw-pool overhead ≤ 5× | RLS-6 cost budget | P1 | MAJOR |
| BenchmarkP16_I8HotPath | Perf | GetMembership ns/op baseline | I-8 | P2 | MAJOR |
| BenchmarkP16_BulkP28_100Users | Perf | Bulk role reconcile throughput | P-28 | P2 | MAJOR |
| BenchmarkP16_OutboxInsertOne | Perf | Single outbox enqueue latency | outbox insert | P2 | MAJOR |
| BenchmarkP16_OutboxDrain50 | Perf | 50-row SKIP LOCKED drain throughput | outbox runner | P2 | MAJOR |

---

## Change log

- **2026-07-26 (Phase 17)** — initial registry created; covers all tests generated in Phases 12–16.
