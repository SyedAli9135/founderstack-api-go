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
-- w.team_id (workflow 18) lets the frontend tell a team's orchestrator run
-- apart from an ordinary single-agent run when someone lands on the plain
-- GET /runs/{id} page directly (an old bookmark, a shared link, or the
-- founder just browsing /runs — that list has no other way to distinguish
-- them either, see ListRunsForOrg below) — the run page redirects to the
-- correct /agents/teams/{team_id}/runs/{id} view instead of rendering a
-- single-agent pipeline that doesn't know what a "delegate" node is.
SELECT wr.id, wr.workflow_id, wr.status, wr.current_node, wr.triggered_by, wr.output,
       wr.input_tokens, wr.output_tokens, wr.cached_tokens, wr.cost_so_far_usd,
       wr.tool_call_count, wr.started_at, wr.completed_at, wr.duration_ms, wr.created_at,
       w.team_id
FROM workflow_runs wr
JOIN workflows w ON w.id = wr.workflow_id
WHERE wr.org_id = $1 AND wr.id = $2;

-- name: GetRunAgentID :one
-- Resolves a run's agent_id via its workflow — Launcher.Resume needs this
-- before it can rebuild the RunDeps/Nodes a suspended run's checkpoint
-- alone doesn't carry (agent_id isn't part of RunState's own JSON).
-- COALESCE(wr.agent_id, ...) is workflow 18's addition: a team's specialist
-- sub-runs all share the team's one `workflows` row (whose agent_id is the
-- orchestrator's, not theirs), so their real agent is stored directly on
-- workflow_runs.agent_id instead — NULL there for every ordinary run
-- (and the orchestrator's own run), which falls through to the original
-- workflow-derived lookup unchanged.
SELECT COALESCE(wr.agent_id, w.agent_id) AS agent_id
FROM workflow_runs wr
JOIN workflows w ON w.id = wr.workflow_id
WHERE wr.org_id = $1 AND wr.id = $2;

-- name: ListRunsForOrg :many
-- parent_run_id IS NULL excludes a team's specialist sub-runs from the
-- flat run list — a founder browsing "my runs" sees the team run as one
-- row; its specialists only surface via GET /teams/{id}/runs/{run_id}'s
-- aggregated trace (workflow 18). w.team_id (also workflow 18) is what
-- lets that one row actually be *labeled* as a team run and link
-- correctly — before this, a team run looked identical to any other row
-- here, and clicking it opened the wrong page entirely (a real, reported
-- confusion, not a hypothetical).
SELECT wr.id, wr.workflow_id, wr.status, wr.output, wr.cost_so_far_usd, wr.started_at,
       wr.completed_at, wr.duration_ms, wr.created_at, w.team_id
FROM workflow_runs wr
JOIN workflows w ON w.id = wr.workflow_id
WHERE wr.org_id = $1
  AND wr.parent_run_id IS NULL
  AND (sqlc.narg(status)::varchar IS NULL OR wr.status = sqlc.narg(status))
  AND (sqlc.narg(workflow_id)::uuid IS NULL OR wr.workflow_id = sqlc.narg(workflow_id))
ORDER BY wr.created_at DESC
LIMIT $2 OFFSET $3;

-- name: ListTeamRuns :many
-- One team's own run history — the "Recent runs" list workflow 18's team
-- detail page needs (there was no way to get back to a past run's page
-- before this; the only route in was the URL Launch's own response
-- handed back right after triggering it, gone the moment you navigated
-- away). team_id resolves through the team's one shared `workflows` row
-- (see InsertTeamWorkflow) rather than a direct column on workflow_runs —
-- consistent with how every other team-scoped run query in this file
-- reaches team_id.
SELECT wr.id, wr.status, wr.output, wr.cost_so_far_usd, wr.started_at, wr.completed_at,
       wr.duration_ms, wr.created_at
FROM workflow_runs wr
JOIN workflows w ON w.id = wr.workflow_id
WHERE wr.org_id = $1 AND w.team_id = $2 AND wr.parent_run_id IS NULL
ORDER BY wr.created_at DESC
LIMIT $3 OFFSET $4;

-- name: InsertTeamWorkflowRun :one
-- Workflow 18's variant of InsertWorkflowRun — used for both the
-- orchestrator's own run (id server-generated, parent_run_id NULL) and
-- each specialist's delegated sub-run (id explicit — see below),
-- always with an explicit agent_id rather than InsertWorkflowRun's
-- workflow-derived one. See GetRunAgentID's and
-- workflow_runs.parent_run_id's doc comments above. id is
-- sqlc.narg(id): NULL lets Postgres's own gen_random_uuid() default fill
-- it (the orchestrator's own run); a specialist's sub-run instead reuses
-- the exact id its delegate node already minted client-side (see
-- graph.RunDeps.A2AClient.Dispatch's doc comment) before dispatch, so the
-- id EventBus.LinkChild registered for matches the id the specialist's
-- own engine run actually publishes events under.
INSERT INTO workflow_runs (id, workflow_id, org_id, triggered_by, agent_id, parent_run_id, delegated_role, status)
VALUES (COALESCE(sqlc.narg(id)::uuid, gen_random_uuid()), $1, $2, $3, $4, $5, $6, 'pending')
RETURNING id, status, created_at;

-- name: GetRunTeamAndWorkflow :one
-- Resolves a run's team (and the one workflow row every run in that team
-- shares — see InsertTeamWorkflow) via its own workflow_id — the a2a
-- tasks/send handler uses this (via the request's sessionId, the
-- dispatching orchestrator's own run) to find which team the target agent
-- must belong to, rather than trusting a client-supplied team id, and
-- which workflow_id to insert the specialist's own sub-run row against.
SELECT w.team_id, wr.workflow_id
FROM workflow_runs wr
JOIN workflows w ON w.id = wr.workflow_id
WHERE wr.org_id = $1 AND wr.id = $2;

-- name: ListChildRuns :many
-- The specialist sub-runs a team's orchestrator run dispatched — how
-- GET /teams/{id}/runs/{run_id} builds its aggregated, per-specialist
-- trace. delegated_role/agent_id are always set on these rows (see
-- InsertTeamWorkflowRun), so no join back to agent_team_members is needed
-- (and wouldn't survive a member later being removed from the team).
SELECT wr.id, wr.agent_id, a.name AS agent_name, wr.delegated_role, wr.status,
       wr.current_node, wr.output, wr.cost_so_far_usd, wr.input_tokens, wr.output_tokens,
       wr.started_at, wr.completed_at, wr.duration_ms, wr.created_at
FROM workflow_runs wr
JOIN agents a ON a.id = wr.agent_id
WHERE wr.org_id = $1 AND wr.parent_run_id = $2
ORDER BY wr.created_at ASC;

-- Workflow 11 (run trace + cost). writeWorkflowStep (internal/core/graph/
-- observability.go) is the one place InsertWorkflowStep is called from,
-- fired at each of the 5 node functions plus once per LLM turn and once
-- per tool call inside executorNode -- see BuildNodes' doc comment.

-- name: InsertWorkflowStep :exec
INSERT INTO workflow_steps (run_id, node_name, step_type, agent_name, input_data,
                             output_data, input_tokens, output_tokens, duration_ms, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: ListWorkflowSteps :many
-- Joins workflow_runs for org-scoping (workflow_steps itself has no
-- org_id column) -- same shape as GetRunAgentID's join above.
SELECT ws.id, ws.node_name, ws.step_type, ws.agent_name, ws.input_data, ws.output_data,
       ws.input_tokens, ws.output_tokens, ws.duration_ms, ws.status, ws.created_at
FROM workflow_steps ws
JOIN workflow_runs r ON r.id = ws.run_id
WHERE r.org_id = $1 AND r.id = $2
ORDER BY ws.created_at ASC;

-- name: GetRunCostBreakdown :many
SELECT cost_type,
       COALESCE(SUM(estimated_cost_usd), 0)::double precision AS total_usd,
       COALESCE(SUM(input_tokens), 0)::bigint AS input_tokens,
       COALESCE(SUM(output_tokens), 0)::bigint AS output_tokens,
       COALESCE(SUM(cached_tokens), 0)::bigint AS cached_tokens
FROM cost_ledger
WHERE org_id = $1 AND run_id = $2
GROUP BY cost_type;

-- name: FinalizeRunHoursSaved :one
-- Only ever called once, right after FinalizeRun, guarded on the run
-- having just reached status='completed' -- see finalizeIfTerminal.
-- estimated_manual_minutes defaults to 15 (a conservative baseline) when
-- the workflow never had one set. parent_run_id IS NOT NULL (workflow 18's
-- specialist sub-runs) instead sets an explicit NULL, not a computed
-- estimate -- only the orchestrator's own run represents the end-to-end
-- task a founder would otherwise have done by hand; accruing hours_saved
-- per specialist too would double- (or N-times-) count the same task.
-- accrueHoursSaved's existing "hoursSaved == nil -> nothing to accrue"
-- check already handles that NULL correctly, so no Go-side change is
-- needed for this guard.
UPDATE workflow_runs
SET hours_saved = CASE
    WHEN workflow_runs.parent_run_id IS NOT NULL THEN NULL
    ELSE COALESCE(
        (SELECT w.estimated_manual_minutes FROM workflows w WHERE w.id = workflow_runs.workflow_id),
        15
    )::double precision / 60.0
END
WHERE workflow_runs.org_id = $1 AND workflow_runs.id = $2
RETURNING hours_saved;

-- name: IncrementOrgTotalHoursSaved :exec
UPDATE organizations SET total_hours_saved = total_hours_saved + $2 WHERE id = $1;

-- name: GetOrgTotalHoursSaved :one
SELECT total_hours_saved FROM organizations WHERE id = $1;

-- name: GetHoursSavedSince :one
SELECT COALESCE(SUM(hours_saved), 0)::double precision AS hours_saved
FROM workflow_runs
WHERE org_id = $1 AND completed_at >= $2;
