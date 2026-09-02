# Versioning and releases

This repository is a **deployed Go microservice**, not a library other services `go get` — `pkg/requestctx` is an internal helper for this process, and the `go.mod` module path exists so this repo's own code compiles, not for import by sibling repos (contrast [`platform-events`](https://github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events)'s `VERSIONING.md`, which this document is modeled on but differs from for that reason). What gets versioned here is the **container image** (`ghcr.io/bcbp-solutions-fzc-llc/iam-org-membership`) and the **Helm chart** (`deploy/helm/`) that deploys it, plus the runtime contract Realm Provisioner, Billing, AuthZ Enrichment, Workflow Service, Delegation Service, Tender ACL Service, Group Mapping / JIT Config Service, Catalog / Admin Config Service, and operators depend on. Versions are published with **Git tags** and described in [CHANGELOG.md](./CHANGELOG.md).

This document follows the same SemVer / image-tag / maintainer-process layout as [`iam-realm-provisioner/VERSIONING.md`](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/blob/main/VERSIONING.md), which is itself modeled on `iam-authz-enrichment/VERSIONING.md`.

## Semantic versioning (SemVer)

We use [SemVer 2.0.0](https://semver.org/): `MAJOR.MINOR.PATCH` (e.g. `v1.2.3`).

| Bump | When you change | Examples |
|------|-----------------|----------|
| **MAJOR** | Breaking change in the runtime contract (§ below) | Removing/renaming a P-*/I-*/O-* route, a published event `type` or its frozen payload field names, the `processed_events.consumer` value, a required env var, a domain error **code**, or an incompatible `values.yaml` restructure |
| **MINOR** | New backward-compatible capability | A new endpoint (new ID, never reuse a retired one — see the "Retired IDs" note below), a new optional env var / Helm value, a new produced or consumed event type, a new `iam_*` metric, an additive payload field |
| **PATCH** | Backward-compatible fix | Bug fix (e.g. a consumer reading the wrong payload field name), performance improvement, dependency bump with no observable behavior change, documentation-only correction |

### What counts as the runtime contract

This service has no Go package for another service to import — its "public API" is the wire/deployment contract every caller and operator depends on. Unlike sibling `iam-realm-provisioner`, this LLD has no dedicated §25-style frozen-name registry — the closest practical equivalents are the endpoint catalogue (`.claude/api-caching-events.md`, README.md § API overview) and `internal/core/domain/event.go`'s `Event*` string constants. Treat both as frozen in the same spirit RP's §25 intends, even without a dedicated section naming them so.

| In scope (SemVer applies) | Out of scope (may change without MAJOR) |
|---------------------------|------------------------------------------|
| The 36 active routes across three prefixes — `/api/v1/*` (P-1..13, P-24-28, P-30-31, P-34; 21), `/api/v1/internal/*` (I-1..5, I-8..11, I-13..16; 13), `/api/v1/operator/*` (O-4, O-7; 2) — method, path, and documented request/response field names (`.claude/api-caching-events.md`, README.md) | `internal/*` package structure, exported Go identifiers, file layout — nothing here is imported by another module. Gin's internal route-group structure is not part of the wire contract |
| The 14 produced event `type` names and their payload fields — 12 on `iam.membership.events`, 2 (`TenantCreated`/`TrialStarted`) on `iam.tenant.events` (`internal/core/domain/event.go`'s `Event*` constants) — plus the 2 inbound queue names (`tenant-orgm-q`, `billing-orgm-q`) and their `EventType` filters | Envelope field order / JSON key sort; the Glue wire-format header internals; which of the two topics an event happens to be co-produced on with a sibling service, as long as the `type` name and payload shape are unchanged |
| `processed_events.consumer = "iam-org-membership"` (a single literal, unlike some sibling services that discriminate by named sub-consumer) and `tenants.suspension_source` ∈ `{billing_lapse, operator}` | Internal dedup-check implementation details (`port.IdempotencyStore`) |
| Domain error **codes** (`internal/core/domain/errors.go`, 49 sentinels, e.g. `seat_limit_reached`, `workflow_resolution_required`, `optimistic_lock_conflict`) and their HTTP statuses (`HandleError`, LLD §17) | Exact error `message` wording |
| Required env var **names and semantics** (`.env-example`; genuinely required outside dev: `DATABASE_URL`/`PG_*`, `MIGRATION_DATABASE_URL`, `SYSTEM_DATABASE_URL`, `VALKEY_URL`, `SNS_TOPIC_MEMBERSHIP_ARN`/`SNS_TOPIC_TENANT_ARN`, `GLUE_REGISTRY_MEMBERSHIP_NAME`/`GLUE_REGISTRY_TENANT_NAME`) | Env var **defaults** (`SEAT_OVERAGE_GRACE_DAYS=30`, `SUBSCRIPTION_GRACE_DAYS=30`, `INVITATION_EXPIRY_DAYS=7`, etc.) — tunable without a MAJOR bump unless the new default itself breaks a documented invariant |
| `deploy/helm/values.yaml` top-level key names and shapes consumers actually set (`image.*`, `env`, `envFromSecret`, `cronjobs`, `autoscaling`, `service.*`, `networkPolicy.*`, replica/HPA fields) | Chart internals (`_helpers.tpl`, template structure) not exposed as a `values.yaml` key |
| Prometheus metric **names** (`internal/adapter/outbound/metrics/business.go`) — 16 counters (`iam_rls_violations_total`, `iam_unknown_event_acknowledged_total`, `iam_stale_lifecycle_event_skipped_total`, `iam_future_lifecycle_event_rejected_total`, `iam_session_revoke_failed_total`, `iam_delegate_suspend_impact_total`, `iam_tenant_ownerless_escalated_total`, `iam_seat_overage_started_total`, `iam_seat_limit_reached_total`, `iam_invite_throttled_total`, `iam_realm_sync_failed_total`, `iam_delegate_removal_blocked_total`, `iam_delegate_reassignment_total`, `iam_processed_events_duplicates_total`, `iam_xsvc_call_errors_total`, `iam_membership_exists_check_total`), 2 histograms (`iam_lifecycle_consumer_lag_seconds`, `iam_xsvc_call_latency_seconds`), 4 gauges (`iam_tenant_ownerless`, `iam_realm_sync_pending`, `iam_seat_overage_active`, `iam_pending_invitations_stale`) — dashboards and alert rules key off these exact names | Metric **label cardinality** beyond what's documented, histogram bucket boundaries; passthrough `http_*` / `events_*` / `outbox_*` / `pgcommon_*` names owned by `platform-gincommon` / `platform-events` / `platform-pgcommon` |
| `GET /healthz` / `/readyz` (HTTP) — existence and meaning (`/readyz` checks the app Postgres pool, `sysPool`, Valkey, and the outbox runner) | `GET /swagger/*any`, `GET /asyncapi`, `GET /asyncapi.yaml` content shape — documentation surfaces, not a contract a caller must hold stable. `METRICS_PORT`'s default value is a Helm default, not a SemVer surface |

Retired route/event IDs (`P-14..23,29,32,33`; `I-6,7,12`; `O-1,2,3,5,6` — moved to Catalog / Admin Config, Group Mapping, Tender ACL, and Delegation services across ADR-0007/ADR-0008) are **permanently unregistered**, never reused for a new endpoint — reusing a retired ID would itself be a breaking, confusing change even though the ID is technically "free."

This service has no Keycloak Admin API client (or any equivalent third-party admin-API dependency) to track in a compatibility row the way `iam-realm-provisioner` tracks `gocloak` — Keycloak Admin API access is exclusively Realm Provisioner's responsibility. This service's five outbound HTTP clients (Workflow, Realm Provisioner, Catalog Admin, Group Mapping, Delegation Check) are internal platform-service dependencies, tracked by their own repos' releases, not by a client-library version pinned here.

### Guarantees

- **Pre-`v1.0.0` (current status):** per [SemVer §4](https://semver.org/#spec-item-4), anything may change at any time while the major version is `0` — this service has never been deployed to any environment (per [CHANGELOG.md](CHANGELOG.md)'s own statement) and has not yet committed to a stable *service* contract via a Git tag. The table above still indicates what's *more* disruptive than what within `v0.x` (a MINOR bump is still meant to signal "safer than a MAJOR bump would have been"), but a downstream caller should not yet assume `v0.x` compatibility across a MINOR bump the way it could once `v1.0.0` ships.
- **MAJOR (once `v1` ships):** we avoid breaking changes to the runtime contract within `v1.x`. A breaking change ships as `v2.0.0` with migration notes in the CHANGELOG.
- **MINOR:** safe to redeploy without changing caller URLs, event filters, or Helm values, unless you opt into a new capability.
- **PATCH:** drop-in image replacement; upgrade recommended for security fixes (every image is CVE-scanned, see below).

## Supported releases

| Version | Status | Image tag | Notes |
|---------|--------|-----------|-------|
| *(none tagged)* | **Unreleased** | — | [CHANGELOG.md](CHANGELOG.md) lives entirely under `[Unreleased]`. Helm chart `version` is `0.1.0-phase0` (`appVersion` currently `"0.1.0"` — bump both, and normalize the `-phase0` suffix away, in lockstep with the first Git tag; do not treat `appVersion` as a published release). No live deployment yet, so no support window has started |
| `< v0.1.0` | — | — | No tagged Git releases |

**This service is not yet safe to release even once `make ci` is green.** `CHANGELOG.md`'s `[Unreleased]` section currently lists two **outstanding cross-repo event-contract changes** that block a safe deploy: `iam-delegation`'s `delegation-cascade-q` consumer still subscribes to the old `TenantOffboarded` name/topic this service renamed to `TenantMembershipsPurged` (and moved off `iam.tenant.events`, onto `iam.membership.events`), and `iam-tender-acl`'s consumer still subscribes to the retired `TenantMembershipRemoved` name this service consolidated into the shared `MembershipRevoked` event. Both consuming services must update in lockstep before a tag is cut — resolve these first, not by working around them here.

This service has no formal support-window policy yet, since nothing is running in production against a tagged release. Once a `v1.0.0` ships to a real environment, this section will define how long a superseded major line receives security-only fixes (expect the same platform convention `platform-events`/`iam-realm-provisioner` use: security fixes only, for a period the platform team sets, typically ~6 months after the next major).

## Consume a release

This service is **not** consumed via `go get` — do not add this module as a dependency of another Go service. It is consumed as a **container image** on the mesh (public `/api/v1/*`, internal `/api/v1/internal/*`, operator `/api/v1/operator/*`), via the **Helm chart** in `deploy/helm/`. Sibling services call it over HTTP (`RealmProvisionerClient`'s counterpart on RP's side is `orgmembership.Client`; Billing calls I-11; AuthZ Enrichment calls I-8/I-14).

### Pull and verify the image

Once a tag exists:

```bash
docker pull ghcr.io/bcbp-solutions-fzc-llc/iam-org-membership:v0.1.0
```

**Today** only `main`-branch images are published (see CI below). Every image pushed from `ci.yml` is signed keylessly via Sigstore/Cosign (no long-lived key) — verify before deploying:

```bash
# main-branch builds (current — signed by ci.yml's push job)
cosign verify \
  --certificate-identity-regexp "^https://github\.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/.github/workflows/.*@refs/heads/(main|master)$" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/bcbp-solutions-fzc-llc/iam-org-membership@<digest>
```

Tagged releases, published by `.github/workflows/release.yml`, use `@refs/tags/` in the identity regexp instead of `@refs/heads/(main|master)`:

```bash
# tagged releases (signed by release.yml's docker job)
cosign verify \
  --certificate-identity-regexp "^https://github\.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/.github/workflows/.*@refs/tags/.*$" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/bcbp-solutions-fzc-llc/iam-org-membership@<digest>
```

See [README.md § CI](README.md#ci).

### Image tag scheme

`release.yml`'s `docker/metadata-action` step produces, per tag push (same scheme as `iam-realm-provisioner`/`iam-authz-enrichment`):

| Tag pattern | Produced for | Example (from `v1.2.3`) |
|-------------|--------------|--------------------------|
| `vMAJOR.MINOR.PATCH` | Every release | `v1.2.3` |
| `vMAJOR.MINOR` | Every release | `v1.2` |
| `vMAJOR` | Every release | `v1` |
| `latest` | Stable releases only (no pre-release suffix) | `latest` |

Separately, `ci.yml` pushes the image on every merge to `main` with git SHA / branch tags — pin production by a **release tag or digest**, not `latest` or an untagged `main` build.

| Pin style | Use when |
|-----------|----------|
| `vMAJOR.MINOR.PATCH` | Production; exact reproducibility (recommended — also enables digest verification) |
| `vMAJOR.MINOR` | Accept PATCH updates automatically |
| `vMAJOR` | Accept MINOR/PATCH updates automatically — not recommended before `v1.0.0` |
| `latest` / untagged `main` | Local/dev experiments only; never production |

### Deploy via Helm

```bash
helm upgrade iam-org-membership ./deploy/helm \
  --install \
  --namespace iam \
  --set image.tag=v0.1.0
```

`image.tag` (`deploy/helm/values.yaml`) defaults to the chart's own `appVersion` (`Chart.yaml`) when unset. Chart `version`/`appVersion` are bumped manually alongside the Git tag (see the maintainer process below); they are **not** derived automatically from the tag by any workflow today.

The chart deploys **one** `Deployment` (`iam-org-membership` — HTTP API across all three route prefixes + 2 inbound SQS consumers + the transactional outbox runner) plus **7** `CronJob`s (dispatching `reconciler --job=<name>`), all from the same image, which carries both the `iam-org-membership` (server, `ENTRYPOINT`) and `reconciler` binaries.

## Maintainer release process

`.github/workflows/release.yml` implements the validate → build → docker (CVE scan/sign) → **deploy-gate** → publish pipeline, modeled on sibling `iam-realm-provisioner`'s release workflow — with one deliberate difference worth knowing before you tag: **the deploy-gate here is not optional.** Unlike RP's `deploy-gate` (which skips cleanly when no cluster is configured), this service's `deploy-gate` job fails immediately if the `KUBECONFIG_B64` secret isn't set, and the final `publish` job (the one that creates the GitHub Release) requires `deploy-gate` to have *succeeded* — not merely run. Practically: **you cannot complete a tagged release of this service today without a real cluster to deploy to and verify against.** `ci.yml`'s push-to-`main` path (image to GHCR + Cosign, unversioned SHA/branch tags) keeps running independently of tagging — it is not replaced by `release.yml`.

When cutting a tagged release:

1. **Merge** all changes for the release to `main`. Confirm the two outstanding cross-repo blockers in the Supported Releases section above are resolved (or no longer apply) before proceeding — tagging over them ships a contract sibling services haven't caught up to.

2. **Update `CHANGELOG.md`:** move `[Unreleased]` entries into a new `## [X.Y.Z] - YYYY-MM-DD` section. `release.yml`'s `build` job runs `.github/scripts/verify-changelog-entry.sh`, which fails the release if this section is missing — cut it *before* tagging.

3. **Bump `deploy/helm/Chart.yaml`'s `version`/`appVersion`** to match `X.Y.Z` (and drop the current `-phase0` suffix on `version` for the first real tag — that suffix is a pre-release-scaffolding marker, not part of the SemVer scheme this document describes). Nothing does this automatically — a consumer who deploys the chart without setting `image.tag` explicitly gets whatever `appVersion` was last committed, so a forgotten bump here silently ships a stale image.

4. **Run `make ci` locally** to confirm everything is green before tagging:
   ```bash
   make ci   # tidy + fmt-check + vet + lint + test-ci + build
   ```
   Note that `make ci` does **not** currently include `go-arch-lint` (unlike RP's equivalent step) — `go-arch-lint check --project-path .` is run separately in `validate-test.yml` and, as of this writing, fails on a real pre-existing violation (`eventbus/publisher.go` importing `postgres`) plus 9 harmless config-gap notices. Check its output manually before tagging until that's fixed; a red arch-lint run doesn't fail `make ci` by itself.

5. **Create and push an annotated tag** — this is what triggers `release.yml`:
   ```bash
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

6. **The release workflow** (`.github/workflows/release.yml`) runs:
   - `validate-test` / `validate-quality` — the same reusable gates `ci.yml` uses, re-run at the exact tagged commit. **`validate-test.yml`'s coverage gate defaults to a 95% threshold** (`.github/scripts/coverage-gate.sh`) and is not overridden for this repo (unlike sibling `iam-realm-provisioner`, which pins its own gate lower) — current measured coverage is roughly 79%, so this gate will fail a real run today until either the threshold is pinned explicitly or coverage on the thinner packages (`cmd/*`, `eventbus`) closes the gap.
   - `build` — verifies the tag matches HEAD (`verify-release-tag.sh`), verifies the CHANGELOG entry exists (`verify-changelog-entry.sh`), cross-compiles **only the `iam-org-membership` server binary** (`./cmd/server`) for 5 platforms (`prepare-release-binary.sh`) — note this does **not** cross-compile the `reconciler` binary the deployed image also carries; the cross-platform binaries are a convenience for non-Docker runs, not the primary distribution artifact (this service is deployed as a container carrying both entrypoints, and the container build is unaffected by this gap).
   - `docker` — builds and pushes the semver-tagged image (see tag scheme above), CVE-scans it (fails the release on CRITICAL/HIGH), generates a CycloneDX SBOM and SLSA provenance, signs with Cosign, then verifies the signature.
   - `deploy-gate` (requires `KUBECONFIG_B64`; **not skippable** — see above) — live Helm deploy to the `production` environment, deployed-digest verification against what was pushed, `kubectl rollout status`, then a 2-minute Prometheus-backed error-rate check (`http_requests_total{status_class="5xx"}` — this service's own `platform-gincommon`-emitted HTTP metric) that auto-rolls-back via `helm rollback` on failure. The error-rate check itself is skipped (not failed) if `PROMETHEUS_URL` isn't set, but the job as a whole still requires `KUBECONFIG_B64`.
   - `publish` — creates the GitHub Release with the CHANGELOG section as notes, plus the server binaries/checksums/SBOM/provenance attached. Runs only if `build`, `docker`, **and** `deploy-gate` all succeeded.

7. **Notify consumers** — Realm Provisioner, Billing (I-11 seat-usage caller), AuthZ Enrichment (I-8/I-14 caller), Workflow Service (`TenantStateChanged`/delegate-impact subscriber), Delegation Service, Tender ACL Service, Group Mapping / JIT Config Service, Catalog / Admin Config Service, and whoever owns the Helm deployment — with upgrade notes if MINOR or MAJOR. Coordinate anything that touches an event `type` name, a route ID, or the `processed_events.consumer` value first — those are the highest-blast-radius parts of this contract given how many services subscribe to this service's two SNS topics.

### Pre-release tags (optional)

| Tag pattern | Meaning |
|-------------|---------|
| `v1.1.0-rc.1` | Release candidate; not for production unless approved |
| `v1.1.0-beta.1` | Early integration testing |

Both should match a `v[0-9]*.[0-9]*.[0-9]*-*` tag trigger and produce a signed, scanned image — but never a `latest` tag (see the tag scheme table above).

## Compatibility matrix

| iam-org-membership | Go (`go.mod`) | Shared platform libraries | `platform-schemagov` (`schema-gov` CLI) |
|---|---|---|---|
| Unreleased (`main`) | `1.26.6` | `platform-gincommon` v1.3.0, `platform-events` v1.4.0, `platform-pgcommon` v1.3.0 | `0.4` (`SCHEMA_GOV_IMAGE` in `.env-example`) |

None of those libraries is re-exported — a consumer of this *service* never needs them as a direct dependency of *this* module. Callers depend on the HTTP/event contract, not on any Go package here. This service has no Keycloak Admin API dependency of its own (see the note above the Guarantees section) — Keycloak server version is entirely Realm Provisioner's and Keycloak ops' concern, invisible to this compatibility matrix.

## Related files

| File | Purpose |
|------|---------|
| [CHANGELOG.md](./CHANGELOG.md) | User-facing history per version — currently entirely `[Unreleased]`, including the two cross-repo blockers noted above |
| [README.md](./README.md) | Mental model, API/event overview, local dev, CI/CD summary |
| [CONTRIBUTING.md](./CONTRIBUTING.md#deployment) | Deployment pipeline as it exists today (`ci.yml`) |
| [docs/lld/iam-lld-org-membership-service.md](./docs/lld/iam-lld-org-membership-service.md) | The runtime contract in full — §16 open-question register, §17 error taxonomy, §18 cross-service dependencies, §19 migration strategy |
| [ARCHITECTURE.md](./ARCHITECTURE.md) | Layer model, ports, degradation matrix, key invariants, threat model |
| [.github/workflows/ci.yml](./.github/workflows/ci.yml) | Current image build / Trivy / Cosign-on-`main` pipeline |
| [.github/workflows/release.yml](./.github/workflows/release.yml) | Tag-triggered release pipeline (validate → build → docker → deploy-gate → publish) |
| [deploy/helm/Chart.yaml](./deploy/helm/Chart.yaml) | Helm chart version / app version |
| [go.mod](./go.mod) | Module path and minimum Go version — not a consumable package |
