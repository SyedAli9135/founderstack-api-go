-- Workflow 9's HTTP layer (GET /runs, GET /runs/{id}, the SSE stream's
-- final `complete` event) needs to report a run's output, token usage,
-- and duration once it finishes -- none of these columns exist yet.
-- workflow_runs previously only had a generic `run_trace jsonb` plus the
-- checkpoint_state/current_node/cost_so_far_usd/tool_call_count columns
-- migration 000008 added for the engine itself. See WORKFLOW_PLAN_GO.md's
-- Workflow 9 harness planning notes.
ALTER TABLE workflow_runs ADD COLUMN started_at timestamptz;
ALTER TABLE workflow_runs ADD COLUMN completed_at timestamptz;
ALTER TABLE workflow_runs ADD COLUMN output text;
ALTER TABLE workflow_runs ADD COLUMN input_tokens integer NOT NULL DEFAULT 0;
ALTER TABLE workflow_runs ADD COLUMN output_tokens integer NOT NULL DEFAULT 0;
ALTER TABLE workflow_runs ADD COLUMN cached_tokens integer NOT NULL DEFAULT 0;
ALTER TABLE workflow_runs ADD COLUMN duration_ms integer;
