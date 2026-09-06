# Changelog

All notable changes to this service are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning aligned with SemVer.

This service has never been deployed to any environment — there is no released version yet, so everything lives under `[Unreleased]`. Because of that, this file tracks only what is currently true and what is still outstanding, not a phase-by-phase build history; the full development diary (schema build-out, endpoint-by-endpoint rollout, since-removed subsystems) is available via `git log` if it's ever needed.

## [Unreleased]

### Fixed

- **`InvitationService.Invite`'s seat-cap-lost-race path** (`internal/core/service/invitation_service.go`) intermittently reported the wrong outcome to one racer under concurrent invites: `TxRunner.RunInTx` retries its callback on a deadlock/serialization failure, but the closure set one of two mutually-exclusive outer-scope result variables (`created`/`seatLimitErr`) without resetting either between invocations, so a stale value from an earlier, rolled-back attempt could leak into the attempt that actually committed. Fixed by resetting both at the top of the closure on every invocation.
- **CI `test/postgres` reliability**: bounded `pgcommon.NewPool` connections with a per-attempt timeout + retry (a stalled connection attempt previously had no deadline anywhere above it, so it could leak a goroutine indefinitely and starve a *later*, unrelated test's own connection attempt into the same hang); lowered `TEST_POSTGRES_PARALLEL` for CI to reduce concurrent-container resource pressure; raised the CI job and internal test timeouts to match.
- **`TestConsumerEVT14_StaleEventSkipped`** (`test/postgres/consumer_evt_test.go`) intermittently failed on Linux CI runners only: it wrote a full-nanosecond-precision `time.Time` into a `timestamptz` column, which `pgx` truncates to microsecond precision on encode, then compared the re-read (truncated) value against the original (untruncated) one — a mismatch that depends on the OS clock's actual sub-microsecond jitter. Fixed by rounding to microsecond precision before the comparison, matching the sibling test that already did this. The production EVT-14 guard itself was confirmed correct — this was a test-only bug.

### Known gaps

- `domain.ErrGroupMappingUnavailable` (`group_mapping_unavailable`, 503) is declared per LLD §17's error taxonomy, but `GroupMappingService.resolveMappings` deliberately fails open and never actually returns it (ADR-0007 Action Item 4). This is a known mismatch between the declared taxonomy and current behavior, not a bug — flipping Group Mapping to fail-closed is a deliberate future decision, not in scope now.

### Current architecture notes

- `/metrics` is served from its own `http.Server` on `METRICS_PORT` (default `9090`), separate from the API's `APP_PORT` (`8080`) — mirrors `iam-tender-acl`'s split. This lets the monitoring-namespace `NetworkPolicy` grant reach only the metrics port instead of the whole API surface (NetworkPolicy filters by port, not path).
- The four-service decomposition (ADR-0007 + ADR-0008) is complete on this branch: departments/plans catalog ownership, SAML group→dept/role mapping, tender ACL overlays, and delegation grants/OOO coordination have all moved to their respective standalone services (Catalog/Admin Config, Group Mapping/JIT Config, Tender ACL, Delegation). This repo — "Core" — retains only the organizational layer. See `.claude/CLAUDE.md` for the current scope and ownership boundaries.
