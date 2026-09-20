-- Queries backing workflow 19 (Agent Templates Marketplace). Deliberately
-- run through app_user like everything else in this file set — not
-- app_system — even though agent_templates has no org_id/RLS of its own:
-- a request handler already runs inside tenant.WithTx for its other work
-- (the install path also touches agents, which IS RLS-scoped), and there
-- is no cross-tenant write here that would need app_system's bypass.

-- name: ListAgentTemplates :many
SELECT id, name, description, category, icon, is_featured,
       jsonb_array_length(policy_scope->'allowed_tools') AS tool_count
FROM agent_templates
WHERE is_active = true
  AND (sqlc.narg(category)::varchar IS NULL OR category = sqlc.narg(category))
ORDER BY is_featured DESC, name ASC;

-- name: GetAgentTemplate :one
SELECT id, name, description, category, system_prompt, model, policy_scope, icon, is_featured
FROM agent_templates
WHERE id = $1 AND is_active = true;
