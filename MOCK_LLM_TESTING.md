# Manual verification guide: Workflow 9 harness via Mock LLM Mode

This is the step-by-step guide for exercising the whole agent execution engine
(`internal/core/graph`) against a real, running server and real Postgres —
with **zero real LLM provider calls** — using `MOCK_LLM_MODE`. Every step below
was actually run against a local dev server while writing this guide (not just
written and assumed correct); three real bugs/gaps were found and fixed in the
process — see "Bugs found while writing this guide" at the bottom.

## Why this exists

You don't have a real Anthropic/OpenAI/Gemini/Qwen/DeepSeek key yet. The full
Workflow 9 test suite already proves the harness correct
(`go test -tags=integration ./internal/core/graph/...`), but that's Go test
output, not something you can click through. `MOCK_LLM_MODE` lets you drive
the exact same guardrails through the real HTTP API, the real SSE stream, and
real Postgres rows — so you can watch a run happen, not just trust a green
checkmark.

**How it works**: when `MOCK_LLM_MODE=true`, `cmd/api/main.go` swaps the
Launcher's real BYOK `ChatClientResolver` for `llm.MockScenarioResolver`
(`internal/core/llm/mockscenarios.go`). Every agent's plain `model` field
becomes a scenario selector — set an agent's model to `mock:tool-call`, for
example, and every run of that agent gets a scripted, deterministic model
response sequence instead of a real API call. **This is dev-only and asserted
off in production** (`cmd/api/main.go` refuses to boot with
`MOCK_LLM_MODE=true` and `APP_ENV=production`) — see `Config.MockLLMMode`'s
doc comment.

**Important — this affects the whole running process, not just test orgs.**
While `MOCK_LLM_MODE=true`, *every* org's workflow runs get scripted mock
responses, including a real dev org you might also be using for workflows
1–8. Don't leave it on if you're about to demo or test anything that expects
a real model. Turn it off by restarting the server without the env var (or
with it set to `false`).

## One-time setup: a disposable test org

Per this project's own testing convention, never reuse your real dev org for
throwaway test data — use a disposable one and clean it up when done.

### 1. Start the server in mock mode

```bash
cd founderstack-api-go
make build
MOCK_LLM_MODE=true ./bin/api
```

Confirm the boot log shows:
```
WARN MOCK_LLM_MODE enabled — every workflow run will use a scripted MockChatClient, no real LLM provider will be called
```

### 2. Create a disposable org + user (direct SQL, `app_system` role)

```bash
export PGPASSWORD=app_system_password
psql -h localhost -p 5440 -U app_system -d founderstack <<'SQL'
INSERT INTO organizations (clerk_org_id, name, slug, llm_provider)
VALUES ('org_mocktest_demo', 'Mock Test Org', 'mock-test-demo', 'anthropic')
RETURNING id;
SQL
```
Save the returned `id` as `$ORG_ID` for everything below.

```bash
psql -h localhost -p 5440 -U app_system -d founderstack -c \
  "INSERT INTO users (org_id, clerk_user_id, email, role) VALUES ('$ORG_ID', 'user_mocktest_demo', 'mocktest@example.com', 'owner') RETURNING id;"
```

**Raise the agent plan limit** — `organizations.max_agents` defaults to 3, and
this guide has you create one agent per scenario (8+). This is a real,
existing guardrail (workflow 7's plan-limit check) doing exactly its job —
just not what you want for a test org:
```bash
psql -h localhost -p 5440 -U app_system -d founderstack -c \
  "UPDATE organizations SET max_agents = 20 WHERE id = '$ORG_ID';"
```

### 3. Mint a dev auth token

Requires `DEV_TOKEN_SECRET` set in `.env` (dev-only; never set in production).
```bash
TOKEN=$(curl -s -X POST localhost:8000/api/v1/auth/dev-token \
  -H "Content-Type: application/json" \
  -d '{"clerk_user_id":"user_mocktest_demo"}' \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['data']['token'])")
```
Every `curl` command below assumes `$TOKEN` is set this way in your shell.

### 4. Submit a mock BYOK key (via the real API, no raw SQL needed)

`internal/core/llm`'s existing `API_KEY_MOCK_PREFIX` short-circuit
(`mock-test-key-` by default) already lets BYOK validation succeed with zero
network calls — reuse it here so `Launcher.Preflight`'s key-presence check
passes:
```bash
curl -s -X POST localhost:8000/api/v1/settings/api-key \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"provider":"anthropic","api_key":"mock-test-key-abc123"}'
```
The key's actual contents are never used in mock mode (the resolver never
reaches BYOK decryption at all) — any `mock-test-key-...` string works.

You're now ready to create agents and run scenarios.

## Scenario catalog

Every scenario is a fixed value for an agent's `model` field. Create one
agent + one manual-trigger workflow per scenario you want to test — the
pattern is always:

```bash
AGENT_ID=$(curl -s -X POST localhost:8000/api/v1/agents \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{
    "name": "<scenario name>",
    "system_prompt": "You are a test agent used only for exercising a specific workflow 9 guardrail scenario.",
    "model": "<mock:scenario-name>",
    "policy_scope": {"allowed_tools": ["<tool>"]}
  }' | python3 -c "import sys,json;print(json.load(sys.stdin)['data']['id'])")

WF_ID=$(curl -s -X POST localhost:8000/api/v1/workflows \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d "{\"agent_id\":\"$AGENT_ID\",\"name\":\"<scenario> workflow\",\"trigger_type\":\"manual\",\"task_input_template\":\"<any text>\"}" \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['data']['id'])")

RUN_ID=$(curl -s -X POST localhost:8000/api/v1/workflows/$WF_ID/run \
  -H "Authorization: Bearer $TOKEN" \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['data']['run_id'])")
```

Then inspect with `GET /runs/{id}` and/or watch it live (see "Watching the
SSE stream live" below).

### `mock:happy` — node sequencing, no tools

**Proves**: planner→executor→validator→reporter sequencing, the `complete`
SSE event's shape.
```
policy_scope: {"allowed_tools": []}
```
**Expected**: `status: "completed"`, `output: "Task complete — no tools were
needed for this run."`, `tool_call_count: 0`.

### `mock:tool-call` — a real tool call, checkpointing, audit trail

**Proves**: `tool_call`/`tool_result` SSE events, per-tool-call checkpointing,
`cost_ledger`/`audit_logs` writes, the terminal-tool-error-fed-back-to-model
path (since your test org has no real Notion connection, the call will
legitimately fail with a token-fetch error — the model still sees it and
finishes normally).
```
policy_scope: {"allowed_tools": ["notion.read_page"]}
```
**Expected**: `status: "completed"`, `tool_call_count: 1`.

Verify the audit trail:
```bash
psql -h localhost -p 5440 -U app_system -d founderstack -c \
  "SELECT cost_type, provider, model FROM cost_ledger WHERE run_id = '$RUN_ID';"
# -> 2 llm_inference rows + 1 tool_call row

psql -h localhost -p 5440 -U app_system -d founderstack -c \
  "SELECT action, resource_type, status FROM audit_logs WHERE metadata_info->>'run_id' = '$RUN_ID';"
# -> 1 row, action='tool.executed', status='error' (no real Notion connected) or 'success' (if you connect one — see "Testing a real tool execution" below)
```

### `mock:approval` — the full suspend → approve/reject → resume cycle

**Proves**: destructive/financial tools always suspend regardless of amount,
`approval_required` SSE event, `workflow_runs.status = 'awaiting_approval'`,
`checkpoint_state.pending_tool_calls`, and — via the dev-only resume route —
the actual resume mechanism itself (workflow 10's real approve/reject
endpoints don't exist yet; this is the closest manual equivalent).
```
policy_scope: {"allowed_tools": ["stripe.refund_payment"]}
```
Trigger the run, then immediately check status:
```bash
sleep 1.5
curl -s localhost:8000/api/v1/runs/$RUN_ID -H "Authorization: Bearer $TOKEN"
# -> status: "awaiting_approval", tool_call_count: 0
```
Inspect the suspended checkpoint:
```bash
psql -h localhost -p 5440 -U app_system -d founderstack -c \
  "SELECT current_node, status, checkpoint_state->'pending_tool_calls' AS pending FROM workflow_runs WHERE id = '$RUN_ID';"
# -> current_node: approval_gate, pending: [{"Name": "stripe.refund_payment", ...}]
```
**Approve it** (dev-only stand-in for workflow 10's `POST
/approvals/{id}/approve`):
```bash
curl -s -X POST localhost:8000/api/v1/runs/$RUN_ID/dev-resume \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"approved": true, "reason": "manual verification"}'
sleep 2
curl -s localhost:8000/api/v1/runs/$RUN_ID -H "Authorization: Bearer $TOKEN"
# -> status: "completed", output: "Refund processed. Task complete.", tool_call_count: 1
```
**Or reject it** (run a fresh instance of this scenario for this path):
```bash
curl -s -X POST localhost:8000/api/v1/runs/$RUN_ID/dev-resume \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"approved": false, "reason": "test rejection"}'
# -> status: "completed", output: "Run stopped: the pending action(s) were not approved. Reason: test rejection", tool_call_count: 0
```
(Rejection still ends the *run* in `completed` — a rejected action is a
normal, successful outcome of the harness doing its job, not a failure.)

### `mock:policy-violation` — executor-level allowed_tools enforcement

**Proves**: `agents.policy_scope.allowed_tools` is enforced independently of
what the model requests — planner/model intent is never trusted as a
security boundary.
```
policy_scope: {"allowed_tools": ["notion.read_page"]}   # deliberately NOT write_page
```
The scenario requests `notion.write_page`, which isn't in the allow-list.
**Expected**: `status: "failed"`, `tool_call_count: 0` (never dispatched).

### `mock:stuck-loop` — no-progress detector

**Proves**: the harness aborts if the model requests the exact same tool call
twice in a row, rather than burning the whole `max_tool_calls` budget on a
death spiral.
```
policy_scope: {"allowed_tools": ["notion.read_page"]}
```
**Expected**: `status: "failed"`, `tool_call_count: 1` (the first call
executed; the second, identical request is what triggers the abort, before
it runs).

### `mock:tool-call-cap` — `max_tool_calls` enforcement

**Proves**: `policy_scope.max_tool_calls` actually stops a run.
```
policy_scope: {"allowed_tools": ["notion.read_page"], "max_tool_calls": 2}
```
The scenario requests 5 different calls in a row (different args each time,
so the stuck-loop detector never fires first).
**Expected**: `status: "failed"`, `tool_call_count: 2` (aborts right at the
cap, not before, not after).

### `mock:cost-cap` — `max_cost_per_run_usd` enforcement

**Proves**: `policy_scope.max_cost_per_run_usd` actually stops a run, using
`llm.EstimateCostUSD`'s rough interim per-token estimate (real billing-grade
pricing is workflow 11's job — see `internal/core/llm/pricing.go`'s doc
comment).
```
policy_scope: {"allowed_tools": ["notion.read_page"], "max_cost_per_run_usd": 0.05}
```
Same 5-distinct-calls shape as `mock:tool-call-cap`, but each turn reports
5000 input / 2000 output tokens (≈$0.011/turn at the fallback rate).
**Expected**: `status: "failed"`, `tool_call_count: 5`, `cost_so_far_usd`
just over `0.05` (confirmed live: `0.055`) — CheckCaps aborts right after the
call that pushes cumulative cost over the cap, same "checked after every
tool call" semantics as `max_tool_calls`.

### `mock:cancel` — mid-run cancellation

**Proves**: `POST /runs/{id}/cancel` actually stops an in-flight run and
lands it on `status = 'cancelled'` (not `'failed'` — see "Bugs found while
writing this guide" below for a real bug this exact test caught).
```
policy_scope: {"allowed_tools": ["notion.read_page"]}
```
This scenario sleeps 5 seconds before each mock response, on purpose — giving
you time to cancel it:
```bash
sleep 1
curl -s -X POST localhost:8000/api/v1/runs/$RUN_ID/cancel -H "Authorization: Bearer $TOKEN"
sleep 1
curl -s localhost:8000/api/v1/runs/$RUN_ID -H "Authorization: Bearer $TOKEN"
# -> status: "cancelled"
```

## Watching the SSE stream live

Every scenario except `mock:cancel` uses an 800ms delay per mock "model call"
(see `defaultMockDelay` in `mockscenarios.go`) specifically so a manually
attached SSE stream has time to catch real events — a plain, undelayed mock
response returns in under a millisecond, faster than a human can open a
second terminal. **Attach the stream as close to the run-trigger call as
possible** — if you wait even a second or two after the run has already
finished, there's nothing left to deliver (events aren't buffered/replayed
for a late subscriber):

```bash
RUN_ID=$(curl -s -X POST localhost:8000/api/v1/workflows/$WF_ID/run \
  -H "Authorization: Bearer $TOKEN" \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['data']['run_id'])")
curl -s -N localhost:8000/api/v1/runs/$RUN_ID/stream -H "Authorization: Bearer $TOKEN"
```

Expect a sequence like:
```
event: node_start   data: {...,"data":{"node":"planner","agent_name":"..."}}
event: node_end     data: {...,"data":{"node":"planner","agent_name":"..."}}
event: node_start   data: {...,"data":{"node":"executor","agent_name":"..."}}
event: tool_call     data: {...,"data":{"tool":"notion.read_page","args":{...}}}
event: tool_result   data: {...,"data":{"tool":"notion.read_page","is_error":true}}
event: node_end     data: {...,"data":{"node":"executor","agent_name":"..."}}
event: node_start   data: {...,"data":{"node":"validator", ...}}
...
event: complete      data: {...,"data":{"output":"...","token_usage":{...},"cost_so_far_usd":0}}
```

## Testing a real tool execution (optional, needs a real integration)

Every scenario above works with **zero** connected third-party integrations —
the tool call is genuinely attempted, just fails with a token-fetch error
(itself a real, useful thing to verify: the terminal-error-fed-back-to-model
path). If you want to see a tool call *succeed*, connect a real integration
for your test org first (Notion is the easiest — free, no financial risk) via
the normal `/integrations/notion/connect` OAuth flow, then re-run
`mock:tool-call` against a real Notion page id.

## Preflight scenarios (no mock scenario needed — pure config)

### No BYOK key configured
```bash
curl -s -X DELETE "localhost:8000/api/v1/settings/api-key?provider=anthropic" -H "Authorization: Bearer $TOKEN"
curl -s -X POST localhost:8000/api/v1/workflows/$WF_ID/run -H "Authorization: Bearer $TOKEN"
# -> 400, code NO_BYOK_KEY
```
(Re-submit the mock key afterward to keep testing other scenarios.)

### Org kill switch (`agents_paused`)
```bash
psql -h localhost -p 5440 -U app_system -d founderstack -c \
  "UPDATE organizations SET agents_paused = true WHERE id = '$ORG_ID';"
curl -s -X POST localhost:8000/api/v1/workflows/$WF_ID/run -H "Authorization: Bearer $TOKEN"
# -> 400, code AGENTS_PAUSED
psql -h localhost -p 5440 -U app_system -d founderstack -c \
  "UPDATE organizations SET agents_paused = false WHERE id = '$ORG_ID';"
```

## Response-time acceptance criteria

Both of `WORKFLOW_PLAN_GO.md`'s soft-latency criteria are easy to eyeball
against mock mode too:
```bash
time curl -s -X POST localhost:8000/api/v1/workflows/$WF_ID/run -H "Authorization: Bearer $TOKEN"
# should be well under 500ms (was ~27ms in local testing)
```

## Known, deliberate gaps this guide does NOT cover

- **The prompt-injection validator warning** (a tool result containing text
  like "ignore previous instructions") isn't reachable through this guide
  without a real connected integration whose actual data contains a trigger
  phrase — proven instead by the automated test
  `TestBuildNodes_ValidatorFlagsSuspiciousToolResult` in
  `internal/core/graph/nodes_integration_test.go`.
- **Rate limiting** (30 calls/min per org+service) and **retry-on-5xx/429**
  are also better proven by the automated suite
  (`internal/core/mcp/ratelimit_integration_test.go`,
  `internal/core/mcp/servers/http_helper_test.go`) than by hand — both need
  either a real flaky/rate-limited third party or a fake one, which the Go
  tests already set up.

## Cleanup

```bash
psql -h localhost -p 5440 -U app_system -d founderstack <<SQL
DELETE FROM cost_ledger WHERE org_id = '$ORG_ID';
DELETE FROM workflow_runs WHERE org_id = '$ORG_ID';
DELETE FROM workflows WHERE org_id = '$ORG_ID';
DELETE FROM agents WHERE org_id = '$ORG_ID';
DELETE FROM api_key_registry WHERE org_id = '$ORG_ID';
DELETE FROM users WHERE org_id = '$ORG_ID';
DELETE FROM organizations WHERE id = '$ORG_ID';
SQL
```
(`audit_logs` cascades with the organization automatically — confirmed
during this guide's own testing, no separate delete needed.)

Restart the server without `MOCK_LLM_MODE=true` (or with it explicitly
`false`) before doing anything with your real dev org again.

## Bugs found while writing this guide

All three were caught by actually running the scenario end to end against a
real server, not by reading the code — worth remembering as evidence for why
this guide exists at all, not just a checklist.

1. **`mock:approval`'s naive canned-response design broke across the
   suspend/resume boundary.** `Engine.Resume` triggers a brand-new
   `ChatClientResolver` call — a fresh `MockChatClient` with its
   canned-response index reset to 0 — unlike a real provider client, where
   "what comes next" is driven by the actual conversation history, not by
   how many times the client object itself has been constructed. The first
   version of this scenario aborted with a false "stuck loop detected" the
   instant a human approved, because the freshly-resolved mock's first
   response was once again the exact same `stripe.refund_payment` request
   the stuck-loop detector had just recorded as the run's last batch. Fixed
   by making `mock:approval` conversation-aware instead
   (`approvalScenarioClient` in `mockscenarios.go`): it decides its response
   by checking whether a `stripe.refund_payment` tool result is already
   present in the conversation history, not by a sequential counter.
2. **A real, pre-existing production bug**, unrelated to mock mode itself:
   `Launcher.Launch`'s error-handling goroutine called
   `markRunFailedNoCheckpoint` (which unconditionally writes
   `status = 'failed'`) whenever `Engine.Run` returned *any* error —
   including a correctly cancelled run, whose own checkpoint had *already*
   written `status = 'cancelled'` moments earlier. The unconditional
   overwrite silently clobbered it back to `'failed'` every single time,
   which the existing automated test suite never caught because its
   cancellation test (`TestEngine_CancelStopsAnInFlightRun`) exercises
   `Engine.Cancel` directly, bypassing `Launcher.Launch` entirely. Found by
   running `mock:cancel` against the real `POST /workflows/{id}/run` →
   `POST /runs/{id}/cancel` path and seeing `status: "failed"` where
   `"cancelled"` was expected. Fixed in `launch.go`: the fallback now checks
   the run's actual current status first, and only overwrites when it's
   still `'pending'` (i.e. dependency resolution genuinely failed before
   `Engine.Run` ever started) — the same fix was applied to the new
   `Launcher.Resume`'s equivalent fallback (checks for `'awaiting_approval'`
   instead of `'pending'`).
3. **`policy_scope.max_cost_per_run_usd` could never actually trip.**
   `RunState.CostSoFarUSD` was never incremented anywhere in
   `internal/core/graph` — every mock scenario's canned `ChatResponse` also
   defaulted `Usage` to the zero value, so even after the fix below, cost
   wouldn't have moved without also giving the scenarios real token counts.
   Two-part fix: `internal/core/llm/pricing.go`'s `EstimateCostUSD` (a
   rough, clearly-labeled-as-an-estimate per-token price table — real
   billing-grade pricing stays workflow 11's job) is now called from
   `accumulateUsage` (adds to `RunState.CostSoFarUSD`) and from
   `writeCostLedgerLLMCall` (the persisted `cost_ledger.estimated_cost_usd`,
   previously hardcoded `0`); every scenario response in
   `mockscenarios.go` now reports a representative `Usage` value (500
   input / 150 output tokens per turn, or 5000/2000 for the new
   `mock:cost-cap` scenario specifically). Verified live: a `$0.05` cap
   against `mock:cost-cap`'s 5 calls aborted the run at `cost_so_far_usd:
   0.055`, `tool_call_count: 5`, `status: "failed"` — exactly the "checked
   after every tool call" semantics `max_tool_calls` already had.
