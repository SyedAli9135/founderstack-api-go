.PHONY: run build test test-integration coverage vet fmt tidy install-hooks \
        docker-up docker-down docker-logs \
        migrate-up migrate-down migrate-force migrate-version migrate-create \
        sqlc-generate \
        health

MIGRATIONS_DIR      := internal/db/migrations
DATABASE_URL        ?= $(shell grep -E '^DATABASE_URL=' .env 2>/dev/null | cut -d '=' -f2-)
SYSTEM_DATABASE_URL ?= $(shell grep -E '^SYSTEM_DATABASE_URL=' .env 2>/dev/null | cut -d '=' -f2-)
MIGRATE             := $(shell go env GOPATH)/bin/migrate
SQLC                := $(shell go env GOPATH)/bin/sqlc

# Minimum acceptable total statement coverage (generated code in
# internal/db/dbgen excluded — see the `coverage` target below for why).
# Set a few points below the actual current total (~59.6% as of the tests
# that established this number) so small legitimate additions in
# still-thin areas (cmd/api wiring, health.go) don't fail CI on their own,
# while a real regression (e.g. deleting the webhook tests) still trips it.
COVERAGE_THRESHOLD := 55

## --- App -------------------------------------------------------------

run: ## Run the API locally (reads .env)
	go run ./cmd/api

build: ## Compile the API binary to bin/api
	go build -o bin/api ./cmd/api

test: ## Run all Go tests (fast, no DB — build-tagged integration tests are excluded)
	go test ./...

install-hooks: ## One-time setup: enable the versioned pre-push hook (.githooks/pre-push)
	git config core.hooksPath .githooks
	@echo "Installed. 'git push' now runs 'make coverage' first — skip once with 'git push --no-verify'."

test-integration: ## Run integration tests too, against local Postgres (needs docker-up + migrate-up first)
	TEST_SYSTEM_DATABASE_URL="$(SYSTEM_DATABASE_URL)" go test -tags=integration ./... -v

coverage: ## Run the full suite with coverage and enforce COVERAGE_THRESHOLD (needs docker-up + migrate-up first)
	@TEST_SYSTEM_DATABASE_URL="$(SYSTEM_DATABASE_URL)" go test -tags=integration ./... -coverprofile=coverage.out -covermode=atomic || exit 1
	@grep -v "/internal/db/dbgen/" coverage.out > coverage.filtered.out
	@pct=$$(go tool cover -func=coverage.filtered.out | tail -1 | grep -oE '[0-9]+\.[0-9]+'); \
	echo ""; \
	echo "Total coverage (internal/db/dbgen generated code excluded): $$pct%"; \
	awk -v pct="$$pct" -v threshold="$(COVERAGE_THRESHOLD)" 'BEGIN { \
		if (pct + 0 < threshold + 0) { \
			print "FAIL: " pct "% is below the " threshold "% threshold"; exit 1; \
		} else { \
			print "PASS: " pct "% meets the " threshold "% threshold"; exit 0; \
		} \
	}'

vet: ## Static analysis
	go vet ./...

fmt: ## Format all Go source
	gofmt -w .

tidy: ## Sync go.mod/go.sum with actual imports
	go mod tidy

health: ## Curl the running API's health endpoint
	curl -sS http://localhost:8000/api/v1/health | python3 -m json.tool

## --- Local infra (Postgres/Redis/LocalStack) --------------------------

docker-up: ## Start local Postgres, Redis, LocalStack
	docker compose -f docker-compose.local.yml up -d

docker-down: ## Stop and remove local infra containers
	docker compose -f docker-compose.local.yml down

docker-logs: ## Tail local infra logs
	docker compose -f docker-compose.local.yml logs -f

## --- Migrations (golang-migrate) ---------------------------------------
## Install once: go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest

migrate-up: ## Apply all pending migrations
	$(MIGRATE) -path $(MIGRATIONS_DIR) -database "$(DATABASE_URL)" up

migrate-down: ## Roll back one migration
	$(MIGRATE) -path $(MIGRATIONS_DIR) -database "$(DATABASE_URL)" down 1

migrate-force: ## Force the schema_migrations version (usage: make migrate-force V=1)
	$(MIGRATE) -path $(MIGRATIONS_DIR) -database "$(DATABASE_URL)" force $(V)

migrate-version: ## Print the current migration version
	$(MIGRATE) -path $(MIGRATIONS_DIR) -database "$(DATABASE_URL)" version

migrate-create: ## Scaffold a new migration pair (usage: make migrate-create NAME=add_thing)
	$(MIGRATE) create -ext sql -dir $(MIGRATIONS_DIR) -seq $(NAME)

## --- sqlc (typed Go from internal/db/queries/*.sql) ---------------------
## Install once: go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest

sqlc-generate: ## Regenerate internal/db/dbgen from internal/db/queries + the migration schema
	$(SQLC) generate
