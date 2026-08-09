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
equivalent). Neither exists yet.

**Only workflows 1 (bootstrap) and 2 (Clerk org/user sync) are implemented.** Don't assume
routes, tables, or packages from later workflows in `WORKFLOW_PLAN_GO.md` exist yet — check
`internal/api/v1/` and `internal/api/webhooks/` for what's actually registered.

> **A note on how this codebase relates to the Python original**: match
> `founderstack-api`'s *wire contract* (JSON shapes, field names, status codes, endpoint
> paths — founderstack-web depends on these), not its internal architecture. Where idiomatic
> Go differs from FastAPI/SQLAlchemy patterns, idiomatic Go wins — see "Response Envelope"
> and "No ORM" below for concrete examples of where this repo deliberately departs from how
> the Python backend does the same thing internally.

## Commands

```bash
# Run the API locally (reads .env)
make run                    # equivalent to: go run ./cmd/api

# Build a binary
make build                  # outputs bin/api

# Tests / static analysis
make test                   # go test ./...                          (fast, no DB)
make test-integration       # go test -tags=integration ./... -v     (needs docker-up + migrate-up)
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

# sqlc (install once: go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest)
make sqlc-generate           # regenerate internal/db/dbgen from internal/db/queries/*.sql

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
models). `updated_at` is maintained by a Postgres trigger (`set_updated_at()`, applied to every
table) rather than ORM `onupdate=...` logic, since this backend has no ORM to hook into.

### Row-Level Security (`internal/db/migrations/000002_enable_rls.up.sql`)

Every table has a `tenant_isolation` policy scoped to the Postgres session variable
`app.current_org_id`, read through a `current_org_id()` SQL function that returns `NULL`
(never errors) when the variable is unset or blank. Because `org_id = NULL` is never `true`,
a connection that forgets to `SET LOCAL app.current_org_id = '<uuid>'` sees **zero rows**, not
every row — deny-by-default. Tables without their own `org_id` column
(`agent_team_members`, `workflow_steps`, `approval_decisions`, `document_chunks`) are scoped
transitively via an `EXISTS` subquery against their parent. `organizations` itself is scoped
by its own `id`, since it *is* the tenant root.

RLS is enforced (`FORCE ROW LEVEL SECURITY`) for a new non-superuser role, `app_user`
(created by the same migration — local-dev password only, see the migration file's comments).
**RLS does not apply to superusers or table owners**, so it was a no-op for the `postgres`
role migrations run as. Verified directly against Postgres (not just "the SQL looks right"):
inserted two orgs as `postgres`, then as `app_user` confirmed (a) no session var → 0 rows,
(b) scoped to org A → only org A's rows across a direct table (`users`) and a
transitively-scoped table (`document_chunks`), (c) an `INSERT` targeting org B while scoped
to org A is rejected by the `WITH CHECK` clause.

**The API's runtime connection pool now connects as `app_user`** (`cmd/api/main.go`, via
`config.AppDatabaseURL` / the `APP_DATABASE_URL` env var — kept distinct from `DatabaseURL`,
which stays the `postgres` superuser DSN used only by the `migrate` CLI). Confirmed live via
`pg_stat_activity`: the running server's connection shows as `app_user`, not `postgres`. The
health check still passes because `Ping` doesn't touch any RLS-protected table.

A second role, `app_system` (`BYPASSRLS`, migration `000003_add_system_role.up.sql`), is now
in real use: `cmd/api/main.go` opens a second `pgxpool.Pool` against
`config.SystemDatabaseURL` / `SYSTEM_DATABASE_URL`, and `internal/api/webhooks/clerk.go` is
the first consumer — inserting a brand-new `organizations` row (an org this backend has never
seen before) is inherently something the RLS-scoped `app_user` pool cannot do: its own
`WITH CHECK (id = current_org_id())` policy would reject the insert, since there's no
tenant-context session variable to set for an org that doesn't exist yet.

**Still genuinely deferred:** nothing calls `SET LOCAL app.current_org_id = $1` yet, because
that requires an *authenticated request's* org_id, which doesn't exist as a concept until
real user-facing auth lands (workflow 3+, BYOK settings is the first endpoint that reads the
Clerk JWT). The webhook is a system context by design — it doesn't need per-request tenant
scoping, it *is* the thing that creates tenants. Don't reuse `app_user` for future
system-context work (schedulers, admin tooling); a bug there should fail loud via a rejected
query, not silently return zero rows.

### No ORM — `pgx` + `sqlc`, not GORM

Deliberate choice over GORM: this schema relies on Postgres RLS policies keyed on `org_id`,
which require a precise `SET LOCAL app.current_org_id = ...` on the transaction before the
first tenant-scoped query. `pgx` gives direct control over transactions and connection-level
session state so that guarantee is enforced at the driver level; GORM's pooling/session
abstraction makes it easy to accidentally run a query on a pooled connection that skipped the
`SET LOCAL`, silently leaking cross-tenant rows.

`sqlc` generates typed Go structs and query methods **from** hand-written SQL + the migration
schema — the opposite direction of Alembic's autogenerate-from-ORM-models flow, since there's
no ORM model to diff against. SQL stays the source of truth:

1. Write/edit a query in `internal/db/queries/*.sql` (one file per feature area, e.g.
   `clerk_sync.sql`), using a `-- name: QueryName :one|:many|:exec|:execrows` comment per
   [sqlc's convention](https://docs.sqlc.dev/en/stable/reference/query-annotations.html).
2. `make sqlc-generate` (config: `sqlc.yaml`, targets the `pgx/v5` driver) — regenerates
   `internal/db/dbgen/` (package `dbgen`, gitignored-nothing — generated code is committed,
   same as the migrations it's generated from).
3. Call it from a handler via `dbgen.New(pool)`, where `pool` is whichever `*pgxpool.Pool`
   matches the trust boundary the query needs (`app_user` for ordinary tenant-scoped requests,
   `app_system` for cross-tenant system contexts — see "Row-Level Security" above).

`internal/db/dbgen/*.go` is generated — edit the `.sql` and regenerate, never hand-edit the
output.

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

### Response Envelope (`internal/api/response/response.go`)

Every non-health-check response uses one of two JSON shapes — this is a wire-contract match
with the Python backend's `SuccessEnvelope[T]`/`ErrorEnvelope` (founderstack-web expects
these exact shapes), not an architectural port:

- `response.OK(c, status, message, data)` → `{"status": "success", "message": ..., "data": ...}`
- `response.Fail(c, status, code, message)` → `{"status": "error", "error": {"code": ..., "message": ..., "request_id": ...}}`

What's *not* ported: FastAPI's three global exception handlers
(`validation_exception_handler`/`http_exception_handler`/`global_exception_handler`), which
exist because Python routes control flow through raised exceptions that need centralized
translation. Idiomatic Go doesn't have that problem — handlers check errors and call
`response.Fail` explicitly at the point of failure, the same way any Go function handles an
`error` return. `middleware.Recovery` exists only as a last-resort safety net for genuine
panics (nil derefs, etc.), not as the primary error path. One upside of not centralizing:
each call site picks its own specific error code (`"ORG_NOT_FOUND_YET"`,
`"MISSING_SVIX_HEADERS"`, ...) instead of every manually-raised error collapsing to Python's
generic `"HTTP_EXCEPTION"`.

`middleware.RequestID` assigns a fresh opaque ID per request (always generated, never trusts
a client-supplied `X-Request-ID` — accepting one would let a caller inject arbitrary values
into server logs), stores it for `response.Fail` to attach, and echoes it in the response
header.

`GET /api/v1/health` is the one deliberate exception — it returns a flat, unwrapped JSON body
(`{"status": ..., "checks": {...}}`), matching the Python original, because health/liveness
probes conventionally expect a flat shape, not an application-level envelope.

### Clerk Webhook Sync (`internal/api/webhooks/clerk.go`)

`POST /api/webhooks/clerk` handles `organization.created`/`.updated`,
`organizationMembership.created`/`.updated`/`.deleted`, `user.updated`/`.deleted`, and
`organization.deleted` — eight event types total.

**Deliberately unhandled** (falls through to the `default` case, acked with 200 so Clerk
doesn't retry, nothing written) — not gaps, considered and rejected:
- `session.*` — the `sessions` table exists in the schema (inherited from the Python models)
  but nothing reads it; no endpoint or workflow consumes session data yet. Populating it now
  would be write-only data with no consumer.
- `organizationInvitation.*` — no `invitations` table exists to track pending-invite state,
  and there's nothing missing functionally either: an accepted invitation is exactly what
  fires `organizationMembership.created`, which is already handled. Note this is also the
  likely explanation if "add an existing user to an org" via the Clerk dashboard appears to do
  nothing — the dashboard's "invite" action creates an `OrganizationInvitation` first, and
  `organizationMembership.created` only fires once that invite is *accepted*, not when it's
  sent. A direct "add existing member" action (no invite/acceptance step), if the Clerk
  instance's UI exposes one, fires the membership event immediately instead.
- `user.created` — fires before a user has joined any org. `users.org_id` is `NOT NULL` in
  this schema, so there is no valid row to create at that point — matches the Python
  original's choice to skip this event too.

Signature verification (`internal/pkg/svix`) is a ~90-line hand-rolled implementation of
Svix's HMAC-SHA256 scheme against stdlib `crypto/hmac` — not the `svix-webhooks` SDK — with
its own test suite (`verify_test.go`) covering wrong secret, tampered body/id, replay (stale
timestamp), and malformed-header cases, since this is the one piece of workflow 2 where a
subtle bug is a real security hole, not just a functional bug.

Idempotency is UPSERT-based (`INSERT ... ON CONFLICT DO UPDATE` on `clerk_org_id` /
`clerk_user_id`), not a separate delivery-ID dedup table — a redelivered or out-of-order
event just re-applies the same write harmlessly. Covered by
`TestClerkWebhook_FullLifecycle/replaying_organization.created_is_idempotent` in
`clerk_integration_test.go`: replaying `organization.created` produces the same single row,
not a duplicate.

`organizationMembership.created` arriving before its org's `organization.created` (a real
possibility — Clerk doesn't guarantee delivery order) returns `422 ORG_NOT_FOUND_YET`, which
tells Clerk/Svix to retry with backoff; this self-heals once the org event lands. Covered by
the same test file's first subtest.

Each delivery is bounded by a 10s `context.WithTimeout` (`handlerTimeout` in `clerk.go`) —
Gin sets no request deadline of its own, so without this a hung DB call would block the
handling goroutine indefinitely.

**Deliberate deviation from the Python backend's actual behavior, on all three delete-ish
events**: `organization.deleted`, `organizationMembership.deleted`, and `user.deleted` are all
soft-deletes (`is_active = false`) here, not the Python webhook's hard `DELETE`s. Three
reasons: (1) it matches every other delete-ish endpoint's convention in this product (agents,
documents — soft-delete, never hard), (2) `workflow_runs` and `approvals` reference
`organizations.id` **without** `ON DELETE CASCADE`, so a hard delete of an org would raise a
foreign-key violation the moment it has any run history — a real bug in the Python original
that just hasn't been hit by test data yet, and (3) it preserves `audit_logs`/`cost_ledger`
history instead of destroying it. `organizationMembership.deleted` and `user.deleted` both
resolve to the same `SoftDeleteUserByClerkUserID` query — a membership removal and a full
account deletion are different Clerk events but the same local action, since a `users` row is
scoped to exactly one `org_id` in this schema: once that membership is gone, so is any reason
for the row to stay active. Neither cascades beyond the one row, same as `organization.deleted`.

### Testing Strategy & CI

Two tiers, split by a Go build tag rather than a runtime check, so the default `go test ./...`
stays fast and dependency-free:

- **Unit tests** (`*_test.go`, no build tag) — pure functions only, no DB, no network. Run on
  every `go test ./...`. Example: `internal/pkg/svix/verify_test.go` (security-critical, so it
  gets its own thorough table of attack cases), `internal/api/webhooks/clerk_test.go`
  (`normalizeRole`, `nilIfEmpty`).
- **Integration tests** (`*_integration_test.go`, `//go:build integration`) — exercise the
  real HTTP handler via `httptest` against a real Postgres, connected as whichever role
  production actually uses for that handler (`app_system` for the webhook). Excluded from a
  plain `go test ./...`; run via `go test -tags=integration ./...` (`make test-integration`
  locally, or CI). These are what turn a one-time manual `curl`+`psql` verification session
  into something that keeps being true — see `clerk_integration_test.go`'s doc comment.

When adding a new handler with real DB-write logic, add both: unit tests for any pure
helper logic (payload parsing, business rules), and an integration test file covering the
same scenarios you'd otherwise verify by hand — replay/idempotency, error paths, and any
"this looks obviously right" edge case, since those are exactly the ones that regress
silently.

**CI** (`.github/workflows/ci.yml`) runs on every push/PR: `go build`, `go vet -tags=integration`
(so the integration test file is also type-checked, even though it doesn't execute without the
tag), a `gofmt -l` check, then spins up a throwaway Postgres service container, runs the real
migrations against it from empty, and runs `make coverage` — the exact same command a
developer runs locally, just against a fresh ephemeral DB instead of the local dev one. This
is CI in the "automated build/test gate" sense only — there is no CD step, no deployment
target, and none is needed yet (see conversation history: no budget, no deployment target
exists currently). Costs nothing at this project's scale (GitHub Actions' free tier is 2,000
minutes/month for private repos; this suite runs in well under a minute).

**Coverage gate** (`make coverage`, `COVERAGE_THRESHOLD` in the `Makefile`, currently `55`):
runs the full tagged suite with `-coverprofile`, strips `internal/db/dbgen` (sqlc-generated —
testing generated code directly isn't meaningful; it's already exercised indirectly through
the handler integration tests that call it) out of the profile, and fails if the remaining
total statement coverage drops below the threshold. The threshold was set a few points below
the actual total at the time it was introduced (~59.6%), not at 100% or an arbitrary round
number — low enough that small legitimate additions in still-thin areas (`cmd/api`'s
`run()`/`newRouter()` wiring, `internal/api/v1/health.go`, both accepted, documented gaps
rather than hidden ones) don't fail CI on their own, high enough that a real regression (e.g.
deleting the webhook tests) still trips it immediately.

**Important nuance**: neither CI nor the coverage gate can literally block a `git push` — a
push to a remote you have write access to always succeeds; what CI failing does is mark that
commit's check red, which only *prevents merging* if branch protection rules on `main`
require the check to pass (not currently configured — solo-dev repo, direct-to-main today).
The one thing that *can* stop a push from completing locally is a client-side git hook — see
below — and even that is trivially bypassed with `git push --no-verify`. Treat CI as the
authoritative, unavoidable gate and the local hook as a fast-feedback convenience, not the
other way around.

**Local pre-push hook** (`.githooks/pre-push`, not enabled by default — `make install-hooks`
once per clone to opt in, via `git config core.hooksPath .githooks` rather than the
untracked, per-clone `.git/hooks/`). Checks local Postgres is reachable (clear error pointing
at `make docker-up` if not, rather than a cryptic connection failure) and then runs the same
`make coverage`. Purely a local convenience for fast feedback before a push leaves your
machine — CI runs the authoritative version of the same check regardless.

### Router Layout

| Prefix | File | Purpose |
|--------|------|---------|
| `/api/v1/health` | `internal/api/v1/health.go` | DB + Redis + Pinecone liveness |
| `/api/webhooks/clerk` | `internal/api/webhooks/clerk.go` | Clerk org/user sync (workflow 2) |

(Everything else in `WORKFLOW_PLAN_GO.md` — settings, integrations, documents, agents,
workflows, runs — is unbuilt. Add rows here as routers land.)

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
fails listing all that are missing, not just the first): `DATABASE_URL`, `APP_DATABASE_URL`,
`SYSTEM_DATABASE_URL`, `CLERK_SECRET_KEY`, `CLERK_PUBLISHABLE_KEY`, `CLERK_WEBHOOK_SECRET`,
`LOCALSTACK_AUTH_TOKEN`, `PINECONE_API_KEY`, `ENCRYPTION_KEY`, `OAUTH_STATE_SECRET`. The three
`*_DATABASE_URL` vars connect as three different Postgres roles for three different trust
boundaries — see "Row-Level Security" above before adding a fourth use case to any of them.

## Adding a New Feature

1. **New table / schema change**: `make migrate-create NAME=descriptive_name`, write the
   `.up.sql`/`.down.sql` pair by hand (no autogenerate — see "No ORM" above), `make migrate-up`
   against local Postgres to verify it applies cleanly.
2. **New query**: add it to a `.sql` file under `internal/db/queries/` (new file per feature
   area, matching existing ones like `clerk_sync.sql`), `make sqlc-generate`, use the
   generated `dbgen.Queries` method — see "No ORM" above.
3. **New endpoint**: add a handler package under `internal/api/`, register it on the
   appropriate route group in `cmd/api/main.go::newRouter`. Use `response.OK`/`response.Fail`
   for every response — see "Response Envelope" above. Decide which `*pgxpool.Pool` it needs
   (`app_user` for ordinary tenant-scoped requests, `app_system` only for genuine cross-tenant
   system contexts) before wiring it in `main.go`.
4. **New external dependency**: `go get` it when you write the first line of code that imports
   it, not before (see "Dependency policy" above).
5. **New secret/config value**: add a field to `internal/config/config.go`'s `Config` struct
   (use `secret.Value` for anything sensitive), add it to the `defaults` map in `Load()`, add
   it to `requiredFields` if the app can't run without it, document it in `.env.example`.
