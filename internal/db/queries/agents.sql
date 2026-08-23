-- Queries backing workflow 7 (agent configuration CRUD). All tenant-scoped,
-- run through app_user via tenant.WithTx like every other feature area in
-- this file set — nothing here is cross-tenant, unlike the recovery-sweep
-- style queries elsewhere.

-- name: ListAgents :many
SELECT id, name, slug, description, agent_type, model, system_prompt,
       context_window_tokens, max_output_tokens, temperature,
       policy_scope, allowed_mcp_servers, is_active, version,
       created_at, updated_at
FROM agents
WHERE org_id = $1 AND is_active = true
ORDER BY created_at DESC;

-- name: GetAgent :one
SELECT id, name, slug, description, agent_type, model, system_prompt,
       context_window_tokens, max_output_tokens, temperature,
       policy_scope, allowed_mcp_servers, is_active, version,
       created_at, updated_at
FROM agents
WHERE org_id = $1 AND id = $2;

-- name: CountActiveAgents :one
SELECT COUNT(*) FROM agents WHERE org_id = $1 AND is_active = true;

-- name: GetOrganizationMaxAgents :one
SELECT max_agents FROM organizations WHERE id = $1;

-- name: InsertAgent :one
-- ON CONFLICT ... DO NOTHING against the partial unique index added in
-- 000006_agents_unique_org_name_active: a real duplicate name returns 0
-- rows (pgx.ErrNoRows on the :one Scan), which the handler translates to a
-- 400 DUPLICATE_AGENT_NAME rather than a generic 500.
INSERT INTO agents (
    org_id, name, slug, description, agent_type, model, system_prompt,
    max_output_tokens, temperature, policy_scope, allowed_mcp_servers, created_by
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (org_id, name) WHERE is_active = true DO NOTHING
RETURNING id, name, slug, description, agent_type, model, system_prompt,
    context_window_tokens, max_output_tokens, temperature,
    policy_scope, allowed_mcp_servers, is_active, version,
    created_at, updated_at;

-- name: UpdateAgent :one
-- Partial update via COALESCE against sqlc.narg — every field is optional
-- on the PATCH wire contract; only the ones actually present in the
-- request are non-nil here. A rename that collides with another active
-- agent's name still hits the 000006 partial unique index (COALESCE can't
-- route around a real constraint), surfacing as a Postgres 23505 the
-- handler translates the same way InsertAgent's ON CONFLICT branch does.
UPDATE agents SET
    name = COALESCE(sqlc.narg(name), name),
    description = COALESCE(sqlc.narg(description), description),
    agent_type = COALESCE(sqlc.narg(agent_type), agent_type),
    model = COALESCE(sqlc.narg(model), model),
    system_prompt = COALESCE(sqlc.narg(system_prompt), system_prompt),
    max_output_tokens = COALESCE(sqlc.narg(max_output_tokens), max_output_tokens),
    temperature = COALESCE(sqlc.narg(temperature), temperature),
    policy_scope = COALESCE(sqlc.narg(policy_scope), policy_scope),
    allowed_mcp_servers = COALESCE(sqlc.narg(allowed_mcp_servers), allowed_mcp_servers),
    version = version + 1
WHERE org_id = sqlc.arg(org_id) AND id = sqlc.arg(id)
RETURNING id, name, slug, description, agent_type, model, system_prompt,
    context_window_tokens, max_output_tokens, temperature,
    policy_scope, allowed_mcp_servers, is_active, version,
    created_at, updated_at;

-- name: DeactivateAgent :execrows
-- Soft delete — is_active=false, row stays for run history (Workflow 9+).
-- :execrows (not :exec) so the handler can distinguish "deactivated" from
-- "no such agent in this org" (0 rows) and return a real 404.
UPDATE agents SET is_active = false WHERE org_id = $1 AND id = $2 AND is_active = true;
