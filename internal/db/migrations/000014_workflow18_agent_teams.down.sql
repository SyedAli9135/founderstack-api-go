DROP INDEX IF EXISTS idx_workflow_runs_parent_run_id;
ALTER TABLE workflow_runs DROP COLUMN IF EXISTS delegated_role;
ALTER TABLE workflow_runs DROP COLUMN IF EXISTS agent_id;
ALTER TABLE workflow_runs DROP COLUMN IF EXISTS parent_run_id;
