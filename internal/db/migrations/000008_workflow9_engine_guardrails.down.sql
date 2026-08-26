ALTER TABLE organizations DROP COLUMN IF EXISTS agents_paused;

ALTER TABLE approvals DROP COLUMN IF EXISTS risk_level;

ALTER TABLE workflow_runs DROP COLUMN IF EXISTS tool_call_count;
ALTER TABLE workflow_runs DROP COLUMN IF EXISTS cost_so_far_usd;
ALTER TABLE workflow_runs DROP COLUMN IF EXISTS current_node;
ALTER TABLE workflow_runs DROP COLUMN IF EXISTS checkpoint_state;
