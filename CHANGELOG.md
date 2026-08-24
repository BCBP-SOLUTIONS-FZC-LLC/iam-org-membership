# Changelog

All notable changes to this service are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning aligned with SemVer.

This service has never been deployed to any environment — there is no released version yet, so everything lives under `[Unreleased]`. Because of that, this file tracks only what is currently true and what is still outstanding, not a phase-by-phase build history; the full development diary (schema build-out, endpoint-by-endpoint rollout, since-removed subsystems) is available via `git log` if it's ever needed.

## [Unreleased]

### Outstanding — must land before this can safely deploy

These are live cross-repo event-contract changes on this branch that require the *consuming* service to update in lockstep, not just a same-repo rename:

- **`TenantMembershipsPurged` rename.** Core's tenant-offboarding cascade signal was renamed from `TenantOffboarded` to `TenantMembershipsPurged` and moved from `iam.tenant.events` to `iam.membership.events` — the old name collided with Realm Provisioner's own `TenantOffboarded` event, which Core only *consumes*. **`iam-delegation`'s `delegation-cascade-q` consumer still subscribes to the old name/topic and must be updated before this is safe to deploy.**
- **`MembershipRevoked` consolidation.** The per-user removal cascade (`MembershipService.RemoveUser`, `ProvisioningService.DeleteMember`) now emits a single shared `MembershipRevoked` event per LLD §15.2.2, instead of the previous pair (`MembershipRevoked` for Delegation Service, `TenantMembershipRemoved` for Tender-ACL Service). **`iam-tender-acl`'s consumer still subscribes to `TenantMembershipRemoved` and must be updated to subscribe to `MembershipRevoked` instead.**

### Known gaps

- `domain.ErrGroupMappingUnavailable` (`group_mapping_unavailable`, 503) is declared per LLD §17's error taxonomy, but `GroupMappingService.resolveMappings` deliberately fails open and never actually returns it (ADR-0007 Action Item 4). This is a known mismatch between the declared taxonomy and current behavior, not a bug — flipping Group Mapping to fail-closed is a deliberate future decision, not in scope now.

### Current architecture notes

- `/metrics` is served from its own `http.Server` on `METRICS_PORT` (default `9090`), separate from the API's `APP_PORT` (`8080`) — mirrors `iam-tender-acl`'s split. This lets the monitoring-namespace `NetworkPolicy` grant reach only the metrics port instead of the whole API surface (NetworkPolicy filters by port, not path).
- The four-service decomposition (ADR-0007 + ADR-0008) is complete on this branch: departments/plans catalog ownership, SAML group→dept/role mapping, tender ACL overlays, and delegation grants/OOO coordination have all moved to their respective standalone services (Catalog/Admin Config, Group Mapping/JIT Config, Tender ACL, Delegation). This repo — "Core" — retains only the organizational layer. See `.claude/CLAUDE.md` for the current scope and ownership boundaries.
