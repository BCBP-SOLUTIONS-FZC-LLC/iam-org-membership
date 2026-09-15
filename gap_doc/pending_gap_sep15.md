# Pending Gaps — September 15, 2026

**Project:** XpertPMS — IAM Subsystem  
**Service:** `iam-org-membership` cross-service compatibility audit  
**Total Gaps Found:** 18  
**Total Fixed:** 15  
**Total Pending:** 3

---

## Pending Gaps

---

### Gap 6 — SNS Subscription Missing for Delegation Service

**Service:** `iam-delegation`  
**Severity:** HIGH  
**Who Must Act:** Infra / DevOps team  
**Estimated Effort:** 30 minutes

---

**What is the problem?**

The Delegation Service consumes events from `delegation-cascade-q`. This queue needs TWO SNS subscriptions:

| Subscription | Status |
|---|---|
| `iam.membership.events` → `delegation-cascade-q` | ✅ Already exists |
| `iam.user.events` → `delegation-cascade-q` | ❌ **MISSING — must be created** |

The missing subscription is for the `UserUpdated` event from User Profile Service. When a user is disabled, their active delegations should automatically end. This logic is fully coded and tested in `CascadeService.EndForDisabledDelegate` — it will never run until this subscription exists.

**What needs to be done?**

Infra team must apply the Terraform file already written for this:

```
File: iam-delegation/deploy/messaging/sns_subscriptions.tf.example
```

What the Terraform creates:
```
SNS Topic:    iam.user.events        (owned by iam-user-profile)
SQS Target:   delegation-cascade-q
Filter:       { "EventType": ["UserUpdated"] }
DLQ:          delegation-cascade-q-dlq  (maxReceiveCount = 5)
```

**What happens after?**

The consumer code starts working immediately — no deployment needed. When a user is disabled in User Profile, all their active delegations are automatically ended.

---

### Gap 12 — DepartmentCatalogChanged Event (Two Steps Remaining)

**Service:** `iam-catalog-admin` + Infra  
**Severity:** MEDIUM  
**Who Must Act:** Infra team first → then Catalog developer  
**Estimated Effort:** 1 hour (infra) + 2 hours (code)

---

**What is the problem?**

When an operator creates, renames, or retires a department in the Catalog Service, downstream consumers (`iam-org-membership`, `iam-group-mapping`) do not see the change for up to **11 minutes** because of two cache TTL layers:

```
Operator writes to Catalog Service
      ↓  (60 seconds)    ← cat:departments cache expires
      ↓  (600 seconds)   ← om:departments / gm:departments cache expires
Consumers finally see the change
────────────────────────────────
Worst case: ~660 seconds = ~11 minutes
```

A `DepartmentCatalogChanged` event would eliminate this delay — consumers clear their caches immediately.

**What has already been done?**

| Component | Status |
|---|---|
| `port.EventNotifier` interface in catalog-admin | ✅ Done |
| `NoopEventNotifier` default (zero deps) | ✅ Done |
| `DepartmentService` calls `notify()` after every write | ✅ Done |
| `main.go` wired with `NoopEventNotifier` | ✅ Done |
| Terraform IaC for SNS + SQS | ✅ Done |
| org_membership consumer (`catalog_consumer.go`) | ✅ Done and waiting |

**Step 1 — Infra team: Apply Terraform**

```
File: iam-catalog-admin/deploy/messaging/sns_infrastructure.tf.example
```

What the Terraform creates:
```
SNS Topic:  iam-catalog-events
SQS Queue:  catalog-orgm-q        → subscribed to iam-catalog-events
DLQ:        catalog-orgm-q-dlq   (maxReceiveCount = 5)
SQS Queue:  catalog-groupmap-q    → subscribed to iam-catalog-events (future)
Filter:     { "EventType": ["DepartmentCatalogChanged"] }
```

After infra applies this, they must share:
- `SNS_TOPIC_ARN` → set in `iam-catalog-admin` Helm values
- `SQS_CATALOG_ORGM_QUEUE_URL` → set in `iam-org-membership` Helm values

**Step 2 — Catalog developer: Add SNSEventNotifier**

After `SNS_TOPIC_ARN` is available, add the real SNS implementation:

```
File to create: iam-catalog-admin/internal/adapter/outbound/events/sns_notifier.go
```

This replaces `NoopEventNotifier` with a real SNS publish call. Once wired, the full chain works:

```
Operator writes dept in Catalog Admin
        ↓
DepartmentService.notify() → SNSEventNotifier → iam-catalog-events (SNS)
        ↓
catalog-orgm-q (SQS) → org_membership CatalogConsumer
        ↓
om:departments + om:departments:stale cleared immediately
```

**Impact when complete:** Cache propagation drops from ~11 minutes to under 1 second.

---

### Gap 14 — SNS Subscription Missing for Group Mapping

**Service:** `iam-group-mapping`  
**Severity:** HIGH  
**Who Must Act:** Infra / DevOps team  
**Estimated Effort:** 30 minutes

---

**What is the problem?**

The Group Mapping Service consumes `TenantMembershipsPurged` from `tenant-lifecycle-groupmap-q`. This queue has **no SNS subscription** at all — it never receives any messages. The consumer (`OffboardingConsumer`) is fully written and tested but will never run until the subscription is created.

When a tenant is offboarded, `iam-org-membership` emits `TenantMembershipsPurged`. The Group Mapping Service should receive this and delete all three mapping tables' rows for that tenant. Currently this **never happens** — offboarded tenants' mapping rows accumulate forever.

**What needs to be done?**

Infra team must apply the Terraform file already written for this:

```
File: iam-group-mapping/deploy/messaging/sns_subscriptions.tf.example
```

What the Terraform creates:
```
SNS Topic:    iam.membership.events    (owned by iam-org-membership)
SQS Target:   tenant-lifecycle-groupmap-q
Filter:       { "EventType": ["TenantMembershipsPurged"] }
DLQ:          tenant-lifecycle-groupmap-q-dlq
              (maxReceiveCount = 5, retention = 8 days / 691200 seconds)
```

**What happens after?**

The consumer code starts working immediately — no deployment needed. When a tenant is offboarded, all their group-mapping rows across all three tables are deleted.

---

## Action Plan

| Priority | Gap | Action | Owner | Effort |
|---|---|---|---|---|
| 🔴 **High** | Gap 6 | Apply Terraform → `iam-delegation/deploy/messaging/sns_subscriptions.tf.example` | Infra team | 30 min |
| 🔴 **High** | Gap 14 | Apply Terraform → `iam-group-mapping/deploy/messaging/sns_subscriptions.tf.example` | Infra team | 30 min |
| 🟠 **Medium** | Gap 12 (Step 1) | Apply Terraform → `iam-catalog-admin/deploy/messaging/sns_infrastructure.tf.example` | Infra team | 1 hour |
| 🟠 **Medium** | Gap 12 (Step 2) | Add `SNSEventNotifier` in `iam-catalog-admin/internal/adapter/outbound/events/` | Catalog developer | 2 hours |

---

## Notes

- All Terraform `.tf.example` files are ready — infra team only needs to adapt ARN/region/account references and apply
- All consumer Go code is already written and waiting — no developer action needed for Gaps 6 and 14
- For Gap 12, only the `SNSEventNotifier` implementation remains on the developer side (after infra provides `SNS_TOPIC_ARN`)
- **Gaps 6 and 14 should be done before first production deploy** — without them, delegate-disabled cascade and tenant offboarding cascade never fire

---

*Prepared by: Sharmila Dayalan*  
*Date: 2026-09-15*  
*Contact: sharmila.dayalan@bcbpsolutions.com*
