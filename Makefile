.PHONY: run build test vet fmt tidy \
        docker-up docker-down docker-logs \
        migrate-up migrate-down migrate-force migrate-version migrate-create \
        health

MIGRATIONS_DIR := internal/db/migrations
DATABASE_URL   ?= $(shell grep -E '^DATABASE_URL=' .env 2>/dev/null | cut -d '=' -f2-)
MIGRATE        := $(shell go env GOPATH)/bin/migrate

## --- App -------------------------------------------------------------

run: ## Run the API locally (reads .env)
	go run ./cmd/api

build: ## Compile the API binary to bin/api
	go build -o bin/api ./cmd/api

test: ## Run all Go tests
	go test ./...

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
