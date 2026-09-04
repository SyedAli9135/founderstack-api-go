-- Workflow 11 (View Run Trace & Cost). workflow_steps has existed since
-- migration 000001 but nothing has ever written to it -- the engine only
-- ever published SSE events and wrote cost_ledger/audit_logs, never a
-- persisted per-step trace. agent_name/input_tokens/output_tokens are new:
-- the original table only had node_name/step_type/input_data/output_data/
-- duration_ms/status, missing 2 of the fields WORKFLOW_PLAN_GO.md's
-- acceptance criteria call for per step.
ALTER TABLE workflow_steps ADD COLUMN agent_name varchar(255);
ALTER TABLE workflow_steps ADD COLUMN input_tokens integer;
ALTER TABLE workflow_steps ADD COLUMN output_tokens integer;

-- hours_saved: per-run figure (workflows.estimated_manual_minutes / 60, or
-- a 15-minute default), set once a run completes successfully.
-- total_hours_saved: the org's running total, accumulated at the same time
-- -- GET /analytics/hours-saved reads this directly instead of re-summing
-- every run on each request.
ALTER TABLE workflow_runs ADD COLUMN hours_saved double precision;
ALTER TABLE organizations ADD COLUMN total_hours_saved double precision NOT NULL DEFAULT 0;
