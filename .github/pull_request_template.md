
## Description
Provide a clear description of the changes.

---

## Type of Change
- [ ] Bug fix
- [ ] New feature
- [ ] Refactor
- [ ] Documentation
- [ ] Test
- [ ] Breaking change
- [ ] New migration
- [ ] Event contract change (asyncapi.yaml or eventschema/)

---

## Testing
- [ ] Unit tests added/updated (`make test-unit`)
- [ ] Postgres / RLS integration tests added/updated (`make test-postgres`)
- [ ] Integration tests added/updated (`make test-e2e`)
- [ ] All tests passing with race detector (`make race`)
- [ ] Manual testing performed (if required)

---

## Checklist

### Code Quality
- [ ] Code is properly formatted (`make fmt-check`)
- [ ] Linting passed (`make lint`)
- [ ] Vet passed (`make vet`)
- [ ] No debug logs / commented-out code
- [ ] No secrets or DSNs hardcoded

### API Contract
- [ ] Swagger docs regenerated if handler annotations changed (`make swag` — all three files in `docs/swagger/` committed)
- [ ] AsyncAPI spec updated if a new event type or payload field was added (`api/asyncapi.yaml`)
- [ ] Event schema JSON files updated to match (`internal/adapter/outbound/eventbus/schemas/*.json`)
- [ ] Contract tests updated (`test/unit/events/contract_test.go`)
- [ ] `"additionalProperties": true` is set on every modified or new schema in `internal/adapter/outbound/eventbus/schemas/`
      (CI Pass 5 enforces this; the item is a pre-review reminder)

### Consumer Forward-Compatibility
*Complete only when `internal/adapter/outbound/eventbus/schemas/` changed.*

Backward-compatible changes (new fields, widened enums) do not require a consumer migration cycle,
but consumers must be configured for lenient deserialization or they will crash on the new fields.

- [ ] **Consumer lenient-parsing confirmed** — All known consumers of this event type are configured
      to ignore unknown fields. Required per-language settings:
      - **Go (encoding/json):** do NOT call `json.Decoder.DisallowUnknownFields()` — silently ignored by default.
      - **Go (sonic):** use `sonic.ConfigDefault` or `sonic.ConfigFastest`, NOT `sonic.ConfigStrict`.
      - **Java (Jackson):** `mapper.configure(DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES, false)` or `@JsonIgnoreProperties(ignoreUnknown=true)`.
      - **Python (Pydantic v2):** `model_config = ConfigDict(extra='ignore')` on every payload model.
      - **Kotlin (kotlinx.serialization):** `Json { ignoreUnknownKeys = true }`.
- [ ] **Deploy order followed for non-breaking additions** — Consumers deployed first, then producer.
      New fields in the payload reach consumers before the producer starts sending them. Consumers
      that haven't been updated yet will receive the new field as an ignored unknown — no crash.
      See `api/asyncapi.yaml § x-forward-compatibility.rollout-sequence` for the canonical order.

### Event Schema Semantic Evolution
*Complete only when `api/asyncapi.yaml` or `internal/adapter/outbound/eventbus/schemas/` changed.*

Structural schema diff and lifecycle checks run in CI automatically. This section
covers semantic drift that is **invisible to tooling**: a field's valid range, units,
encoding, or business meaning can change without any JSON Schema difference.

- [ ] **No semantic drift** — Confirmed that no existing field changed its valid range,
      units, encoding, or business meaning without a structural schema change.
      *Example of a semantic-only breaking change:*
      `score` field was `1–100` (integer percentage), now `0–1` (normalised float).
      The JSON Schema type stays `number` — structural diff cannot catch this.
- [ ] **Semantic change acknowledged → `.v2` created** — If a field's meaning changed
      without a structural change, a new versioned event type has been created
      (e.g. `user.updated.v2`) and the old type is marked deprecated in `asyncapi.yaml`
      with `x-lifecycle: {status: deprecated, deprecated-by: user.updated.v2, retire-after: YYYY-MM-DD}`.
- [ ] **Description-only change confirmed** — Any `description` field edits in
      `asyncapi.yaml` are wording clarifications only (no change to valid range,
      units, nullability, or consumer-observable behaviour).
      CI Pass B will list changed descriptions in the log — review them here.
      Add `[skip-semantic-check]` to the commit message to suppress the notice
      once confirmed as documentation-only.

### Database / Migrations
- [ ] New migrations have matching `.up.sql` and `.down.sql`
- [ ] Down migration correctly reverses the up migration
- [ ] RLS policies tested with `FORCE ROW LEVEL SECURITY` (`make test-postgres`)
- [ ] Migration tested against PgBouncer simple-protocol mode (`PG_BOUNCER_MODE=true`)

### Security
- [ ] No secrets or DSNs hardcoded
- [ ] GUC values are not logged (no credential leakage in slow-query output)
- [ ] `GLUE_REGISTRY_NAME` and `PG_BOUNCER_MODE=true` not set simultaneously (mutually exclusive — startup panics if both are set)
- [ ] New config fields documented in README env-vars table and `validateRequiredEnv`

### Documentation
- [ ] README updated (if public API, env vars, or rate-limit behaviour changed)
- [ ] ARCHITECTURE.md updated (if layering, flows, or key invariants changed)

---

## Related Issue
Closes #<issue-id>

---

## Deployment Notes
Mention anything important for operators upgrading (migration steps, new required env vars, config changes, breaking event schema changes).
