-- Written from internal/core/graph/observability.go — every tool call
-- and every LLM call the executor makes writes one row to each of these,
-- closing the "nothing existing to reuse" gap flagged in the Workflow 9
-- harness planning notes. Real per-token dollar pricing is workflow 11's
-- job (see WORKFLOW_PLAN_GO.md) — estimated_cost_usd is written as 0 for
-- now; token counts themselves are real and useful on their own.

-- name: InsertAuditLog :exec
INSERT INTO audit_logs (org_id, actor_id, actor_type, action, resource_type, resource_id, status, metadata_info)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: InsertCostLedgerEntry :exec
INSERT INTO cost_ledger (org_id, run_id, agent_id, cost_type, provider, model, input_tokens, output_tokens, cached_tokens, estimated_cost_usd)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);
