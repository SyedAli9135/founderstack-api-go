-- Queries backing workflow 18 (Multi-Agent Team Run / A2A). agent_teams
-- and agent_team_members have existed, RLS-covered, since 000001/000002 —
-- these are the first queries ever written against them. All tenant-scoped
-- through app_user via tenant.WithTx, same as every other feature area in
-- this file set — team membership and A2A dispatch never cross an org
-- boundary.

-- name: InsertAgentTeam :one
INSERT INTO agent_teams (org_id, name, description, orchestrator_agent_id)
VALUES ($1, $2, $3, $4)
RETURNING id, name, description, orchestrator_agent_id, max_agent_hops,
    parallel_execution, timeout_seconds, is_active, created_at, updated_at;

-- name: InsertAgentTeamMember :one
INSERT INTO agent_team_members (team_id, agent_id, role, priority)
VALUES ($1, $2, $3, $4)
RETURNING id, team_id, agent_id, role, priority;

-- name: ListAgentTeamsForOrg :many
-- member_count is a correlated subquery, same reasoning as ListAgents'
-- workflow_count — a team with 0 members (mid-setup, or every member
-- since removed) must still appear exactly once.
SELECT t.id, t.name, t.description, t.orchestrator_agent_id, o.name AS orchestrator_agent_name,
       t.max_agent_hops, t.parallel_execution, t.timeout_seconds, t.is_active,
       t.created_at, t.updated_at,
       (SELECT COUNT(*) FROM agent_team_members m WHERE m.team_id = t.id) AS member_count
FROM agent_teams t
JOIN agents o ON o.id = t.orchestrator_agent_id
WHERE t.org_id = $1 AND t.is_active = true
ORDER BY t.created_at DESC;

-- name: GetAgentTeam :one
-- is_active = true, unlike GetWorkflow's deliberate "show it either way"
-- choice — a deactivated team has no equivalent of a workflow's
-- pause/resume toggle (DeactivateAgentTeam is a one-way soft delete, see
-- its own doc comment), so a founder should never be able to Get or Run
-- one again after deleting it.
SELECT t.id, t.name, t.description, t.orchestrator_agent_id, o.name AS orchestrator_agent_name,
       t.max_agent_hops, t.parallel_execution, t.timeout_seconds, t.is_active,
       t.created_at, t.updated_at
FROM agent_teams t
JOIN agents o ON o.id = t.orchestrator_agent_id
WHERE t.org_id = $1 AND t.id = $2 AND t.is_active = true;

-- name: ListAgentTeamMembers :many
-- Joined against agents for display (name, description, model) — the
-- specialist card the team page and the multi-agent pipeline UI both need,
-- without a second round trip per member.
SELECT m.id, m.agent_id, a.name AS agent_name, a.description AS agent_description,
       m.role, m.priority
FROM agent_team_members m
JOIN agents a ON a.id = m.agent_id
WHERE m.team_id = $1
ORDER BY m.priority ASC, m.created_at ASC;

-- name: ValidateAgentForTeamMembership :one
-- Same "must belong to this org and be active" guard InsertWorkflow's
-- ValidateAgentForOrg already applies to workflows — a team's orchestrator
-- and every specialist must pass it too.
SELECT id, name FROM agents WHERE org_id = $1 AND id = $2 AND is_active = true;

-- name: DeactivateAgentTeam :execrows
-- One-way, unlike workflows' pause/resume DeactivateWorkflow — a team has
-- no equivalent "reactivate" endpoint (see GetAgentTeam's own note).
UPDATE agent_teams SET is_active = false WHERE org_id = $1 AND id = $2 AND is_active = true;

-- name: IsAgentOnActiveTeam :one
-- Gates internal/core/a2a's manifest/tasks-send endpoints: only an agent
-- that is (still) a member of at least one active team is A2A-addressable.
-- Checked live against agent_team_members/agent_teams, not a cached flag —
-- same "don't trust a second source of truth" reasoning as workflow 9's
-- policy_scope decision (see CLAUDE.md's Agent Execution Engine section) —
-- a member removed from every team loses A2A reachability immediately, not
-- whenever some denormalized column next happens to get refreshed.
SELECT EXISTS (
    SELECT 1 FROM agent_team_members m
    JOIN agent_teams t ON t.id = m.team_id
    WHERE m.agent_id = $1 AND t.org_id = $2 AND t.is_active = true
) AS on_active_team;

-- name: GetAgentTeamMemberRole :one
-- The real authorization check behind POST .../a2a/agents/{agent_id}/tasks/send:
-- an orchestrator's dispatching run resolves its own team via
-- GetRunTeamID, then this confirms the requested target agent is actually
-- a member of *that* team (pgx.ErrNoRows if not, treated as 403) — never
-- trusts a client-supplied role or team id.
SELECT role FROM agent_team_members WHERE team_id = $1 AND agent_id = $2;

-- name: GetAgentForA2AManifest :one
-- The fields internal/core/a2a/manifest.go needs to build an A2A agent
-- card — name/description/model for display, policy_scope's allowed_tools
-- for the card's declared "skills" (an external caller should be able to
-- see what a specialist can actually do before dispatching a task to it).
SELECT id, org_id, name, description, model, team_role, policy_scope, version
FROM agents
WHERE org_id = $1 AND id = $2 AND is_active = true;
