# -----------------------------
# CONFIG
# -----------------------------
-include .env

APP_NAME      ?= iam-org-membership
APP_ENV       ?= dev
GO            ?= go
BUILD_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Private modules — resolved via SSH (git@github.com:) using the global URL
# rewrite. No tokens needed for local dev; an SSH key registered with the
# BCBP-SOLUTIONS-FZC-LLC org is required.
export GOPRIVATE  ?= github.com/BCBP-SOLUTIONS-FZC-LLC/*
export GONOSUMDB  ?= github.com/BCBP-SOLUTIONS-FZC-LLC/*

export APP_NAME APP_ENV BUILD_VERSION

# Test package groups (explicit to handle per-group build tags cleanly).
TEST_UNIT_PKGS     := ./test/unit/...
TEST_POSTGRES_PKGS := ./test/postgres/...
TEST_INT_PKGS      := ./test/integration/...
TEST_E2E_PKGS      := ./test/e2e/...

# Every test/postgres/*.go test func calls t.Parallel() and spins its own
# testcontainers Postgres (create+migrate+grant, ~1-2s) — capped, not
# GOMAXPROCS-wide, so we don't spin 10+ containers at once on a dev machine
# and reintroduce Docker resource-contention flakes. Override on the CLI
# (e.g. `make test-postgres TEST_POSTGRES_PARALLEL=8`) on a bigger box.
TEST_POSTGRES_PARALLEL ?= 4

# White-box (package-internal) tests included in unit runs.
TEST_INTERNAL_PKGS := ./internal/adapter/inbound/http/... \
                      ./internal/adapter/inbound/consumer/... \
                      ./internal/adapter/outbound/eventbus/... \
                      ./internal/adapter/outbound/postgres/... \
                      ./internal/adapter/outbound/workflow/... \
                      ./internal/adapter/outbound/realmprovisioner/... \
                      ./internal/adapter/outbound/catalogadmin/... \
                      ./internal/adapter/outbound/groupmappingclient/... \
                      ./internal/adapter/outbound/httpx/... \
                      ./internal/adapter/outbound/metrics/... \
                      ./internal/adapter/outbound/valkey/... \
                      ./internal/core/port/... \
                      ./internal/core/service/... \
                      ./pkg/...

COVER_PKG_LIST := $(shell $(GO) list ./internal/... ./pkg/... 2>/dev/null | tr '\n' ',' | sed 's/,$$//')

SCHEMA_GOV_IMAGE ?= ghcr.io/bcbp-solutions-fzc-llc/platform-schemagov:0.4

# -----------------------------
# SETUP
# -----------------------------

.PHONY: setup
setup:
	@test -f .env || cp .env-example .env
	@mkdir -p .git/hooks
	@test -f .githooks/pre-commit && cp .githooks/pre-commit .git/hooks/pre-commit && chmod +x .git/hooks/pre-commit || true
	@echo "Environment ready (.env)"

.PHONY: install-hooks
install-hooks:
	@mkdir -p .git/hooks
	@cp .githooks/pre-commit .git/hooks/pre-commit
	@chmod +x .git/hooks/pre-commit
	@echo "Installed git hooks"

# godoc: serve package documentation locally using pkgsite.
# Opens http://localhost:8080 — browse to the module path in the UI.
.PHONY: godoc
godoc:
	@echo "Starting pkgsite at http://localhost:8080 — press Ctrl-C to stop"
	$(GO) run golang.org/x/pkgsite/cmd/pkgsite@latest -open .

# pin-base-images: fetch and pin the current SHA digests for Dockerfile base
# images. Writes the digests both to the Dockerfile FROM lines and to
# .docker-digests (a checked-in provenance record). CI verifies the two match.
.PHONY: pin-base-images
pin-base-images:
	@echo "Fetching SHA digests for Dockerfile base images..."
	@GOLANG_DIGEST=$$(docker buildx imagetools inspect golang:1.26.6-alpine --format '{{.Manifest.Digest}}') && \
	 DISTROLESS_DIGEST=$$(docker buildx imagetools inspect gcr.io/distroless/static-debian12:nonroot --format '{{.Manifest.Digest}}') && \
	 sed -i.bak \
	   -e "s|FROM golang:1.26.6-alpine|FROM golang:1.26.6-alpine@$$GOLANG_DIGEST|" \
	   -e "s|FROM gcr.io/distroless/static-debian12:nonroot|FROM gcr.io/distroless/static-debian12:nonroot@$$DISTROLESS_DIGEST|" \
	   Dockerfile && rm -f Dockerfile.bak && \
	 echo "golang:1.26.6-alpine $$GOLANG_DIGEST" > .docker-digests && \
	 echo "gcr.io/distroless/static-debian12:nonroot $$DISTROLESS_DIGEST" >> .docker-digests && \
	 echo "Digests written to .docker-digests — commit both Dockerfile and .docker-digests"

.PHONY: help
help:
	@echo "Available commands:"
	@echo "  make setup           - copy .env-example to .env if missing"
	@echo "  make tidy            - go mod tidy"
	@echo "  make fmt             - go fmt ./..."
	@echo "  make fmt-check       - verify gofmt formatting (mirrors CI)"
	@echo "  make vet             - go vet all packages"
	@echo "  make lint            - run golangci-lint (via go tool)"
	@echo "  make test            - unit + postgres + integration tests (requires Docker)"
	@echo "  make test-ci         - test with race detector + coverage (used in CI)"
	@echo "  make test-unit       - unit tests only (no Docker required)"
	@echo "  make test-postgres   - Postgres + RLS integration tests (requires Docker)"
	@echo "  make test-integration - cross-layer integration tests (SNS/SQS via LocalStack)"
	@echo "  make test-e2e        - end-to-end tests (requires Docker)"
	@echo "  make test-smoke      - CI-only image gate: size <=200MB + startup-gate check (requires a local docker image tagged iam-org-membership-ci-test)"
	@echo "  make race            - all tests with -race flag"
	@echo "  make run             - run the server locally (go run)"
	@echo "  make build           - compile both binaries to bin/"
	@echo "  make cover           - coverage HTML report"
	@echo "  make cover-func      - coverage summary by function"
	@echo "  make ci              - tidy + fmt-check + vet + lint + test-ci + build"
	@echo "  make docker-up       - start local infra with LocalStack community (no token needed)"
	@echo "  make docker-up-pro   - start local infra with LocalStack Pro (requires LOCALSTACK_AUTH_TOKEN in .env)"
	@echo "  make docker-down     - stop local containers"
	@echo "  make mod-verify      - go mod verify"
	@echo "  make vuln-check      - govulncheck on internal + pkg"
	@echo "  make extract-schemas - derive internal/adapter/outbound/eventbus/schemas/*.json"
	@echo "  make swag            - regenerate docs/swagger/ from handler annotations (mirrors sibling iam-user-profile2)"
	@echo "  make swag-check      - fail if Swagger regeneration would change docs/swagger/ (CI drift gate)"
	@echo "  make install-hooks   - install .githooks/pre-commit into .git/hooks"
	@echo "  make godoc           - serve local godoc/pkgsite at http://localhost:8080"
	@echo "  make pin-base-images - fetch + pin SHA digests for Dockerfile base images"
	@echo "  make clean           - remove build artefacts"
	@echo ""
	@echo "Schema governance (platform-schemagov 0.4):"
	@echo "  make schema-pull      - pull the schema-gov Docker image"
	@echo "  make schema-validate  - validate AsyncAPI + event schemas (no AWS needed)"
	@echo "  make schema-diff      - diff two schemas: CURRENT=<path> PROPOSED=<path>"
	@echo "  make schema-register  - register event schemas to Glue (requires AWS)"
	@echo "  make schema-verify    - pre-deploy check for expected schemas"
	@echo "  make schema-prune     - dry-run: list orphaned Glue schemas"

# -----------------------------
# GO BASICS
# -----------------------------

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: fmt
fmt:
	@gofmt -l -w .

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -l . 2>/dev/null); \
	if [ -n "$$unformatted" ]; then \
		echo "FAIL: unformatted files:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	@echo "gofmt: all files formatted"

.PHONY: mod-verify
mod-verify:
	$(GO) mod verify

.PHONY: vuln-check
vuln-check:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./internal/... ./pkg/...

# -----------------------------
# LINT
# -----------------------------

.PHONY: lint
lint:
	@echo "Running linter..."
	$(GO) tool golangci-lint run

# -----------------------------
# TESTS
# -----------------------------

.PHONY: _test-unit
_test-unit: | .coverage
	$(GO) test $(TEST_UNIT_PKGS) $(TEST_INTERNAL_PKGS) \
	  -race -count=1 -timeout 120s \
	  -coverpkg=$(COVER_PKG_LIST) \
	  -coverprofile=.coverage/unit.out

.PHONY: _test-postgres
_test-postgres: | .coverage
	$(GO) test $(TEST_POSTGRES_PKGS) \
	  -tags=integration -race -count=1 -timeout 300s -parallel $(TEST_POSTGRES_PARALLEL) \
	  -coverpkg=$(COVER_PKG_LIST) \
	  -coverprofile=.coverage/postgres.out

.PHONY: _test-integration
_test-integration: | .coverage
	$(GO) test $(TEST_INT_PKGS) \
	  -tags=integration -race -count=1 -timeout 300s \
	  -coverpkg=$(COVER_PKG_LIST) \
	  -coverprofile=.coverage/integration.out

.PHONY: test
test:
	$(MAKE) -j3 _test-unit-plain _test-postgres-plain _test-integration-plain

.PHONY: _test-unit-plain _test-postgres-plain _test-integration-plain
_test-unit-plain:
	$(GO) test $(TEST_UNIT_PKGS) $(TEST_INTERNAL_PKGS) -count=1 -timeout 120s -v
_test-postgres-plain:
	$(GO) test $(TEST_POSTGRES_PKGS) -tags=integration -count=1 -timeout 300s -parallel $(TEST_POSTGRES_PARALLEL) -v
_test-integration-plain:
	$(GO) test $(TEST_INT_PKGS) -tags=integration -count=1 -timeout 300s -v

# Merge the three per-suite profiles into a single coverage.out (max-count
# strategy — any suite covering a block wins). Mirrors iam-user-profile2.
.PHONY: _merge-coverage
_merge-coverage:
	@python3 scripts/merge_coverage.py \
	  .coverage/unit.out .coverage/postgres.out .coverage/integration.out \
	  > coverage.out
	@echo "==> coverage.out merged from all suites (max-count strategy)"

.PHONY: test-ci
test-ci: | .coverage
	$(MAKE) -j3 _test-unit _test-postgres _test-integration
	$(MAKE) _merge-coverage

.PHONY: test-unit
test-unit:
	$(GO) test $(TEST_UNIT_PKGS) $(TEST_INTERNAL_PKGS) -count=1 -timeout 60s -v

.PHONY: test-postgres
test-postgres:
	$(GO) test $(TEST_POSTGRES_PKGS) -tags=integration -count=1 -timeout 300s -parallel $(TEST_POSTGRES_PARALLEL) -v

.PHONY: test-integration
test-integration:
	$(GO) test $(TEST_INT_PKGS) -tags=integration -count=1 -timeout 300s -v

.PHONY: test-e2e
test-e2e:
	$(GO) test $(TEST_E2E_PKGS) -tags=e2e -count=1 -timeout 300s -v

.PHONY: test-smoke
test-smoke:
	bash .github/scripts/smoke-tests.sh

.PHONY: race
race:
	$(MAKE) -j3 _test-unit _test-postgres _test-integration

# -----------------------------
# RUN
# -----------------------------

.PHONY: run
run:
	@-lsof -ti :$${APP_PORT:-8080} | xargs kill -9 2>/dev/null; true
	bash -c 'set -a && source .env && set +a && BUILD_VERSION=$(BUILD_VERSION) $(GO) run ./cmd/server'

# -----------------------------
# BUILD
# -----------------------------

.PHONY: build
build:
	@echo "Building binaries..."
	@mkdir -p bin
	$(GO) build -ldflags "-X main.buildVersion=$(BUILD_VERSION)" -o bin/$(APP_NAME) ./cmd/server
	$(GO) build -ldflags "-X main.buildVersion=$(BUILD_VERSION)" -o bin/reconciler ./cmd/reconciler
	@echo "Verifying library packages compile..."
	$(GO) build ./internal/... ./pkg/...

# -----------------------------
# DOCKER
# -----------------------------

.PHONY: docker-up
docker-up:
	@echo "Starting local PostgreSQL + PgBouncer + Valkey + LocalStack (community)..."
	docker compose up -d postgres pgbouncer redis localstack

.PHONY: docker-up-pro
docker-up-pro:
	@echo "Starting local PostgreSQL + PgBouncer + Valkey + LocalStack Pro (Glue + KMS)..."
	@grep -q '^LOCALSTACK_AUTH_TOKEN=.\+' .env 2>/dev/null || { echo "ERROR: LOCALSTACK_AUTH_TOKEN not set in .env"; exit 1; }
	docker compose -f docker-compose.yml -f docker-compose.pro.yml up -d postgres pgbouncer redis localstack

.PHONY: docker-down
docker-down:
	@echo "Stopping local containers..."
	docker compose down

# -----------------------------
# CI
# -----------------------------

.PHONY: ci
ci: tidy fmt-check vet lint test-ci build

# -----------------------------
# COVERAGE
# -----------------------------

.PHONY: cover
cover: test-ci
	$(GO) tool cover -html=coverage.out

.PHONY: cover-func
cover-func: test-ci
	$(GO) tool cover -func=coverage.out

# -----------------------------
# SCHEMA GOVERNANCE
# -----------------------------

.PHONY: schema-pull
schema-pull:
	docker pull "$(SCHEMA_GOV_IMAGE)"

.PHONY: extract-schemas
extract-schemas:
	@echo "Extracting event schemas from api/asyncapi.yaml..."
	docker run --rm \
	  -v "$(CURDIR)":/workspace \
	  "$(SCHEMA_GOV_IMAGE)" extract \
	  --asyncapi   api/asyncapi.yaml \
	  --schema-dir internal/adapter/outbound/eventbus/schemas
	@echo "Done. Run 'git add internal/adapter/outbound/eventbus/schemas/' to stage."

.PHONY: schema-validate
schema-validate: extract-schemas
	docker run --rm \
	  -v "$(CURDIR)":/workspace \
	  "$(SCHEMA_GOV_IMAGE)" validate \
	  --asyncapi   api/asyncapi.yaml \
	  --schema-dir internal/adapter/outbound/eventbus/schemas

.PHONY: schema-diff
schema-diff:
	@test -n "$(CURRENT)" && test -n "$(PROPOSED)" || { \
	  echo "Usage: make schema-diff CURRENT=<current.json> PROPOSED=<proposed.json>"; \
	  exit 1; \
	}
	docker run --rm \
	  -v "$(CURDIR)":/workspace \
	  "$(SCHEMA_GOV_IMAGE)" diff \
	  --current     "$(CURRENT)" \
	  --proposed    "$(PROPOSED)" \
	  --schema-name "$(or $(SCHEMA_NAME),$(notdir $(basename $(PROPOSED))))"

# schema-register: register event schemas to BOTH Glue registries (SCHEMA-7 —
# unlike single-registry iam-user-profile, org-membership backs two SNS
# topics with two registries). Requires AWS credentials or LocalStack; set
# AWS_ENDPOINT_URL=http://localhost:4567 in .env for LocalStack.
.PHONY: schema-register
schema-register:
	@test -n "$(GLUE_REGISTRY_MEMBERSHIP_NAME)" || { \
	  echo "GLUE_REGISTRY_MEMBERSHIP_NAME is not set — add it to .env"; \
	  exit 1; \
	}
	@test -n "$(GLUE_REGISTRY_TENANT_NAME)" || { \
	  echo "GLUE_REGISTRY_TENANT_NAME is not set — add it to .env"; \
	  exit 1; \
	}
	@for registry in $(GLUE_REGISTRY_MEMBERSHIP_NAME) $(GLUE_REGISTRY_TENANT_NAME); do \
	  echo "Registering schemas -> $$registry"; \
	  docker run --rm \
	    -v "$(CURDIR)":/workspace \
	    -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_SESSION_TOKEN \
	    -e AWS_REGION="$(AWS_REGION)" \
	    -e AWS_ENDPOINT_URL="$(AWS_ENDPOINT_URL)" \
	    "$(SCHEMA_GOV_IMAGE)" register \
	    --registry   "$$registry" \
	    --schema-dir internal/adapter/outbound/eventbus/schemas || exit 1; \
	done

# schema-verify: fail if any of the 13 expected PascalCase schema names is
# missing from its Glue registry (11 in GLUE_REGISTRY_MEMBERSHIP_NAME, 2 —
# TenantCreated/TrialStarted — in GLUE_REGISTRY_TENANT_NAME per SCHEMA-7).
# Names match domain.TopicForEvent's routing + the LLD §7.3.1 registry-layout
# table. Surfaces a mismatch pre-deploy rather than at first-event publish.
# Requires GLUE_REGISTRY_MEMBERSHIP_NAME/GLUE_REGISTRY_TENANT_NAME and AWS
# credentials.
.PHONY: schema-verify
schema-verify:
	@test -n "$(GLUE_REGISTRY_MEMBERSHIP_NAME)" || { \
	  echo "GLUE_REGISTRY_MEMBERSHIP_NAME is not set — add it to .env"; \
	  exit 1; \
	}
	@test -n "$(GLUE_REGISTRY_TENANT_NAME)" || { \
	  echo "GLUE_REGISTRY_TENANT_NAME is not set — add it to .env"; \
	  exit 1; \
	}
	@missing=""; \
	for name in DepartmentMembershipGranted DepartmentMembershipLevelChanged DepartmentMembershipRevoked MembershipRevoked TenantMembershipsPurged TenantRoleGranted TenantRoleRevoked TenantSeatOverageResolved TenantSeatOverageStarted TenantStateChanged TenderAssigneeOverridden MFAReset; do \
	  if ! aws glue get-schema \
	      --schema-id "RegistryName=$(GLUE_REGISTRY_MEMBERSHIP_NAME),SchemaName=$$name" \
	      --region "$(AWS_REGION)" >/dev/null 2>&1; then \
	    missing="$$missing $(GLUE_REGISTRY_MEMBERSHIP_NAME):$$name"; \
	  fi; \
	done; \
	for name in TenantCreated TrialStarted; do \
	  if ! aws glue get-schema \
	      --schema-id "RegistryName=$(GLUE_REGISTRY_TENANT_NAME),SchemaName=$$name" \
	      --region "$(AWS_REGION)" >/dev/null 2>&1; then \
	    missing="$$missing $(GLUE_REGISTRY_TENANT_NAME):$$name"; \
	  fi; \
	done; \
	if [ -n "$$missing" ]; then \
	  echo "FAIL: missing Glue schemas:$$missing"; \
	  echo "     run 'make schema-register' to create them"; \
	  exit 1; \
	fi; \
	echo "OK: all 14 schemas present across both registries"

# schema-prune: dry-run scan for orphaned Glue schemas in BOTH registries
# (exist in Glue, not in repo). Pass EXECUTE=true to archive and delete:
# make schema-prune EXECUTE=true. Requires GLUE_REGISTRY_MEMBERSHIP_NAME/
# GLUE_REGISTRY_TENANT_NAME and AWS credentials.
.PHONY: schema-prune
schema-prune:
	@test -n "$(GLUE_REGISTRY_MEMBERSHIP_NAME)" || { echo "GLUE_REGISTRY_MEMBERSHIP_NAME is not set"; exit 1; }
	@test -n "$(GLUE_REGISTRY_TENANT_NAME)" || { echo "GLUE_REGISTRY_TENANT_NAME is not set"; exit 1; }
	@for registry in $(GLUE_REGISTRY_MEMBERSHIP_NAME) $(GLUE_REGISTRY_TENANT_NAME); do \
	  echo "Pruning orphaned schemas -> $$registry"; \
	  docker run --rm \
	    -v "$(CURDIR)":/workspace \
	    -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_SESSION_TOKEN \
	    -e AWS_REGION="$(AWS_REGION)" \
	    "$(SCHEMA_GOV_IMAGE)" prune \
	    --registry "$$registry" \
	    $(if $(filter true,$(EXECUTE)),--execute,) || exit 1; \
	done

# -----------------------------
# CLEAN
# -----------------------------

.coverage:
	@mkdir -p .coverage

.PHONY: clean
clean:
	rm -rf bin .coverage
	rm -f coverage.out coverage.html

# -----------------------------
# DOCS
# -----------------------------
# OpenAPI and AsyncAPI specs have different sources of truth:
#   - OpenAPI (docs/swagger/{docs.go,swagger.json,swagger.yaml}) is GENERATED
#     from swag `@Summary`/`@Tags`/`@Router` annotations on handler functions
#     via `make swag` — never hand-edited. `docs.go`'s init() registers it
#     for ginSwagger.WrapHandler to serve; there is no raw-file embed.
#   - AsyncAPI (api/asyncapi.yaml) IS hand-maintained; the service binary
#     embeds it directly via //go:embed in api/embed.go (apispec.AsyncAPISpec) —
#     no synced duplicate copy, so there is nothing to fall out of sync.
# Serves:
#   /              landing page (linking both doc surfaces)
#   /docs          same landing page
#   /swagger       Swagger UI rendering the generated OpenAPI spec (REST — Try it out enabled)
#   /asyncapi      AsyncAPI Studio rendering /asyncapi.yaml (Events — read-only)
#   /asyncapi.yaml raw spec
#
# Run `make swag` after changing handler annotations; api/asyncapi.yaml takes
# effect on the next build/run since it's a direct compile-time embed.

# swag: generate the OpenAPI/Swagger 2.0 spec from // @… annotations on handlers
# under cmd/server and internal/adapter/inbound/http. Mirrors iam-user-profile2's
# pattern so developers moving between services see the same authoring workflow.
# The output under docs/swagger/ is checked into the repo; CI's swag-check target
# fails a PR whose annotations drift from what's on disk.
.PHONY: swag
swag:
	@echo "Generating Swagger docs..."
	$(GO) tool swag init \
	  -g swagger_info.go \
	  -d cmd/server,internal/adapter/inbound/http \
	  --output docs/swagger \
	  --parseDependency \
	  --parseInternal
	@python3 scripts/patch-swagger-extensions.py
	@echo "Swagger docs written to docs/swagger/"

.PHONY: swag-check
swag-check:
	bash .github/scripts/check-swagger-stale.sh

.PHONY: docs-serve
docs-serve: run
	@echo "Docs will be available at:"
	@echo "  http://localhost:8080/            — landing page"
	@echo "  http://localhost:8080/swagger     — Swagger UI (REST APIs)"
	@echo "  http://localhost:8080/asyncapi    — AsyncAPI Studio (Events)"
