-- Queries backing workflow 8 (workflow config CRUD + scheduling). Most are
-- tenant-scoped through app_user via tenant.WithTx, like every other
-- feature area in this file set. The 3 queries under "background
-- scheduler" are the deliberate exception — the scheduler scans due
-- workflows across every org, which is inherently a cross-tenant
-- system-context operation, so those run on app_system directly, same
-- reasoning as internal/core/integrations/refresh.go's RunRefreshJob and
-- internal/core/documents/recover.go's RecoverStuckJobs.

-- name: ValidateAgentForOrg :one
-- Used at create time to confirm the agent_id a workflow is being pointed
-- at both belongs to this org and is still active — you shouldn't be able
-- to assign a workflow to a deactivated agent. Also returns name so the
-- Create response can embed agent_name without a second query.
SELECT id, name FROM agents WHERE org_id = $1 AND id = $2 AND is_active = true;

-- name: ListWorkflows :many
-- Deliberately NOT filtered by is_active, unlike ListAgents/ListDocuments
-- — pausing a workflow (PATCH is_active=false) must keep it visible so the
-- founder can toggle it back on; is_active in the response drives the
-- pause/resume toggle and "Paused" badge client-side. agent_name comes via
-- a plain JOIN, not a nullable LEFT JOIN: agent_id is NOT NULL and agents
-- are only ever soft-deleted (never actually removed), so the referenced
-- row always exists.
SELECT w.id, w.agent_id, a.name AS agent_name, w.name, w.description,
       w.trigger_type, w.cron_expression, w.timezone, w.next_run_at,
       w.requires_approval, w.task_input_template, w.estimated_manual_minutes,
       w.is_active, w.version, w.created_at, w.updated_at
FROM workflows w
JOIN agents a ON a.id = w.agent_id
WHERE w.org_id = $1
ORDER BY w.created_at DESC;

-- name: GetWorkflow :one
SELECT w.id, w.agent_id, a.name AS agent_name, w.name, w.description,
       w.trigger_type, w.cron_expression, w.timezone, w.next_run_at,
       w.requires_approval, w.task_input_template, w.estimated_manual_minutes,
       w.is_active, w.version, w.created_at, w.updated_at
FROM workflows w
JOIN agents a ON a.id = w.agent_id
WHERE w.org_id = $1 AND w.id = $2;

-- name: InsertWorkflow :one
-- graph_definition is fixed, not user-configurable — every workflow runs
-- the same planner -> rag_retriever -> executor -> validator -> reporter
-- pipeline (Workflow 9's node list); nothing in this workflow's spec lets
-- a founder customize the graph shape itself, only the trigger/schedule
-- and the agent + task input that pipeline runs against.
INSERT INTO workflows (
    org_id, agent_id, name, description, trigger_type, graph_definition,
    cron_expression, next_run_at, requires_approval, task_input_template,
    estimated_manual_minutes, created_by
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING id, agent_id, name, description, trigger_type, cron_expression,
    timezone, next_run_at, requires_approval, task_input_template,
    estimated_manual_minutes, is_active, version, created_at, updated_at;

-- name: UpdateWorkflow :one
-- Partial update via COALESCE, same convention as UpdateAgent — but
-- trigger_type/cron_expression/next_run_at are 3 fields the *handler*
-- treats as one coupled unit (see internal/api/workflows/handler.go's
-- Update): it always passes explicit values for all 3 together, computed
-- from the effective (existing-unless-overridden) trigger config, rather
-- than relying on COALESCE to reconcile them independently — a bare
-- COALESCE per-field could otherwise land on a state like
-- trigger_type='scheduled' with a stale or absent cron_expression.
-- agent_id is intentionally not updatable here — reassigning a workflow to
-- a different agent isn't part of this workflow's spec.
UPDATE workflows SET
    name = COALESCE(sqlc.narg(name), name),
    description = COALESCE(sqlc.narg(description), description),
    trigger_type = COALESCE(sqlc.narg(trigger_type), trigger_type),
    cron_expression = sqlc.narg(cron_expression),
    next_run_at = sqlc.narg(next_run_at),
    requires_approval = COALESCE(sqlc.narg(requires_approval), requires_approval),
    task_input_template = COALESCE(sqlc.narg(task_input_template), task_input_template),
    estimated_manual_minutes = COALESCE(sqlc.narg(estimated_manual_minutes), estimated_manual_minutes),
    is_active = COALESCE(sqlc.narg(is_active), is_active),
    version = version + 1
WHERE org_id = sqlc.arg(org_id) AND id = sqlc.arg(id)
RETURNING id, agent_id, name, description, trigger_type, cron_expression,
    timezone, next_run_at, requires_approval, task_input_template,
    estimated_manual_minutes, is_active, version, created_at, updated_at;

-- name: DeactivateWorkflow :execrows
-- Semantically identical to PATCH {is_active: false} (pause) — kept as its
-- own endpoint only for symmetry with every other resource's DELETE verb
-- in this codebase, not because it does anything a pause doesn't already
-- do. See CLAUDE.md's workflow 8 section for why workflows don't have a
-- separate "really gone" state the way documents/agents effectively do.
UPDATE workflows SET is_active = false WHERE org_id = $1 AND id = $2 AND is_active = true;

-- name: InsertWorkflowRun :one
-- "Run Now" and the scheduler both stop here — INSERT a pending run row,
-- nothing consumes it yet. See CLAUDE.md's workflow 8 section: turning
-- 'pending' rows into actual execution is Workflow 9's job
-- (internal/core/graph, not built).
INSERT INTO workflow_runs (workflow_id, org_id, triggered_by, status)
VALUES ($1, $2, $3, 'pending')
RETURNING id, status, created_at;

-- name: GetWorkflowRun :one
SELECT id, workflow_id, status, triggered_by FROM workflow_runs WHERE org_id = $1 AND id = $2;

-- Background scheduler (internal/core/workflows/scheduler.go) — app_system,
-- never tenant.WithTx; see the file-level comment above.

-- name: ListDueScheduledWorkflows :many
SELECT id, org_id, cron_expression FROM workflows
WHERE trigger_type = 'scheduled' AND is_active = true AND next_run_at <= now();

-- name: UpdateWorkflowNextRunAt :exec
UPDATE workflows SET next_run_at = $2 WHERE id = $1;

-- name: SystemInsertWorkflowRun :exec
-- Same shape as InsertWorkflowRun (triggered_by is NULL — the system, not
-- a user, triggered this one), used because the scheduler has no
-- per-request user/org session to run InsertWorkflowRun's tenant.WithTx
-- variant under.
INSERT INTO workflow_runs (workflow_id, org_id, status)
VALUES ($1, $2, 'pending');
