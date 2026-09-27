-- Workflow 22 (SOP Library). Two trust boundaries, same split as practice.sql:
--   * reads that span tenants (a playbook's deployments across client
--     workspaces, with their names and connected integrations) run on
--     app_system, and every one is pinned to the practice_id the handler
--     already authorized plus the caller's own active memberships;
--   * writes run on app_user under tenant.WithTx — playbook edits scoped to
--     the practice, deploy/sync writes scoped to the target client workspace
--     (sop_deployments' WITH CHECK only admits rows for the current org).

-- name: ListSopPlaybooks :many
-- System pool: the deployment counts join client organizations, which the
-- practice's own RLS context can't see. Counts only workspaces the caller
-- belongs to, the same rule as ListSopDeploymentsForPlaybook, so a card's
-- count always matches the deployments list behind it.
SELECT p.id, p.name, p.description, p.category, p.current_version,
       p.agent_config, p.workflow_config, p.parameters, p.created_at, p.updated_at,
       (SELECT count(*) FROM sop_deployments d
          JOIN organizations o ON o.id = d.target_org_id AND o.is_active = true
          JOIN users u ON u.org_id = d.target_org_id AND u.clerk_user_id = sqlc.arg(clerk_user_id) AND u.is_active = true
         WHERE d.sop_playbook_id = p.id AND d.is_active = true)::bigint AS active_deployments,
       (SELECT count(*) FROM sop_deployments d
          JOIN organizations o ON o.id = d.target_org_id AND o.is_active = true
          JOIN users u ON u.org_id = d.target_org_id AND u.clerk_user_id = sqlc.arg(clerk_user_id) AND u.is_active = true
         WHERE d.sop_playbook_id = p.id AND d.is_active = true
           AND d.deployed_version < p.current_version)::bigint AS outdated_deployments
FROM sop_playbooks p
WHERE p.practice_id = sqlc.arg(practice_id) AND p.is_active = true
ORDER BY p.name;

-- name: GetSopPlaybook :one
SELECT id, name, description, category, current_version, agent_config, workflow_config,
       parameters, created_at, updated_at
FROM sop_playbooks
WHERE id = $1 AND practice_id = $2 AND is_active = true;

-- name: ListSopPlaybookVersions :many
SELECT version, changelog, created_at FROM sop_playbook_versions
WHERE sop_playbook_id = $1 ORDER BY version DESC;

-- name: GetSopPlaybookVersion :one
SELECT agent_config, workflow_config, parameters FROM sop_playbook_versions
WHERE sop_playbook_id = $1 AND version = $2;

-- name: InsertSopPlaybook :one
-- A name collision with another active SOP returns no row (ErrNoRows), same
-- convention as InsertAgent.
INSERT INTO sop_playbooks (practice_id, name, description, category, agent_config, workflow_config, parameters, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (practice_id, name) WHERE is_active = true DO NOTHING
RETURNING id;

-- name: InsertSopPlaybookVersion :exec
INSERT INTO sop_playbook_versions (sop_playbook_id, version, agent_config, workflow_config, parameters, changelog, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: UpdateSopPlaybookMeta :execrows
-- Name/description/category aren't part of what a deployment renders, so
-- they change in place without a new version.
UPDATE sop_playbooks SET
    name = COALESCE(sqlc.narg(name), name),
    description = COALESCE(sqlc.narg(description), description),
    category = COALESCE(sqlc.narg(category), category)
WHERE id = sqlc.arg(id) AND practice_id = sqlc.arg(practice_id) AND is_active = true;

-- name: BumpSopPlaybookVersion :one
-- The row lock this UPDATE takes serializes concurrent edits, and
-- sop_playbook_versions' UNIQUE(sop_playbook_id, version) backs it up.
UPDATE sop_playbooks SET
    agent_config = $3, workflow_config = $4, parameters = $5,
    current_version = current_version + 1
WHERE id = $1 AND practice_id = $2 AND is_active = true
RETURNING current_version;

-- name: DeactivateSopPlaybook :execrows
UPDATE sop_playbooks SET is_active = false WHERE id = $1 AND practice_id = $2 AND is_active = true;

-- name: ListSopDeploymentsForPlaybook :many
-- Only workspaces the caller belongs to, same rule as the portfolio.
SELECT d.id, d.target_org_id, o.name AS workspace_name, o.is_active AS workspace_active,
       d.deployed_version, d.parameter_overrides, d.agent_id, d.workflow_id,
       d.deployed_at, d.synced_at,
       COALESCE(a.is_active, false)::boolean AS agent_active,
       ARRAY(SELECT m.service_name FROM mcp_connections m
              WHERE m.org_id = d.target_org_id AND m.is_active = true)::text[] AS connected_services
FROM sop_deployments d
JOIN organizations o ON o.id = d.target_org_id
JOIN agents a ON a.id = d.agent_id
JOIN users u ON u.org_id = d.target_org_id AND u.clerk_user_id = $2 AND u.is_active = true
WHERE d.sop_playbook_id = $1 AND d.is_active = true
ORDER BY o.name;

-- name: GetSopDeployment :one
SELECT d.id, d.target_org_id, d.deployed_version, d.parameter_overrides, d.agent_id, d.workflow_id
FROM sop_deployments d
WHERE d.id = $1 AND d.sop_playbook_id = $2 AND d.is_active = true;

-- name: HasActiveSopDeployment :one
SELECT EXISTS (
    SELECT 1 FROM sop_deployments
    WHERE sop_playbook_id = $1 AND target_org_id = $2 AND is_active = true
)::boolean;

-- name: ListConnectedServices :many
SELECT service_name FROM mcp_connections WHERE org_id = $1 AND is_active = true;

-- Client-side writes (tenant.WithTx on the target workspace).

-- name: InsertSopDeployment :one
INSERT INTO sop_deployments (sop_playbook_id, sop_name, deployed_version, target_org_id, agent_id, workflow_id, parameter_overrides, deployed_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, deployed_at;

-- name: MarkSopDeploymentSynced :exec
UPDATE sop_deployments SET
    deployed_version = $3, parameter_overrides = $4, sop_name = $5, workflow_id = $6, synced_at = now()
WHERE id = $1 AND target_org_id = $2;

-- name: DeactivateSopDeployment :execrows
UPDATE sop_deployments SET is_active = false WHERE id = $1 AND target_org_id = $2 AND is_active = true;

-- name: SyncSopAgent :one
-- Overwrites every SOP-controlled field. Returns no row if the client has
-- since removed the agent, which the handler reports as a broken deployment.
UPDATE agents SET
    name = $3, description = $4, agent_type = $5, model = $6, system_prompt = $7,
    max_output_tokens = $8, temperature = $9, policy_scope = $10, allowed_mcp_servers = $11,
    version = version + 1
WHERE org_id = $1 AND id = $2 AND is_active = true
RETURNING id;

-- name: SyncSopWorkflow :execrows
-- Deliberately leaves is_active alone: a sync must never un-pause a
-- workflow the client paused.
UPDATE workflows SET
    name = $3, description = $4, trigger_type = $5, cron_expression = $6, next_run_at = $7,
    requires_approval = $8, task_input_template = $9, estimated_manual_minutes = $10,
    version = version + 1
WHERE org_id = $1 AND id = $2;

-- name: DeactivateSopAgent :exec
UPDATE agents SET is_active = false WHERE org_id = $1 AND id = $2;

-- name: PauseSopWorkflow :exec
UPDATE workflows SET is_active = false WHERE org_id = $1 AND id = $2;

-- name: ListSopManagedResources :many
-- Tenant-scoped (app_user in the client workspace): which of this
-- workspace's agents/workflows a SOP deployment manages, for the "Managed by
-- SOP" labels. sop_name is the deployment's own snapshot, since the
-- practice's playbook rows are invisible from here.
SELECT agent_id, workflow_id, sop_name, deployed_version
FROM sop_deployments
WHERE target_org_id = $1 AND is_active = true;

-- name: DetachSopDeployments :exec
-- System pool: runs with the library delete, which executes in the
-- practice's context, while deployments are only writable from their client
-- workspace's. Stops tracking only — the agents/workflows are untouched and
-- keep running; this just clears their "Managed by SOP" labels, since a
-- deleted SOP can never sync them again.
UPDATE sop_deployments SET is_active = false
WHERE sop_playbook_id = $1 AND is_active = true;
