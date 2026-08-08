# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

FounderStack API (Go) is the Go rewrite of `../founderstack-api` (FastAPI/Python) — same
multi-tenant "Headless COO" product, same API surface, same 18-table Postgres schema, same
frontend (`../founderstack-web`, unchanged). See `../WORKFLOW_PLAN_GO.md` for the full
workflow-by-workflow spec this backend is built against; `../founderstack-api/CLAUDE.md`
documents the Python original this mirrors.

Orchestration will run on `anthropics/anthropic-sdk-go`'s built-in Tool Runner for the inner
agent loop, plus a hand-rolled, Postgres-checkpointed `internal/core/graph` package for the
outer planner → RAG → executor → approval → validator → reporter sequence (LangGraph's Go
equivalent). Neither exists yet — this repo currently implements workflow 1 (bootstrap) only:
config loading, local infra, schema migrations, and a `/health` endpoint.

**Only workflow 1 is implemented.** Don't assume routes, tables, or packages from later
workflows in `WORKFLOW_PLAN_GO.md` exist yet — check `internal/api/v1/` for what's actually
registered.

## Commands

```bash
# Run the API locally (reads .env)
make run                    # equivalent to: go run ./cmd/api

# Build a binary
make build                  # outputs bin/api

# Tests / static analysis
make test                   # go test ./...
make vet                    # go vet ./...
make fmt                    # gofmt -w .
make tidy                   # go mod tidy

# Local infra (Postgres :5440, Redis :6379, LocalStack :4566)
make docker-up
make docker-down

# Migrations (golang-migrate — install once:
#   go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest)
make migrate-up
make migrate-down
make migrate-version
make migrate-create NAME=add_thing   # scaffolds internal/db/migrations/NNNN_add_thing.{up,down}.sql

# Hit the running API's health check
make health
```

## Architecture

### Shared local Postgres with the Python backend — read this before touching migrations

`docker-compose.local.yml` pins `name: founderstack-api`, matching the Python repo's
*default* (directory-derived) Compose project name. Running `docker compose up` from either
`founderstack-api/` or `founderstack-api-go/` manages the **same** containers/volumes/network
— there is only one local Postgres, on port 5440, database `founderstack`. This was a
deliberate choice (see conversation history) so both backends can be developed against
identical local data during the rewrite, without doubling up on infra.

**Consequence:** migration ownership for this local database now belongs to
`golang-migrate` (`internal/db/migrations/`), not Alembic. The Python backend's
`alembic_version` bookkeeping table was intentionally dropped from this local DB (along with
its then-current schema) to hand ownership to the Go migration. If you run
`alembic upgrade head` from `../founderstack-api` against this same local DB again, it will
try to recreate tables from scratch and collide with what's already here. If both backends
need to run locally at once going forward, point one of them at a second database (e.g.
`founderstack_go`) rather than resolving the collision by hand again.

### Multi-Tenancy

Every tenant-scoped table carries `org_id` (see `internal/db/migrations/000001_init_schema.up.sql`
for the full 18-table schema, structurally identical to the Python backend's SQLAlchemy
models). Row-Level Security policies are **not** in the migration — same as the Python
backend, they're applied manually per environment (see `WORKFLOW_PLAN_GO.md` workflow 1).
`updated_at` is maintained by a Postgres trigger (`set_updated_at()`, applied to every table)
rather than ORM `onupdate=...` logic, since this backend has no ORM to hook into.

### No ORM — `pgx` directly, not GORM

Deliberate choice over GORM: this schema relies on Postgres RLS policies keyed on `org_id`,
which require a precise `SET LOCAL app.current_org_id = ...` on the transaction before the
first tenant-scoped query. `pgx` gives direct control over transactions and connection-level
session state so that guarantee is enforced at the driver level; GORM's pooling/session
abstraction makes it easy to accidentally run a query on a pooled connection that skipped the
`SET LOCAL`, silently leaking cross-tenant rows. `sqlc` (typed Go generated **from** hand-written
SQL + this migration's schema — the opposite direction of Alembic's autogenerate-from-ORM-models
flow) will be added once there are real queries to generate from (workflow 2+), not before.

### Configuration (`internal/config/config.go`)

`config.Load()` reads `.env` (via `godotenv`, dev convenience only — nothing in production
should depend on a `.env` file existing) then environment variables (via `viper`,
env-wins-over-.env) into a typed `*Config`. Required-but-unset variables are collected and
reported together as one error, not fail-fast on the first miss. Secrets (`CLERK_SECRET_KEY`,
`ENCRYPTION_KEY`, etc.) are wrapped in `internal/pkg/secret.Value`, which redacts itself in
`%v`/`%s`/`%#v` — call `.Expose()` deliberately at the point of use, never log a `Config` struct
directly and expect it to be safe by accident (it is, but don't rely on that reflexively).

`ENCRYPTION_KEY` is reused from the Python backend's Fernet key: base64-decoded it's exactly
the 32 bytes AES-256-GCM needs. The two backends' encrypted vaults are **not** byte-compatible
(different envelope formats), only the key material is shared.

### Health Check (`internal/api/v1/health.go`)

`GET /api/v1/health` probes Postgres (`pgx` ping), Redis (`go-redis` `PING`), and Pinecone
(`ListIndexes`) concurrently, mirroring `founderstack-api/app/api/v1/health.py`. Returns `200`
when the two *critical* dependencies (database, Redis) are healthy — Pinecone is reported but
never fails the overall status. Pinecone check is skipped (not failed) when no
`PINECONE_API_KEY` is configured.

### CORS

`cmd/api/main.go::corsConfig` mirrors the Python backend's policy: wide open in development,
locked to `APP_BASE_URL` + `https://founderstack.ai` in production. Development mode uses
`AllowOriginFunc` returning `true` rather than a literal `AllowOrigins: []string{"*"}`,
because the CORS spec forbids combining a wildcard origin with `AllowCredentials: true` — the
browser rejects it outright.

### Router Layout

| Prefix | File | Purpose |
|--------|------|---------|
| `/api/v1/health` | `internal/api/v1/health.go` | DB + Redis + Pinecone liveness |

(Everything else in `WORKFLOW_PLAN_GO.md` — webhooks, settings, integrations, documents,
agents, workflows, runs — is unbuilt. Add rows here as routers land.)

### Dependency policy

Go dependencies are added when code actually imports them, not pre-installed speculatively
against the full `WORKFLOW_PLAN_GO.md` dependency table — `go mod tidy` strips unused
`require`s anyway, and an unused import is dead weight (and an unreviewed supply-chain
surface) until something calls it. `golang-jwt/jwt`, `anthropic-sdk-go`, `cohere-go`,
`sentry-go`, and `otel` are all planned (see the dependency table in `WORKFLOW_PLAN_GO.md`)
but not yet in `go.mod` — add each when the workflow that needs it is implemented.

## Environment Variables

See `.env.example` for the full list with inline notes on Go-specific deviations from the
Python `.env.example` (DSN scheme, `sslmode=disable`, AES-256-GCM key note). Required (Load
fails listing all that are missing, not just the first): `DATABASE_URL`, `CLERK_SECRET_KEY`,
`CLERK_PUBLISHABLE_KEY`, `CLERK_WEBHOOK_SECRET`, `LOCALSTACK_AUTH_TOKEN`, `PINECONE_API_KEY`,
`ENCRYPTION_KEY`, `OAUTH_STATE_SECRET`.

## Adding a New Feature

1. **New table / schema change**: `make migrate-create NAME=descriptive_name`, write the
   `.up.sql`/`.down.sql` pair by hand (no autogenerate — see "No ORM" above), `make migrate-up`
   against local Postgres to verify it applies cleanly.
2. **New endpoint**: add a handler in `internal/api/v1/`, register it on the `apiV1` group in
   `cmd/api/main.go::newRouter`.
3. **New external dependency**: `go get` it when you write the first line of code that imports
   it, not before (see "Dependency policy" above).
4. **New secret/config value**: add a field to `internal/config/config.go`'s `Config` struct
   (use `secret.Value` for anything sensitive), add it to the `defaults` map in `Load()`, add
   it to `requiredFields` if the app can't run without it, document it in `.env.example`.
