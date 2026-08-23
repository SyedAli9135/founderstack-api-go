-- Workflow 7's acceptance criteria says "System rejects duplicate agent
-- names within the same org", but nothing enforced that at the DB level —
-- same gap-fix pattern as 000004 (api_key_registry) and 000005
-- (mcp_connections). Scoped to is_active = true (a partial index, not a
-- plain UNIQUE constraint — Postgres constraints can't carry a WHERE
-- clause, only indexes can) so a deactivated agent's name doesn't block
-- reusing it for a brand-new one.
CREATE UNIQUE INDEX idx_agents_org_name_active ON agents(org_id, name) WHERE is_active = true;
