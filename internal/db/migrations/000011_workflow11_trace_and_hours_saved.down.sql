ALTER TABLE organizations DROP COLUMN IF EXISTS total_hours_saved;
ALTER TABLE workflow_runs DROP COLUMN IF EXISTS hours_saved;
ALTER TABLE workflow_steps DROP COLUMN IF EXISTS output_tokens;
ALTER TABLE workflow_steps DROP COLUMN IF EXISTS input_tokens;
ALTER TABLE workflow_steps DROP COLUMN IF EXISTS agent_name;
