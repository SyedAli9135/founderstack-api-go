-- internal/core/graph's checkpoint read/write — the engine.Run/Resume/checkpoint
-- calls added workflow9_engine_guardrails.up.sql for. Always run through
-- tenant.WithTx (a fresh transaction per call, org-scoped) — never held open across
-- a node's tool-call loop or an approval-gate pause. See WORKFLOW_PLAN_GO.md's
-- Workflow 9 harness planning notes for the full reasoning.

-- name: UpdateRunCheckpoint :exec
UPDATE workflow_runs
SET checkpoint_state = $3,
    current_node     = $4,
    cost_so_far_usd  = $5,
    tool_call_count  = $6,
    status           = $7
WHERE org_id = $1 AND id = $2;

-- name: GetRunCheckpoint :one
SELECT checkpoint_state, current_node, cost_so_far_usd, tool_call_count, status
FROM workflow_runs
WHERE org_id = $1 AND id = $2;

-- graph.Launcher's run lifecycle (POST /workflows/{id}/run's async goroutine
-- — see internal/core/graph/launch.go) and the read-only HTTP endpoints
-- (GET /runs, GET /runs/{id}).

-- name: GetOrgRunSettings :one
-- Preflight (kill switch) + provider resolution for Launcher.Launch.
SELECT llm_provider, agents_paused FROM organizations WHERE id = $1;

-- name: MarkRunStarted :exec
UPDATE workflow_runs SET status = 'running', started_at = now() WHERE org_id = $1 AND id = $2;

-- name: MarkRunFailedPreflight :exec
-- Used only when dependency resolution itself fails before Engine.Run
-- ever starts (a bad policy_scope, a registry error, ...) — there's no
-- checkpoint yet for a normal Engine-driven "failed" transition to have
-- written, so this writes the terminal status directly.
UPDATE workflow_runs SET status = 'failed', completed_at = now() WHERE org_id = $1 AND id = $2;

-- name: GetRunStatus :one
-- Read back the definitive status Engine's own checkpoint() already
-- wrote (completed/failed/cancelled/awaiting_approval) — Launcher can't
-- infer this from Engine.Run's returned error alone, since a suspended
-- run also returns a nil error.
SELECT status FROM workflow_runs WHERE org_id = $1 AND id = $2;

-- name: FinalizeRun :exec
-- Fills in the completion-summary fields checkpoint() itself doesn't own
-- (status stays checkpoint()'s alone) — only called once a run reaches a
-- genuinely terminal status, never for awaiting_approval.
UPDATE workflow_runs
SET output = $3, input_tokens = $4, output_tokens = $5, cached_tokens = $6,
    completed_at = now(),
    duration_ms = (EXTRACT(EPOCH FROM (now() - COALESCE(started_at, created_at))) * 1000)::int
WHERE org_id = $1 AND id = $2;

-- name: GetRunDetail :one
SELECT id, workflow_id, status, triggered_by, output, input_tokens, output_tokens,
       cached_tokens, cost_so_far_usd, tool_call_count, started_at, completed_at,
       duration_ms, created_at
FROM workflow_runs
WHERE org_id = $1 AND id = $2;

-- name: GetRunAgentID :one
-- Resolves a run's agent_id via its workflow — Launcher.Resume needs this
-- before it can rebuild the RunDeps/Nodes a suspended run's checkpoint
-- alone doesn't carry (agent_id isn't part of RunState's own JSON).
SELECT w.agent_id
FROM workflow_runs wr
JOIN workflows w ON w.id = wr.workflow_id
WHERE wr.org_id = $1 AND wr.id = $2;

-- name: ListRunsForOrg :many
SELECT id, workflow_id, status, output, cost_so_far_usd, started_at, completed_at,
       duration_ms, created_at
FROM workflow_runs
WHERE org_id = $1
  AND (sqlc.narg(status)::varchar IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(workflow_id)::uuid IS NULL OR workflow_id = sqlc.narg(workflow_id))
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;
