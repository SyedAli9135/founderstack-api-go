-- Workflow 9 (execution engine) needs 3 things the schema doesn't have yet.
-- See WORKFLOW_PLAN_GO.md's Workflow 9 harness planning notes (2026-08-26) and the
-- project_founderstack_workflow9_harness_plan memory for the full reasoning behind
-- each one. Deliberately does NOT touch `agents` — max_tool_calls/max_cost_per_run_usd/
-- allowed_tools already live inside agents.policy_scope JSONB (workflow 7), already
-- validated; adding dedicated columns for those would create a second, conflicting
-- source of truth.

-- workflow_runs: checkpoint/resume state (the interrupt()/thread_id replacement) plus
-- the two running counters the engine checks against agents.policy_scope's caps after
-- every tool call.
ALTER TABLE workflow_runs ADD COLUMN checkpoint_state jsonb;
ALTER TABLE workflow_runs ADD COLUMN current_node varchar(100);
ALTER TABLE workflow_runs ADD COLUMN cost_so_far_usd double precision NOT NULL DEFAULT 0;
ALTER TABLE workflow_runs ADD COLUMN tool_call_count integer NOT NULL DEFAULT 0;

-- approvals: static risk tier, set once at approval_gate_node creation time from the
-- triggering tool's registered risk_level. Workflow 10's frontend already expects a
-- 🔴/🟡/🟢 badge; nothing has populated it until now.
ALTER TABLE approvals ADD COLUMN risk_level varchar(50);

-- organizations: the kill switch. Blocks new POST .../run calls only (checked at that
-- endpoint and at POST /approvals/{id}/approve) — does not force-cancel runs already
-- in flight, a deliberate choice to avoid leaving a tool call half-executed.
ALTER TABLE organizations ADD COLUMN agents_paused boolean NOT NULL DEFAULT false;
