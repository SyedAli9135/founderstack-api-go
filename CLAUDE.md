# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

FounderStack API (Go) is the Go rewrite of `../founderstack-api` (FastAPI/Python) — same
multi-tenant "Headless COO" product, same API surface, same 18-table Postgres schema, same
frontend (`../founderstack-web`, unchanged). See `../WORKFLOW_PLAN_GO.md` for the full
workflow-by-workflow spec this backend is built against; `../founderstack-api/CLAUDE.md`
documents the Python original this mirrors.

Orchestration will run on a **hand-rolled, provider-agnostic state machine** for the inner
agent loop (decided 2026-08-21, after studying Goose AI's architecture — supersedes an earlier
plan to use `anthropics/anthropic-sdk-go`'s built-in Tool Runner, which is Anthropic-specific
and would have locked orchestration to one provider), plus a hand-rolled, Postgres-checkpointed
`internal/core/graph` package for the outer planner → RAG → executor → approval → validator →
reporter sequence (LangGraph's Go equivalent). Neither exists yet (workflow 9).

BYOK is **not** Claude-only: `internal/core/llm` validates and stores keys for 5 providers —
Anthropic, OpenAI, Google Gemini, Qwen, and DeepSeek (generalized 2026-08-21 from an
Anthropic-only original). See "BYOK API Keys" below.

**Only workflows 1 (bootstrap), 2 (Clerk org/user sync), 3 (BYOK API key management), 4 (connect
integration — OAuth/API-key), 5 (MCP tool gateway), 6 (document upload / RAG), and 7 (agent
configuration) are implemented.** Don't assume routes, tables, or packages from later workflows
in `WORKFLOW_PLAN_GO.md` exist yet — check `internal/api/v1/`, `internal/api/webhooks/`,
`internal/api/settings/`, `internal/api/identity/`, `internal/api/integrations/`,
`internal/api/documents/`, and `internal/api/agents/` for what's actually registered. Workflow 7
is configuration CRUD only — no agent actually runs, calls an LLM, or executes a tool yet; that's
workflow 9's job (`internal/core/graph`, not built). Workflow 4's code is complete and
tested, but nothing will actually connect to a live third party until real OAuth app credentials
are registered on each provider's dashboard and put in `.env` — see "Third-Party Integrations
(workflow 4)" below and its "Status" note in `WORKFLOW_PLAN_GO.md`.

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

**No longer deferred as of workflow 3**: `internal/db/tenant.WithTx` is the one correct way to
run a tenant-scoped operation against `app_user` — it opens a transaction, sets
`app.current_org_id` via `set_config(..., true)` (not a string-built `SET LOCAL`, which
doesn't support real parameterization), runs the caller's queries, commits. Every handler
under `internal/api/settings/` goes through it; nothing queries `app_user` directly outside
of it. Verified against real Postgres (`internal/db/tenant/tenant_integration_test.go`) doing
exactly what §Row-Level Security above only asserted by hand: scoped to org A, a query sees
org A and not org B; a query against `app_user` with no `WithTx` around it sees nothing at
all, not everything.

Don't reuse `app_user` (or `tenant.WithTx`) for system-context work (the webhook, a future
scheduler); that's what `app_system` is for. A bug in a system-context handler should fail
loud via a rejected query, not silently return zero rows because someone reached for the
wrong pool.

### Authentication (`internal/api/middleware/auth.go`)

`middleware.RequireAuth(systemPool, cfg)` is the gate every tenant-scoped route sits behind
(currently just `internal/api/settings/`). Mirrors founderstack-api's
`get_current_user` → `get_current_org` dependency chain — same two failure cases in the same
order (`clerk_user_id` not found/inactive in our `users` table → `USER_NOT_SYNCHRONIZED`;
resolved `org_id` not found/inactive → `ORGANIZATION_NOT_FOUND`), same idea that the *local*
DB row (not whatever the JWT happens to claim) is the source of truth for whether someone
still has access. **One real improvement over the Python original**: that implementation
explicitly decodes the JWT with signature verification turned off
(`jwt.decode(token, options={"verify_signature": False})`) and relies on nothing upstream
having tampered with it; this verifies the signature for real, against Clerk's JWKS, via the
official `clerk-sdk-go`. JWKs are cached by `kid` in-process (`jwkCache`, per Clerk's own
recommendation — "cache and only invalidate on an unrecognized kid," not fetch every request)
rather than relying on undocumented caching inside the SDK's higher-level middleware helpers.

Resolving identity (JWT → `clerk_user_id` → local user → local org) runs on `systemPool`
(`app_system`), not `app_user` — the same chicken-and-egg reasoning as the webhook's org
creation: you can't RLS-scope a query by an org_id you're still in the process of
discovering. Once `RequireAuth` succeeds, it stores the resolved identity on the request via
`internal/api/authctx`, and everything downstream switches to `app_user` + `tenant.WithTx`.

**Dev token fallback** (`internal/pkg/devtoken`, `POST /api/v1/auth/dev-token`): the Python
original's dev-token utility works by minting an *unsigned* JWT, which only functions because
that backend's auth skips signature verification entirely. This one doesn't skip it, so a
Python-style token would just be rejected. Instead this is a genuinely separate, parallel
dev-only auth path — a short-lived HS256 token signed and verified under one shared secret
(`DEV_TOKEN_SECRET`, optional, absent from `requiredFields` — leave it unset anywhere real).
`RequireAuth` only attempts `devtoken.Verify` as a fallback *after* real Clerk verification
has already failed, and only when `!cfg.IsProduction() && cfg.DevTokenSecret` is set — so
production behaves identically whether or not the code path exists. The endpoint itself
responds `404` (not `403`) when disabled, so an unauthenticated prod caller can't even
confirm the route exists.

### BYOK API Keys (`internal/core/llm`, `internal/pkg/vault`, `internal/api/settings`)

- **`vault`** — `Encrypt`/`Decrypt` via AES-256-GCM. `ENCRYPTION_KEY` is reused from
  founderstack-api's Fernet key: base64-decoded, a Fernet key is exactly the 32 raw bytes
  AES-256 needs. Confirmed against the real `.env` value, not just a synthetic test key. The
  two backends' ciphertexts are **not** interchangeable — Fernet's envelope (version byte,
  timestamp, IV, HMAC) differs from GCM's (nonce + ciphertext + auth tag) — only the key
  material is shared. Decoded once at startup (`cmd/api/main.go`) so a misconfigured key
  fails the process at boot, not silently on a founder's first key submission.
- **`llm`** — **generalized 2026-08-21 from Anthropic-only to 5 providers**
  (`Catalog` in `internal/core/llm/catalog.go`: Anthropic, OpenAI, Google Gemini, Qwen,
  DeepSeek — each with a display `Name` and a real-key-format `KeyPrefix`). `ValidateKey`
  mirrors the Python original's dual-path check (mock-prefix short-circuit vs. a real, cheap
  network call) but now dispatches per provider through a `verifiers` map
  (`internal/core/llm/verify.go`): Anthropic uses the SDK's `Models.List(limit=1)`; OpenAI,
  Qwen (DashScope's compatible-mode endpoint), and DeepSeek share one
  `verifyOpenAICompatible(modelsURL)` (Bearer-authed `GET .../models` — no SDK, same
  "don't add a dependency a single GET doesn't justify" reasoning as Stripe's `ValidateKey`);
  Gemini gets its own `verifyGemini` (key passed as a `?key=` query param, not a Bearer header,
  and a bad key returns HTTP `400`, not `401`/`403`, unlike every other provider here).
  The mock-prefix check now runs *before* the format check (reordered from the original
  single-provider version) since one shared `API_KEY_MOCK_PREFIX` can't satisfy every
  provider's real format at once (Anthropic's `sk-ant-` vs. Gemini's `AIza`, ...).
  Distinguishes two failure modes the Python version conflates: `ErrKeyRejected` (provider
  said no — the founder's problem, → `400`) vs. `ErrValidationUnavailable` (couldn't reach the
  provider at all — not the founder's problem, → `503`). The OpenAI-compatible/Gemini
  request-shape logic (status-code mapping, header/query construction) is covered by
  `verify_test.go` against fake `httptest` servers, not live third-party APIs — same
  flakiness/cost-liability reasoning as `verifyAnthropic` staying manually-verified-only.
  `GetClient` stays **Anthropic-only by design**: nothing calls it yet (agent execution is
  workflow 9), and building real per-provider chat/tool-call clients is the hand-rolled state
  machine's job (see Project Overview above), not this BYOK key-management package's — it
  stops at "is this key real, and can I decrypt it back."
- **`settings`** — the 3 BYOK endpoints, now provider-aware: `POST /api/v1/settings/api-key`
  takes an optional `provider` field (**defaults to `"anthropic"` when omitted** — preserves
  founderstack-web's existing `ApiKeyForm`, which predates multi-provider and never sends this
  field — a real wire-contract compatibility requirement, not a guess); `GET .../status` and
  `DELETE .../api-key` take an optional `?provider=` query param with the same default. An
  unrecognized provider string is a `400 UNKNOWN_PROVIDER`, not silently treated as "no key."
  Multiple providers' keys coexist per org (`api_key_registry` has one row per
  `(org_id, provider)`); `organizations.llm_provider` + `active_api_key_id` track which single
  provider is currently "active" (whichever was submitted most recently). **Real bug caught
  and fixed during generalization**: the original single-provider `DELETE` unconditionally
  cleared `organizations.active_api_key_id`, which would have been silently wrong with
  multiple providers — deleting a non-active provider's key would clobber a different,
  still-active provider's pointer. Fixed via `ClearOrganizationActiveApiKeyForProvider`, which
  only clears the org's active pointer when the deleted provider *is* `organizations.llm_provider`
  — covered by `TestSettingsAPIKey_MultiProvider`'s
  `deleting_the_non-active_provider's_key_does_not_clobber_the_active_one` case.
  `organization.deleted`-style soft behavior still applies: `DELETE` deactivates
  (`is_valid=false`), never removes the row. Fixed a real gap in the Python original along the
  way: `api_key_registry` had no `UNIQUE(org_id, provider)` constraint despite a comment
  stating "we enforce one Anthropic key per org" — nothing actually enforced it, so a race
  between two concurrent submissions could create two rows. Migration
  `000004_api_key_registry_unique_org_provider.up.sql` adds the constraint the stated intent
  already assumed existed (now doing real work with 5 providers instead of 1), enabling a real
  `INSERT ... ON CONFLICT` upsert instead of Python's racy read-then-write.

  **`GET /api/v1/settings/api-key/providers`** (added alongside the frontend's multi-provider
  UI, 2026-08-21) merges `llm.Catalog` with the org's `ListKeyStatuses` rows — every one of the
  5 providers always appears, `is_configured`/`is_valid`/`is_active` false and `key_prefix` nil
  for ones the org hasn't touched. Same catalog+status merge shape as
  `internal/api/integrations.Handler.ListIntegrations`, deliberately: one request gives the
  frontend everything it needs to render a provider picker/status grid, instead of the
  `provider`-scoped `GET .../status` (kept as-is, still defaults to `anthropic`, still used
  nowhere in the new frontend flow but not removed — an existing, working, backward-compatible
  endpoint costs nothing to leave in place).

### Third-Party Integrations (workflow 4) — `internal/core/integrations`, `internal/api/integrations`

8 catalog entries (`internal/core/integrations/catalog.go`): Slack, Discord, Notion, Google
Drive, Google Calendar, LinkedIn via OAuth 2.0; Stripe, GitHub via a founder-pasted API
key/PAT. **Twitter/X was deliberately dropped** (no free developer tier as of Feb 2026 —
pay-per-use only); **Telegram and QuickBooks Online were both built, manually verified
end-to-end against real credentials, then dropped** at the founder's discretion (2026-08-15).
QuickBooks specifically was cut after friction with Intuit's account-verification requirements
during signup outweighed its value — it was never part of the original "Core 5" (workflow 5's
finance tools were scoped to Stripe, not QuickBooks), so nothing was actually lost. Dropping
QuickBooks also reverted `OAuthProvider.ExchangeCode` and `TokenValidator.ValidateToken` to
simpler signatures (see the interface segregation note below) — QuickBooks' `realmId` quirk was
the only reason those interfaces carried extra parameters every other provider had to ignore.
For both drops, code/tests/docs were fully removed rather than left disabled, so there's
nothing stale to trip over if either is reconsidered later — re-adding one is a new
`providers/*.go` file, one `catalog.go` line, one `main.go` registry line (plus, for QuickBooks
specifically, re-widening those two interfaces again).
**LinkedIn is scoped to just the free
"Share on LinkedIn" product** (`w_member_social`), not the paid, partner-gated Marketing
Developer Platform — both decisions driven by the same solo-founder/no-funding constraint that
shaped the rest of this catalog; see `WORKFLOW_PLAN_GO.md`'s workflow 4 implementation note for
the full reasoning, including why Google Drive is scoped to `drive.file` (avoids Google's paid
CASA security assessment).

**`POST /api/v1/integrations/{service}/connect` is one endpoint for every auth type, not two**
(fixed 2026-08-16, found while syncing `founderstack-web` against this backend). It was
originally split into a separate `/connect` (OAuth) and `/api-key` (Stripe/GitHub) — a
deviation from `founderstack-api`'s actual wire contract, where `connect_integration` has always
been a single route dispatching internally on `auth_type`. `founderstack-web`'s
`useConnectIntegration` was built correctly against that real contract and always posts to
`/connect` regardless of service — so the split silently broke the key-based connect flow in the
*actual UI* (curl testing against `/api-key` directly never caught this, since manual curl calls
bypassed the frontend's real call path entirely). `Handler.Connect` now branches internally on
`catalog.Meta.AuthType`: `oauth` ignores the body and returns `redirect_url`; `api_key`/`pat`
reads `{ key }` from the body. This is the general lesson, not just this one bug: verify a wire
contract against what the actual frontend sends, not just against what a handler accepts in
isolation.

**Provider interface segregation, not one fat interface** (`internal/core/integrations/types.go`):
`Provider` (just `Name()`) is the only thing every catalog entry implements. `OAuthProvider`,
`Refreshable`, `Revocable`, `TokenValidator`, and `KeyProvider` are separate small interfaces a
provider implements only where it genuinely applies — GitHub's PAT has no `RevokeToken`, Notion
has no public revocation API, Slack bot tokens don't expire so `slack.go` never implements
`Refreshable`. Every call site (the HTTP handlers, the refresh job) resolves a `Provider` via
`Registry.Get(service)` and then type-asserts the capability it needs
(`provider.(integrations.Refreshable)`, ...) rather than switching on the service name. This is
the actual open/closed lever for the package: adding integration #11 is a new
`providers/*.go` file implementing the interfaces it needs, one line in `catalog.go`, and one
line in `cmd/api/main.go::newIntegrationsRegistry`'s constructor call — zero diffs to
`handler.go`, `state.go`, `tokenstore.go`, or `refresh.go`.

`Registry` (`internal/core/integrations/registry.go`) is built once, explicitly, in
`newIntegrationsRegistry` (`cmd/api/main.go`) — deliberately not `init()`-based self-registration
(the pattern `database/sql` drivers use): every provider here is first-party, in this one
module, so there's no genuine plugin/external-driver need that self-registration's hidden
control flow would be earning its keep for.

**Token storage** (`internal/core/integrations/tokenstore.go`): a `Token`
(access/refresh/expiry/scopes/provider-specific `Extra` map) is JSON-marshaled into one envelope
and encrypted with the existing `vault.Encrypt` (same AES-256-GCM, same key as BYOK) into
`mcp_connections.encrypted_credentials`. `Token.Extra` is currently unused by every provider in
the catalog (its one real use case, QuickBooks' `realmId`, was removed — see above) but is kept
as a zero-cost extension point: a nil map costs nothing, and a future provider needing one odd
extra field wouldn't require a new `mcp_connections` column, just a value in this map. Migration
`000005_mcp_connections_unique_org_service.up.sql` adds `UNIQUE(org_id, service_name)` (the same
gap-fix pattern as `000004` for `api_key_registry`), enabling a real `INSERT ... ON CONFLICT`
upsert instead of a racy read-then-write.

**CSRF state** (`internal/core/integrations/state.go`): `StateManager.Generate`/`.Verify`, not
free functions — holds the Redis client and decoded `OAUTH_STATE_SECRET`, built once in
`main.go`, matching every other handler's dependency-injection shape in this codebase. A
callback request carries no JWT (the provider redirects the browser here directly), so
`org_id`/`service` are recovered from a Redis-backed, one-time-use, HMAC-signed nonce rather
than trusted from the callback URL — `Verify` checks the signature *before* touching Redis, so a
forged/garbage state costs nothing but CPU, and deletes the Redis entry immediately on lookup so
a replayed callback fails on its second attempt (covered by an integration test).

**Provider-specific wrinkles worth knowing about before touching a provider file:**
- **Slack** (`providers/slack.go`) is hand-implemented with `net/http`, not
  `golang.org/x/oauth2` — every Slack Web API response, including a *failed* token exchange, is
  HTTP 200 with an `"ok": false` body, which `x/oauth2`'s `Exchange` would silently treat as a
  successful (empty) token.
- **Notion** has no public revoke endpoint — `providers/notion.go` doesn't implement `Revocable`,
  so `DELETE .../notion` deactivates locally only (best-effort, by design, via the type
  assertion in the handler — not a gap).
- **LinkedIn**'s `ValidateToken` uses token introspection
  (`POST /oauth/v2/introspectToken`), not a resource API — with only `w_member_social` granted
  there's no profile endpoint the token is actually authorized to call, but introspection is
  client-authenticated rather than scope-gated, so it works regardless.

**Background refresh job** (`internal/core/integrations/refresh.go`): `RunRefreshJob` runs on
`app_system` (BYPASSRLS) directly, never `tenant.WithTx` — scanning expiring connections across
every org is inherently cross-tenant, same reasoning as the Clerk webhook's org creation. Started
as a goroutine from `cmd/api/main.go::run`, cancelled on the same SIGINT/SIGTERM context the HTTP
server shuts down on. Runs once immediately at startup (a connection that expired while the
process was down shouldn't wait a full 30-minute tick to be caught), then every `RefreshInterval`
(30 min) for any connection expiring within `refreshWindow` (10 min). A refresh response never
re-sends provider-specific `Extra` fields — both the job and the manual `GET .../status` refresh
path preserve the existing connection's `Extra` rather than dropping it (currently a no-op for
every provider in the catalog, but correct and free to keep — see `Token.Extra`'s note above).

**Testing**: unit tests cover every provider's request/response-shape logic (Slack's `ok`-field
handling, LinkedIn's introspection `active` branch, ...) via a shared HTTP-interception harness
(`internal/core/integrations/providers/oauth_flow_test.go`) that reroutes requests for real
provider hostnames to local `httptest` servers — chosen over either hardcoding fake endpoints
into the provider files or skipping this logic's coverage entirely, since a live-third-party-API
dependency in the test suite is exactly the flakiness/cost liability `llm.go`'s `ValidateKey`
doc comment already argues against, and this logic (JSON field names, header construction,
Slack's HTTP-200-with-`ok:false` convention) is real "not obviously right" surface worth locking
down. Integration tests (`internal/api/integrations/handler_integration_test.go`,
`internal/core/integrations/refresh_integration_test.go`) run the full HTTP lifecycle and the
refresh job against real Postgres + Redis, with fake `Provider` implementations standing in for
Slack/Stripe — same reasoning, same pattern. One real bug this caught before it
shipped: `GET .../status` originally re-validated a token against its provider unconditionally,
which meant a locally revoked connection (`DELETE .../{service}`) could flip back to `"connected"`
if the provider's own validation call didn't happen to detect revocation — fixed by checking
`IsActive` first and short-circuiting to the stored status.

### MCP Tool Gateway (workflow 5) — `internal/core/mcp`, `internal/core/mcp/servers`, `cmd/seedtools`

8 tool servers — every workflow-4 integration now has one: the original "Founder's Core 5"
(Stripe, Slack, GitHub, Notion, LinkedIn) plus Discord, Google Drive, and Google Calendar (added
2026-08-23, a deliberate widening beyond the original Core-5 scope at the founder's request).
Each is a real `*mcp.Server` from the official `modelcontextprotocol/go-sdk/mcp`, built via
`servers.New{Stripe,Slack,GitHub,Notion,LinkedIn,Discord,GoogleDrive,GoogleCalendar}Server()`.
18 tools total across them.

**Every tool server is connected over a real MCP session, not called as a bare Go function.**
`internal/core/mcp.NewRegistry` wires each server to a paired in-process `*mcp.Client` via
`mcp.NewInMemoryTransports()` — the same `client.CallTool()` path a subprocess-backed or remote
MCP server would receive. Deliberate design choice (2026-08-23, after reading Goose's and
DeepSeek Harness's architectures — see `WORKFLOW_PLAN_GO.md` workflow 5's design note): moving a
tool server from in-process to a real subprocess later is a transport swap
(`mcp.NewInMemoryTransports()` → `mcp.NewCommandTransport(...)`), not a rewrite of the tool or
`Gateway`.

**Credentials travel via MCP's `_meta`, not `context.Value` — this was a real bug, caught by a
failing test.** The original design set the org's decrypted integration token on the client's
`context.Context` before calling `CallTool`, assuming an in-process transport meant no real
serialization boundary. Wrong: `InMemoryTransport` is genuine newline-delimited JSON framing
("in-memory" only means no subprocess/socket, not no serialization) — the server-side handler's
context comes from its own session's message-read loop, never from the client's per-call
context, so `context.Value`s set client-side never arrive. Fixed via `CallToolParams.Meta`
(`internal/core/mcp/meta.go`'s `WithToken`/`TokenFromRequest`), which *is* transmitted with the
call but stays separate from `Arguments` — the only part of a tool call an LLM's function-calling
schema/response ever includes, so a credential set this way is never visible to (or echoable by)
whatever model is planning the call. Every tool handler reads its token via
`mcp.TokenFromRequest(req)`.

**A second, additive `_meta` key carries `Token.Extra`** (`mcp.WithExtra`/`mcp.ExtraFromRequest`,
added alongside Discord — see below): some connections' real usable credential isn't
`AccessToken` at all (Discord's `webhook.incoming` grant has no generally-callable access token,
only the webhook URL stashed in `Extra`). `GetIntegrationToken`'s return type changed from
`(string, error)` to `(Token, error)` to make this possible — safe, since it had exactly one real
caller (`Gateway.ExecuteTool`). `Gateway.ExecuteTool` merges `WithExtra(tok.Extra)` into the same
`Meta` map whenever `Extra` is non-empty; the 5 original tool servers are entirely unaffected,
since most connections have no `Extra` and their `WithToken`/`TokenFromRequest` calls didn't
change. Same serialization-boundary lesson as `_meta` itself applies to `ExtraFromRequest`:
`Extra` survives a real MCP round trip as `map[string]any` (JSON has no way to signal
"this was originally `map[string]string`"), not the `map[string]string` it started as — the
conversion back is explicit, not assumed, and is what `TestWithExtra_RoundTripsThroughRealMCPSession`
actually verifies (round-tripping through a real in-memory session, not just unit-testing the
conversion function in isolation).

**`Gateway.ExecuteTool(ctx, orgID, service, toolName, args)`** (`internal/core/mcp/gateway.go`)
is the one call site that fetches and decrypts an org's token
(`internal/core/integrations.GetIntegrationToken` — the same function workflow 4 built, reused
as-is) and attaches it via `Meta` before dispatching. Not wired to any HTTP route yet — tool
*execution* is workflow 9's job, once `internal/core/graph`'s executor node exists.
`cmd/api/main.go::newMCPRegistry` builds the `Registry` at boot anyway (unused today) so a wiring
bug — a malformed tool schema, a missing registration — fails the process at startup, not
silently the first time something tries to call a tool; confirmed live via the boot log
(`mcp tool registry ready services=8 tools=18`).

**Provider-specific wrinkles worth knowing about before touching a tool server file:**
- **Stripe** (`stripe.go`) authenticates via HTTP Basic (secret key as username, empty password),
  matching `internal/core/integrations/providers/stripe.go`'s existing `ValidateKey`, and posts
  `application/x-www-form-urlencoded` bodies — Stripe's write endpoints don't accept JSON.
  `get_mrr` has no native Stripe endpoint: it paginates active subscriptions and normalizes each
  item's billing interval to a monthly-equivalent amount — documented as an estimate for planning
  purposes, not a billing-grade reconciliation figure, since calendar months aren't a fixed
  length (weeks/days use the standard average-per-month approximation any such estimate makes).
- **Slack** (`slack.go`) hits the same HTTP-200-with-`ok:false` convention workflow 4's
  `providers/slack.go` already documented — handled via a shared `slackEnvelope`/`checkSlackOK`,
  not a bare status-code check, covered by a test proving a 200-with-`ok:false` response becomes
  a real tool error rather than a false success.
- **Notion** (`notion.go`) requires the `Notion-Version` header on every request (no
  content-negotiation fallback — omitting it is itself a request error). `read_page` extracts
  text from a fixed set of block types (paragraph, heading_1/2/3, list items, quote, to_do) via
  one generic `{"rich_text": [...]}` decode rather than a case per type; unrecognized block types
  and nested children are skipped, not errored — this is a text-content reader, not a full Notion
  renderer. `write_page` only supports a `parent_page_id` (not a database parent): a database's
  title-property key can be any name chosen by whoever set up that database, and guessing at it
  would be a real correctness risk, not just a missing feature.
- **GitHub** (`github.go`) sets the documented `application/vnd.github+json` +
  `X-GitHub-Api-Version` headers rather than relying on bare `application/json` happening to keep
  working. `review_pr` fetches PR metadata + per-file diffs for the agent to read — it does not
  auto-post a review back; that would be a separate write tool, not built yet.
- **LinkedIn** (`linkedin.go`) implements only `draft_post` — the original plan's
  `reply_to_mention` is dropped, not deferred: workflow 4 scoped LinkedIn's OAuth to the free
  "Share on LinkedIn" product (`w_member_social`) rather than the paid, partner-gated Marketing
  Developer Platform (see "Third-Party Integrations" above), and reading/replying to mentions
  lives behind that gated platform — there's no real API this tool could call with the scope
  actually granted. "`draft_post`" is a slight misnomer kept from the original plan: LinkedIn's
  API has no draft state reachable with this scope, so the tool composes *and publishes* in one
  call, and its description says so explicitly. `author_urn` is an explicit tool input rather
  than auto-resolved, since `w_member_social` alone doesn't reliably grant the profile-read scope
  a resolve call would need.
- **Discord** (`discord.go`) implements only `send_message` — the `identify` + `webhook.incoming`
  OAuth grant (see "Third-Party Integrations" above) provides exactly one capability, posting
  through a single incoming webhook bound to whichever channel the founder picked during OAuth
  consent. There's no channel-listing or general messaging API reachable from this grant, so
  `list_channels` isn't buildable. The webhook URL, from `Token.Extra` (see the `WithExtra` note
  above), *is* the credential — the request carries no `Authorization` header at all, unlike
  every other tool server here.
- **Google Drive** (`google_drive.go`) implements `list_files`/`read_file`/`create_file` against
  the `drive.file` scope (see "Third-Party Integrations" above): `list_files`/`read_file` only
  ever see files this app itself created or that the founder explicitly opened via a Drive picker
  this product doesn't have — a freshly connected org legitimately sees nothing until
  `create_file` has made something, which is the scope working as intended, not a gap.
  `read_file` only handles plain files via `alt=media`; Google-native Docs/Sheets need the export
  API, not implemented. `create_file` builds a genuine `multipart/related` body by hand (Go's own
  `multipart.Writer.FormDataContentType()` produces `multipart/form-data`, which Google's upload
  API doesn't accept the same way) — covered by a test that actually parses the multipart body
  the tool sends and asserts both parts' content, not just that the call didn't error.
- **Google Calendar** (`google_calendar.go`) implements `list_events`/`create_event` against the
  primary calendar. Real bug caught by a test: RFC3339 timestamps can carry a `+HH:MM` offset,
  and naively concatenating one into a query string lets `+` get silently reinterpreted as a
  space by the server — fixed with `url.QueryEscape`, covered by a test asserting the `+`
  actually survives the round trip to the (fake) server.

**`cmd/seedtools`** enumerates every registered tool via `Registry.ListTools` (new method,
paginates on `NextCursor`), embeds `service`/`name`/`description`/JSON-schema text per tool via
Cohere (`embed-english-v3.0`, `input_type=search_document`, matching the 1024-dim
`founderstack-tools` index created in workflow 1), and upserts into that index's `tools`
namespace via `pinecone-io/go-pinecone`. Run live against real Cohere + Pinecone (not simulated)
during development, twice: 12/12 tools (the Core 5) then re-run after Discord/Drive/Calendar
landed to bring the total to 18/18 — both runs independently re-verified afterward by re-fetching
vectors by ID and confirming real 1024-dim values and intact metadata, not just trusting the
CLI's own success log line.

**Testing**: 36 unit tests (multiple per tool server) connect through a real
`mcp.Client`/`mcp.Server` pair via `mcp.NewInMemoryTransports()` and drive calls through
`session.CallTool` — exercising `mcp.AddTool`'s real schema validation and dispatch, not bare Go
function calls — against fake `httptest` servers standing in for each third-party API, same
"don't depend on a live third-party service to keep the suite green" reasoning as everywhere else
in this codebase. 2 Postgres integration tests (`internal/core/mcp/gateway_integration_test.go`)
cover `Gateway.ExecuteTool` against a fake in-test "echo the token it received" tool server (not
a real Stripe/Slack call) to prove the token fetch/decrypt/delivery plumbing works without
depending on any third-party API accepting a fake token. 2 more (`internal/core/integrations/providers/oauth_flow_test.go`)
cover Discord's webhook-extraction fix specifically.

### Document Upload / RAG (workflow 6) — `internal/core/documents`, `internal/api/documents`

Founder-uploaded documents (legal agreements, financial reports, SOPs) go through
upload → S3 store → text extraction → chunk → Cohere-embed → Pinecone-upsert, the same
Cohere/Pinecone pair workflow 5's `cmd/seedtools` uses for tool descriptions — but a different
model (`embed-multilingual-v3.0`, not `embed-english-v3.0`): tool descriptions are English by
construction, a founder's documents aren't. `internal/core/documents.Processor` (built once in
`cmd/api/main.go::newDocumentsProcessor`) owns the whole pipeline; `internal/api/documents.Handler`
is a thin HTTP layer over it — `Upload`/`Delete`/`Reindex` each kick off `go processor.Process(...)`
/`Purge(...)`/`Reindex(...)` on a detached `context.Background()`, not `c.Request.Context()`,
since the goroutine needs to keep running well past the request that started it.

**Background jobs are plain goroutines + a boot-time recovery sweep, not `riverqueue/river`** —
the same tradeoff workflow 5's design note discusses for tool execution, made explicitly here
too (founder's choice when asked, 2026-08-23): `Store`s the whole pipeline needs — S3
(`aws-sdk-go-v2/service/s3`, LocalStack-compatible via `BaseEndpoint`+`UsePathStyle` when
`AWS_S3_ENDPOINT_URL` is set), text extraction (`ledongthuc/pdf` for PDF; hand-rolled
`archive/zip`+`encoding/xml` for DOCX — no dependency needed, matches this codebase's
dependency-minimalism policy; direct read for TXT/MD), and a hand-written recursive splitter
(`internal/core/documents/splitter.go`, 1024 chars/128 overlap, cascading
`["\n\n", "\n", ". ", " "]` separators, mirroring `RecursiveCharacterTextSplitter`). What a
durable queue buys over this — surviving a process restart mid-job — is covered instead by
`Processor.RecoverStuckJobs` (`internal/core/documents/recover.go`), run once at boot on
`app_system` (cross-tenant scan, same reasoning as the integrations refresh job): any document
still `pending`/`processing`/`deleting` after a 10-minute `stuckThreshold` gets re-dispatched.

**Cohere's real batch cap is 96 texts, not the 100 the original spec sketched** — caught live,
not from documentation: a small test upload (28 chunks, under one batch) passed fine at 100, but
a real 27MB PDF (480+ chunks, multiple full batches) failed on its second batch with Cohere's
actual `"total number of texts must be at most 96 - received 100"`. `embedBatchSize` in
`internal/core/documents/processor.go` is `96`.

**Large documents can outlast Cohere's per-minute rate limit — caught live, fixed by
configuring the SDK's own retrier, not by writing a new one.** The same 27MB PDF that found the
96-cap bug then failed differently once past it: several batches in, a 429 with
`"trial token rate limit exceeded, limit is 100000 tokens per minute"`. cohere-go/v2 already
retries 429/408/5xx internally with jittered exponential backoff and `Retry-After` support
(`internal/retrier.go` in the SDK) — but only 2 attempts by default, nowhere near enough to
survive a per-minute window. The fix is one line in `cmd/api/main.go::newDocumentsProcessor`:
`coherecli.NewClient(..., coreoption.WithMaxAttempts(7))`, giving ~63s of cumulative backoff
(1s/2s/4s/8s/16s/32s between the 7 attempts) — comfortably past one window reset. An
outer hand-rolled retry loop was tried first and reverted: it just duplicated (worse — no
jitter, no `Retry-After` awareness) what the SDK already does, and layering it on top of the
SDK's own retrier made every outer attempt itself retry internally, which is what
`internal/core/documents/deps_test.go`'s `TestCohereEmbedder_*` tests actually pin down against
a fake `httptest` server standing in for Cohere.

**Processor depends on 3 small interfaces, not the concrete S3/Cohere/Pinecone SDK types**
(`internal/core/documents/deps.go`: `BlobStore`, `Embedder`, `VectorIndex`) — the same
interface-segregation reasoning `internal/core/integrations` documents for its
`Provider`/`OAuthProvider`/... split, added specifically so the pipeline could be integration-
tested against fakes instead of needing LocalStack, a real Cohere key, and a real Pinecone index
in CI (none of which `.github/workflows/ci.yml` provisions — only a throwaway Postgres). `*Store`
satisfies `BlobStore` structurally; `Embedder`/`VectorIndex` wrap the Cohere and Pinecone clients
via `NewCohereEmbedder`/`NewPineconeIndex` (the latter needed because `*pinecone.IndexConnection`'s
own `WithNamespace` returns a concrete type, not an interface, so a thin adapter is what makes
per-namespace chaining (`.Namespace(ns).Upsert(...)`) fake-able at all).

**Purge order matters and is deliberate** (`internal/core/documents/purge.go`): Pinecone vectors,
then the S3 object, then the Postgres rows — external deletions before local ones, and only
after both externals actually succeed, so a failure never leaves a `documents` row with nothing
behind it or (worse) orphaned vectors/files nothing points at anymore. Each external delete gets
one retry, not a full backoff loop (`retryOnce`, matches the Python original's actual behavior
per `WORKFLOW_PLAN_GO.md`). `Reindex` (`internal/core/documents/reindex.go`) clears
`document_chunks` and best-effort deletes the old Pinecone vectors (logged via `slog.Warn`, not
fatal to the reindex — the `pinecone_id` scheme is `{doc_id}-{chunk_index}`, so a fresh
`Process` run overwrites every old id as long as the new chunk count isn't smaller) before
re-running `Process` from scratch.

**Verified twice: once by hand against real infra, once as a real automated suite.** The first
pass (2026-08-23) was a live, manual pass against real Postgres + LocalStack S3 + real Cohere +
real Pinecone — full upload → poll → indexed → reindex → delete → purge cycle for a `.txt`, a
real 27MB PDF, and a real LibreOffice-generated `.docx`, confirming via direct queries (not just
trusting a 200) that S3 object count and `document_chunks` row count both hit 0 after delete.
That pass is what found both the 96-batch-cap bug and the rate-limit bug above — but it wasn't
captured as a test, so it caught bugs once and would never catch a regression. The second pass
closed that gap: `internal/core/documents/processor_integration_test.go` (5 tests — `Process`
success and embed-failure, `Purge` full-cleanup and idempotent-no-op, `Reindex`) and
`internal/api/documents/handler_integration_test.go` (one `t.Run`-subtested full HTTP lifecycle
— auth, validation, upload, background-processing, list, reindex, delete+purge, plus 404/400
error paths) both run against real Postgres but fakes for `BlobStore`/`Embedder`/`VectorIndex` —
same "don't depend on a live third-party service to keep the suite green" reasoning as every
other integration test in this codebase, and what makes these runnable in CI at all (see
"Testing Strategy & CI" below for the coverage-gate story this fixed). One real bug the *test*
itself had, worth knowing if you touch these files: an early version of the reindex subtest
polled for `processing_status == "indexed"` without checking whether that was the *new* run or
a stale reading of the *previous* run's terminal state — it could pass while the actual reindex
was still in flight (or had already failed) in the background. Fixed by comparing `indexed_at`
before/after (`waitForFreshIndexed`), not just the status string. Unit tests
(`internal/core/documents/splitter_test.go`, `extract_test.go`, `deps_test.go`) cover the pure
chunking/extraction logic and the Cohere retry configuration without any live dependency, same
split as every other package in this codebase.

### Agent Configuration (workflow 7) — `internal/api/agents`

Pure CRUD over the `agents` table — name, system prompt, model, and a `policy_scope` JSONB
column (`{max_tool_calls, max_cost_per_run_usd, allowed_tools}`). **Nothing here calls an LLM or
executes a tool** — that's workflow 9's job (`internal/core/graph`, not built yet); this package
only manages the row that later constrains what workflow 9's executor node will be allowed to
do. There's no Python original to mirror here (`app/models/ai.py` defines the `Agent`
SQLAlchemy model, structurally identical to the Go migration's `agents` table, but no
`app/api/v1/endpoints/agents.py` was ever built) — the wire contract below was designed fresh
against `WORKFLOW_PLAN_GO.md`'s spec, same as workflows 5 and 6 were.

**Duplicate-name rejection is a real partial-unique-index, not just an app-level check** —
migration `000006_agents_unique_org_name_active.up.sql` adds a unique index on `(org_id, name)
WHERE is_active = true`, the same gap-fix pattern `api_key_registry` (000004) and
`mcp_connections` (000005) already established. Partial, not a plain `UNIQUE` constraint,
specifically so a deactivated agent's name doesn't block a brand-new agent from reusing it.
`POST` hits it via `INSERT ... ON CONFLICT (org_id, name) WHERE is_active = true DO NOTHING` (0
rows back means `pgx.ErrNoRows` on the `:one` scan → `400 DUPLICATE_AGENT_NAME`); a `PATCH`
rename can't use `ON CONFLICT` (that's insert-only SQL), so a colliding rename just raises a real
Postgres `23505`, caught via `errors.As(err, &pgconn.PgError{})` and translated the same way.

**`allowed_mcp_servers` is derived, never accepted from the client** — computed server-side from
the unique `service` prefixes in `policy_scope.allowed_tools` (`serversFromTools`), so the two
JSONB columns can't drift out of sync the way they would if the frontend had to keep both
consistent by hand.

**`GET /api/v1/agents/tools`, not in the original plan text** — the plan sketched the allowed-
tools multi-select as populated "from `GET /api/v1/integrations` connected tools," but that
endpoint only reports connection *status*, not individual tool names; the real catalog (service,
tool name, description) only exists in workflow 5's `coremcp.Registry`, which had no HTTP
endpoint before this. `Handler.ListAvailableTools` cross-references the org's connected services
(`mcp_connections`, `is_active = true`) against `Registry.ListTools()` and returns the
intersection — read-only introspection, not execution, so it doesn't pull workflow 9's scope
forward. Offering a tool from an unconnected service would just be a dead-end choice nothing
could ever call.

**`GetAgent` intentionally does not filter by `is_active`** — a soft-deleted agent's full config
stays fetchable by id (`DELETE` only sets `is_active = false`; the row is never removed, "without
losing run history" per the acceptance criteria). Only `ListAgents` and the plan-limit's
`CountActiveAgents` exclude inactive agents — a future run-history view (workflow 9/11) needs
`GetAgent` to still resolve for a run whose agent has since been deleted.

**Plan limit reads `organizations.max_agents` (already in the schema, default 3), not a
hardcoded tier table** — workflow 15 (billing) isn't built, so every org sits at the column's
default today; `POST` compares `CountActiveAgents` against it before inserting, accepting the
same small TOCTOU race this codebase tolerates elsewhere at this scale (no billing-grade
guarantee needed yet).

**Verified twice**: a full manual `curl` pass against real Postgres (every validation rejection,
the plan-limit boundary at exactly 3, rename-collision, name-reuse-after-delete), then
`internal/api/agents/handler_integration_test.go` (22 subtests covering the same scenarios) run
against real Postgres with a **fake 2-tool MCP registry** — 2 tiny in-memory `gomcp.Server`s
(`stripe.get_mrr`, `slack.send_message`), same "don't depend on a live third-party service"
reasoning as `internal/core/mcp/gateway_integration_test.go`'s `echoTokenServer`. `handler_test.go`
unit-tests `slugify`/`serversFromTools` in isolation. `internal/api/agents` lands at ~75%
coverage.

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

**Real bug, found and fixed 2026-08-23: the coverage step only ever passed `SYSTEM_DATABASE_URL`
to `make coverage`, never `APP_DATABASE_URL`.** Every integration test needing the RLS-scoped
`app_user` pool (`TEST_APP_DATABASE_URL` — settings, integrations, tenant, mcp, llm, and now
documents) opens with `if dsn == "" { t.Skip(...) }`, so this didn't fail loudly, it just quietly
skipped a large fraction of the suite. Locally this was invisible: the `Makefile`'s
`APP_DATABASE_URL ?= $(shell grep ... .env)` fallback means a developer's own `.env` (present on
every dev machine, never in the repo) silently filled the gap, so `make coverage` run by hand
always looked fine. CI has no `.env` — so the exact same command that passes locally dropped
real coverage from ~64% to as low as ~44% there, with `internal/api/settings` and
`internal/api/integrations` specifically showing `0.0%`. Fixed by adding
`APP_DATABASE_URL="postgresql://app_user:app_password@localhost:5432/founderstack?sslmode=disable"`
alongside the existing `SYSTEM_DATABASE_URL` line in the "Test with coverage" step — matching
`app_user`'s password from `internal/db/migrations/000002_enable_rls.up.sql`, same as the existing
`app_system` line already did for `000003`. The general lesson: any Makefile default that falls
back to reading a gitignored, dev-only `.env` file is invisible in CI by construction — a CI
step invoking that target must pass every variable the target needs explicitly, not just the
ones that happened to matter for whatever was being tested when the step was first written.

**Coverage gate** (`make coverage`, `COVERAGE_THRESHOLD` in the `Makefile`, currently `60`):
runs the full tagged suite with `-coverprofile`, strips `internal/db/dbgen` (sqlc-generated —
testing generated code directly isn't meaningful; it's already exercised indirectly through
the handler integration tests that call it) out of the profile, and fails if the remaining
total statement coverage drops below the threshold. The threshold is kept a few points below
the actual total (raised from 55 to 60 after workflow 4 pushed real coverage to ~64.5%, following
the same "a few points below actual, not 100% or an arbitrary round number" policy the gate
started with at ~59.6%) — low enough that small legitimate additions in still-thin areas
(`cmd/api`'s `run()`/`newRouter()` wiring, `internal/api/v1/health.go`, both accepted, documented
gaps rather than hidden ones) don't fail CI on their own, high enough that a real regression
(e.g. deleting the webhook tests, or workflow 4's provider request-shape tests) still trips it
immediately.

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

| Prefix | File | Auth | Purpose |
|--------|------|------|---------|
| `/api/v1/health` | `internal/api/v1/health.go` | none | DB + Redis + Pinecone liveness |
| `/api/v1/auth/dev-token` | `internal/api/identity/devtoken.go` | none (self-issues) | local test token minting (workflow 3) |
| `/api/v1/settings/api-key*` | `internal/api/settings/apikey.go` | `middleware.RequireAuth` | BYOK key CRUD across 5 LLM providers, plus `.../api-key/providers` (catalog+status merge) (workflow 3) |
| `/api/webhooks/clerk` | `internal/api/webhooks/clerk.go` | Svix signature, not `RequireAuth` | Clerk org/user sync (workflow 2) |
| `/api/v1/integrations`, `/api/v1/integrations/{service}/connect` (all auth types), `.../status`, `DELETE .../{service}` | `internal/api/integrations/handler.go` | `middleware.RequireAuth` | Connect/manage third-party integrations (workflow 4) |
| `/api/v1/integrations/{service}/callback` | `internal/api/integrations/handler.go` | none — org/service recovered from `state`, not a JWT | OAuth provider redirect target (workflow 4) |
| `/api/v1/documents/upload`, `GET /documents`, `GET /documents/{id}`, `DELETE /documents/{id}`, `POST /documents/{id}/reindex` | `internal/api/documents/handler.go` | `middleware.RequireAuth` | Upload/list/reindex/delete founder documents for RAG (workflow 6) |
| `GET/POST /api/v1/agents`, `GET /agents/tools`, `GET/PATCH/DELETE /agents/{id}` | `internal/api/agents/handler.go` | `middleware.RequireAuth` | Agent configuration CRUD — no execution (workflow 7) |

(Everything else in `WORKFLOW_PLAN_GO.md` — workflows, runs — is unbuilt. Add rows here as
routers land.)

### Dependency policy

Go dependencies are added when code actually imports them, not pre-installed speculatively
against the full `WORKFLOW_PLAN_GO.md` dependency table — `go mod tidy` strips unused
`require`s anyway, and an unused import is dead weight (and an unreviewed supply-chain
surface) until something calls it. `clerk-sdk-go/v2`, `golang-jwt/jwt/v5`, and
`anthropic-sdk-go` were all added in workflow 3, exactly when each was first actually used
(real JWT verification, dev tokens, and BYOK key validation, respectively) — see
"Authentication" and "BYOK API Keys" above. `golang.org/x/oauth2` (plus `x/oauth2/google`) was
added in workflow 4 for the 6 standard-OAuth2 providers; **Slack was deliberately implemented
without it** (`providers/slack.go`, plain `net/http`) since Slack's own response shape — HTTP
200 even on a failed token exchange — doesn't fit `x/oauth2`'s assumptions, so pulling in the
dependency there would have bought nothing. Stripe's `ValidateKey` is one REST call, not the
`stripe-go` SDK the original plan sketched — same "don't add a dependency a single GET doesn't
justify" reasoning; this note said "revisit if workflow 5's Stripe MCP tools end up needing more
than that" — they did (4 endpoints: list/create/refund/MRR, not just a balance check), and
`stripe-go` still wasn't added. Plain `net/http` scaled fine to 4 endpoints; revisit again only
if a future Stripe tool needs something REST calls make genuinely awkward (webhook signature
verification, idempotency-key retry logic), not just "more endpoints."
`modelcontextprotocol/go-sdk` and `cohere-ai/cohere-go/v2` were added in workflow 5, exactly when
first used (the MCP tool servers/gateway, and `cmd/seedtools`'s embeddings, respectively) —
`pinecone-io/go-pinecone` was already present (health check's `ListIndexes`), workflow 5 is just
its second real caller (`UpsertVectors`). `aws-sdk-go-v2/{config,credentials,service/s3}` and
`ledongthuc/pdf` were added in workflow 6, exactly when first used (S3 storage and PDF text
extraction, respectively) — DOCX extraction deliberately got no new dependency (hand-rolled
`archive/zip`+`encoding/xml` in `internal/core/documents/extract.go` instead), same reasoning as
Slack's hand-rolled OAuth and Stripe's plain `net/http`. `sentry-go` and `otel` are still planned
but not yet in `go.mod` — add each when its workflow lands.

## Environment Variables

See `.env.example` for the full list with inline notes on Go-specific deviations from the
Python `.env.example` (DSN scheme, `sslmode=disable`, AES-256-GCM key note). Required (Load
fails listing all that are missing, not just the first): `DATABASE_URL`, `APP_DATABASE_URL`,
`SYSTEM_DATABASE_URL`, `CLERK_SECRET_KEY`, `CLERK_PUBLISHABLE_KEY`, `CLERK_WEBHOOK_SECRET`,
`LOCALSTACK_AUTH_TOKEN`, `PINECONE_API_KEY`, `ENCRYPTION_KEY`, `OAUTH_STATE_SECRET`. The three
`*_DATABASE_URL` vars connect as three different Postgres roles for three different trust
boundaries — see "Row-Level Security" above before adding a fourth use case to any of them.

The per-provider OAuth client ID/secret pairs (`SLACK_CLIENT_ID`, `DISCORD_CLIENT_ID`, ...) are
deliberately **not** in that required list, unlike `OAUTH_STATE_SECRET` itself: the app boots
and serves all of workflow 4's routes fine with every one of them blank — a request to `POST
/api/v1/integrations/{service}/connect` for an unconfigured provider just fails at the real
OAuth server (wrong/empty `client_id`), not at boot. Requiring them all up front would force a
solo founder to register every provider's app before the server even starts, for integrations
they may not use yet.

`DEV_TOKEN_SECRET` is deliberately **not** in that required list — it's local-testing-only
(see "Authentication" above) and should stay unset everywhere real, including production.

## Adding a New Feature

1. **New table / schema change**: `make migrate-create NAME=descriptive_name`, write the
   `.up.sql`/`.down.sql` pair by hand (no autogenerate — see "No ORM" above), `make migrate-up`
   against local Postgres to verify it applies cleanly.
2. **New query**: add it to a `.sql` file under `internal/db/queries/` (new file per feature
   area, matching existing ones like `clerk_sync.sql`), `make sqlc-generate`, use the
   generated `dbgen.Queries` method — see "No ORM" above.
3. **New endpoint**: add a handler package under `internal/api/`, register it on the
   appropriate route group in `cmd/api/main.go::newRouter`. Use `response.OK`/`response.Fail`
   for every response — see "Response Envelope" above. If it needs an authenticated caller,
   put `middleware.RequireAuth(systemPool, cfg)` on its route group (see "Authentication"
   above) and read the caller via `authctx.FromContext(c)`, then run its actual DB work
   through `tenant.WithTx(ctx, appPool, user.OrgID, ...)` — never a bare query against
   `app_user`. `app_system` is only for genuine cross-tenant system contexts (webhooks,
   schedulers), not a shortcut around auth.
4. **New external dependency**: `go get` it when you write the first line of code that imports
   it, not before (see "Dependency policy" above).
5. **New secret/config value**: add a field to `internal/config/config.go`'s `Config` struct
   (use `secret.Value` for anything sensitive), add it to the `defaults` map in `Load()`, add
   it to `requiredFields` if the app can't run without it, document it in `.env.example`.
