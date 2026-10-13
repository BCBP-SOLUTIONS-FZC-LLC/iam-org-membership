#!/usr/bin/env bash
# Enforces the Enterprise Platform Events Standard: every event publication,
# outbox write, consumer dedup, and metrics initialisation passes exclusively
# through platform-events/v2.
#
# This script complements check-forbidden-events-bypass.sh (which catches
# direct SDK imports, raw transport calls, bare Envelope literals, and
# hand-rolled processed_events SQL) with structural isolation rules that
# prevent the same platform-events seam from being called from the wrong
# layer (e.g. an adapter bypassing the eventbus.Publisher port).
#
# Rules enforced:
#
#   1. outbox.Enqueue / outbox.EnqueueOrdered — these two platform-events
#      outbox write functions must only appear in
#      internal/adapter/outbound/eventbus/publisher.go.  Every service-layer
#      write passes through port.EventPublisher → eventbus.Publisher.Enqueue,
#      which is the single point that validates the schema, builds the
#      ordered-enqueue advisory lock, and uses plain JSON so the outbox
#      stores human-readable records.  A call site anywhere else — a service,
#      a reconciler job, a repository — would bypass schema validation,
#      ordering guarantees, and the single-writer invariant simultaneously.
#
#   2. inbox.NewStore — the platform-events inbox store constructor must only
#      appear in internal/adapter/outbound/postgres/idempotency_repository.go.
#      That is the sole dedup repository; every consumer accesses it via
#      port.Inbox (processOnce in consumer/dedup.go uses inbox.ProcessOnce,
#      not the store directly).  A second inbox.NewStore call anywhere else
#      would create a second, siloed dedup ledger that shares processed_events
#      rows but has no guarantee of consistent consumer naming or tx scoping.
#
#   3. inbox.Store.Process — the store's claim-and-run function must only be
#      called from idempotency_repository.go.  The repository wraps it with
#      the correct transaction scoping (port.WithTx + port.WithEventPublisher
#      injected into the handler ctx) so consumers get both the DB tx and the
#      tx-bound event publisher atomically.  A raw store.Process call elsewhere
#      would skip that wiring and produce events without an outbox guarantee.
#
#   4. events.InitMetrics — platform-events metrics (platform_messages_*,
#      platform_dlq_messages_total, platform_duplicate_messages_total, …)
#      must be initialised in both composition roots: cmd/server/main.go and
#      cmd/reconciler/main.go.  An InitMetrics call in any other file is a
#      sign that a second metrics identity is being created (potentially with
#      different domain/service/environment labels), which would produce
#      duplicate or misidentified series.  Both binaries must call it, because
#      the reconciler pushes outbox and consumer metrics to the Pushgateway.
#
# Relationship to check-forbidden-events-bypass.sh:
#   - bypass.sh catches WHAT is used (SDK transport calls, Envelope literals,
#     hand-SQL).
#   - this script catches WHERE platform-events' own abstractions are called
#     (isolation of the single-writer outbox seam and the single-store dedup
#     seam).
set -euo pipefail

python3 - <<'PYEOF'
import re
import sys
from pathlib import Path

ROOTS = ["internal", "cmd", "pkg"]
errors = []

def production_go_files():
    for root in ROOTS:
        for path in Path(root).rglob("*.go"):
            if not path.as_posix().endswith("_test.go"):
                yield path

# ── Rule 1: outbox.Enqueue / outbox.EnqueueOrdered isolation ────────────────
OUTBOX_WRITE_RE = re.compile(r'\boutbox\.(Enqueue|EnqueueOrdered)\s*\(')
OUTBOX_WRITE_ALLOWED = "internal/adapter/outbound/eventbus/publisher.go"

for path in production_go_files():
    posix = path.as_posix()
    if posix == OUTBOX_WRITE_ALLOWED:
        continue
    text = path.read_text(encoding="utf-8")
    for i, line in enumerate(text.splitlines(), 1):
        if OUTBOX_WRITE_RE.search(line) and not line.strip().startswith("//"):
            fn = OUTBOX_WRITE_RE.search(line).group(1)
            errors.append(
                f"{posix}:{i}: outbox.{fn}() called outside the allowed file "
                f"({OUTBOX_WRITE_ALLOWED}) — "
                "all event writes must go through port.EventPublisher → "
                "eventbus.Publisher.Enqueue, which provides schema validation, "
                "ordered-enqueue advisory locking, and the single-writer guarantee"
            )

# ── Rule 2: inbox.NewStore isolation ────────────────────────────────────────
INBOX_STORE_NEW_RE = re.compile(r'\binbox\.NewStore\s*\(')
INBOX_STORE_ALLOWED = "internal/adapter/outbound/postgres/idempotency_repository.go"

for path in production_go_files():
    posix = path.as_posix()
    if posix == INBOX_STORE_ALLOWED:
        continue
    text = path.read_text(encoding="utf-8")
    for i, line in enumerate(text.splitlines(), 1):
        if INBOX_STORE_NEW_RE.search(line) and not line.strip().startswith("//"):
            errors.append(
                f"{posix}:{i}: inbox.NewStore() called outside the allowed file "
                f"({INBOX_STORE_ALLOWED}) — "
                "the dedup store must only be constructed in InboxRepository; "
                "a second store siloes the dedup ledger and can lose the correct "
                "tx-scoping and publisher injection that processOnce relies on"
            )

# ── Rule 3: inbox.Store.Process isolation ───────────────────────────────────
# Matches store.Process( where store is any local variable/receiver
# (but not store.Prune which is allowed to be called from the pruner job).
INBOX_PROCESS_RE = re.compile(r'\bstore\.Process\s*\(')
INBOX_PROCESS_ALLOWED = "internal/adapter/outbound/postgres/idempotency_repository.go"

for path in production_go_files():
    posix = path.as_posix()
    if posix == INBOX_PROCESS_ALLOWED:
        continue
    text = path.read_text(encoding="utf-8")
    for i, line in enumerate(text.splitlines(), 1):
        if INBOX_PROCESS_RE.search(line) and not line.strip().startswith("//"):
            errors.append(
                f"{posix}:{i}: store.Process() called outside the allowed file "
                f"({INBOX_PROCESS_ALLOWED}) — "
                "inbox.Store.Process must only run inside InboxRepository.ProcessOnce "
                "to guarantee that port.WithTx + port.WithEventPublisher are injected "
                "into the handler context before the handler writes"
            )

# ── Rule 4: events.InitMetrics presence in both binaries ────────────────────
INIT_METRICS_RE = re.compile(r'\bevents\.InitMetrics\s*\(')
REQUIRED_FILES = {
    "cmd/server/main.go": False,
    "cmd/reconciler/main.go": False,
}
FORBIDDEN_OUTSIDE = set(REQUIRED_FILES.keys())  # must not appear in other files

for path in production_go_files():
    posix = path.as_posix()
    text = path.read_text(encoding="utf-8")
    if not INIT_METRICS_RE.search(text):
        continue
    if posix in REQUIRED_FILES:
        REQUIRED_FILES[posix] = True
    else:
        # Found in an unexpected file
        for i, line in enumerate(text.splitlines(), 1):
            if INIT_METRICS_RE.search(line) and not line.strip().startswith("//"):
                errors.append(
                    f"{posix}:{i}: events.InitMetrics() called outside the "
                    "composition roots — metrics identity must be set once per "
                    "binary in cmd/server/main.go and cmd/reconciler/main.go only"
                )

for fname, found in REQUIRED_FILES.items():
    if not found:
        errors.append(
            f"{fname}: events.InitMetrics() not found — "
            "platform-events metrics (platform_messages_*, platform_dlq_*, "
            "platform_duplicate_*) are not initialised in this binary; "
            "add events.InitMetrics(events.MetricsIdentityFromLabels(identity), "
            "gincommon.MetricsRegisterer()) after metrics.Register()"
        )

if errors:
    for e in errors:
        print(f"::error::{e}")
    print(f"\n{len(errors)} violation(s). See .github/scripts/check-events-platform.sh for rationale.")
    sys.exit(1)

print("  ✔  events platform: outbox writes isolated to eventbus/publisher.go; "
      "inbox.NewStore and store.Process isolated to postgres/idempotency_repository.go; "
      "events.InitMetrics present in both composition roots")
PYEOF
