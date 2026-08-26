ALTER TABLE workflow_runs DROP COLUMN IF EXISTS duration_ms;
ALTER TABLE workflow_runs DROP COLUMN IF EXISTS cached_tokens;
ALTER TABLE workflow_runs DROP COLUMN IF EXISTS output_tokens;
ALTER TABLE workflow_runs DROP COLUMN IF EXISTS input_tokens;
ALTER TABLE workflow_runs DROP COLUMN IF EXISTS output;
ALTER TABLE workflow_runs DROP COLUMN IF EXISTS completed_at;
ALTER TABLE workflow_runs DROP COLUMN IF EXISTS started_at;
