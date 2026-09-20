-- Workflow 18 (Multi-Agent Team Run / A2A). agent_teams/agent_team_members
-- and workflows.team_id/agents.{a2a_endpoint,team_role,a2a_manifest} have
-- existed, RLS-covered, since 000001/000002 -- nobody ever wrote to them.
-- This migration adds the 3 columns workflow_runs itself is still missing
-- to represent a specialist's delegated sub-run without disturbing any
-- existing single-agent run row or query:
--
--   parent_run_id  -- links a specialist's run back to the orchestrator's
--                     run that dispatched it via A2A tasks/send. NULL for
--                     every ordinary (non-team) run, and for a team's own
--                     orchestrator run itself.
--   agent_id       -- the actual agent this specific run executed. Every
--                     existing run resolves its agent transitively via
--                     workflow_id -> workflows.agent_id (see
--                     GetRunAgentID); a team run's child rows have no
--                     single-agent "workflow" of their own to derive this
--                     from (they share the team's one workflow row, whose
--                     agent_id is the orchestrator's), so it's stored
--                     directly instead. NULL means "derive from workflow",
--                     preserving every existing row/query untouched.
--   delegated_role -- the team_role (e.g. "finance", "ops") this sub-run
--                     played, purely for trace-UI labeling -- avoids a
--                     join back to agent_team_members (which has no
--                     history once a member is removed from a team) just
--                     to label a past run.
--
-- No RLS change needed: workflow_runs already carries its own org_id
-- directly on every row (parent and child alike), so the existing
-- tenant_isolation policy already covers these columns.
ALTER TABLE workflow_runs ADD COLUMN parent_run_id uuid REFERENCES workflow_runs(id) ON DELETE CASCADE;
ALTER TABLE workflow_runs ADD COLUMN agent_id uuid REFERENCES agents(id);
ALTER TABLE workflow_runs ADD COLUMN delegated_role varchar(50);

CREATE INDEX idx_workflow_runs_parent_run_id ON workflow_runs(parent_run_id);
