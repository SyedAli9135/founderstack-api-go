-- Workflow 14 (token usage & analytics). All read-only, all against
-- app_user/tenant.WithTx like every other tenant-scoped query in this
-- codebase — RLS already scopes these by org, the explicit org_id
-- parameter matches this codebase's existing belt-and-suspenders
-- convention (see e.g. workflows.sql's ListWorkflows).

-- name: GetCostUsageSince :one
-- Monthly aggregate for GET /settings/api-key/usage and the headline
-- figures on GET /billing/usage.
SELECT
    COALESCE(SUM(input_tokens), 0)::bigint AS input_tokens,
    COALESCE(SUM(output_tokens), 0)::bigint AS output_tokens,
    COALESCE(SUM(cached_tokens), 0)::bigint AS cached_tokens,
    COALESCE(SUM(thinking_tokens), 0)::bigint AS thinking_tokens,
    COALESCE(SUM(estimated_cost_usd), 0)::double precision AS total_estimated_usd
FROM cost_ledger
WHERE org_id = $1 AND created_at >= $2;

-- name: GetDailyCostUsage :many
-- Daily breakdown (last 30 days) for GET /billing/usage's trend chart —
-- one row per day that actually had activity, not a zero-filled series
-- for every calendar day (the handler fills gaps itself, since a query
-- can't easily manufacture rows for days with zero cost_ledger activity).
SELECT
    date_trunc('day', created_at)::date AS day,
    COALESCE(SUM(input_tokens), 0)::bigint AS input_tokens,
    COALESCE(SUM(output_tokens), 0)::bigint AS output_tokens,
    COALESCE(SUM(cached_tokens), 0)::bigint AS cached_tokens,
    COALESCE(SUM(estimated_cost_usd), 0)::double precision AS estimated_cost_usd
FROM cost_ledger
WHERE org_id = $1 AND created_at >= $2
GROUP BY day
ORDER BY day;

-- name: GetAgentCostShare :many
-- Per-agent cost share (last 30 days) for GET /billing/usage's bar chart.
-- agent_id is nullable on cost_ledger (a run's tool-call/llm cost rows
-- always set it, but nothing else does yet) — LEFT JOIN keeps those rows
-- visible under a synthetic "Unattributed" bucket rather than silently
-- dropping real spend from the total.
SELECT
    COALESCE(a.name, 'Unattributed') AS agent_name,
    COALESCE(SUM(cl.estimated_cost_usd), 0)::double precision AS total_cost_usd
FROM cost_ledger cl
LEFT JOIN agents a ON a.id = cl.agent_id
WHERE cl.org_id = $1 AND cl.created_at >= $2
GROUP BY a.name
ORDER BY total_cost_usd DESC;

-- name: CountCostLedger :one
SELECT count(*) FROM cost_ledger WHERE org_id = $1;

-- name: ListCostLedgerPage :many
-- Paginated GET /billing/ledger.
SELECT id, created_at, cost_type, provider, model, run_id, agent_id,
       input_tokens, output_tokens, cached_tokens, thinking_tokens, estimated_cost_usd
FROM cost_ledger
WHERE org_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: GetAgentPerformance :many
-- GET /analytics/agent-performance. success_count/failure_count are
-- separate columns (not one status column) so the handler never has to
-- special-case pending/running/awaiting_approval rows to compute a rate.
SELECT
    a.id AS agent_id,
    a.name AS agent_name,
    count(wr.id)::bigint AS total_runs,
    count(*) FILTER (WHERE wr.status = 'completed')::bigint AS success_count,
    count(*) FILTER (WHERE wr.status = 'failed')::bigint AS failure_count,
    COALESCE(AVG(wr.duration_ms) FILTER (WHERE wr.duration_ms IS NOT NULL), 0)::double precision AS avg_duration_ms,
    COALESCE(AVG(wr.cost_so_far_usd), 0)::double precision AS avg_cost_usd
FROM agents a
JOIN workflows w ON w.agent_id = a.id
JOIN workflow_runs wr ON wr.workflow_id = w.id
WHERE a.org_id = $1
GROUP BY a.id, a.name
ORDER BY total_runs DESC;

-- name: GetRagQualityStats :one
-- GET /analytics/rag-quality. Sourced from audit_logs' rag.search rows —
-- the only place avg_rerank_score/from_cache/result_count are recorded at
-- all (see internal/api/documents/handler.go's auditSearch); a search
-- with 0 results (the ACL-empty short-circuit) contributes a 0 to
-- avg_rerank_score/avg_chunks_retrieved, which is the correct average,
-- not a value worth excluding.
SELECT
    COALESCE(AVG((metadata_info->>'avg_rerank_score')::double precision), 0)::double precision AS avg_rerank_score,
    COALESCE(AVG((metadata_info->>'result_count')::double precision), 0)::double precision AS avg_chunks_retrieved,
    COALESCE(
        AVG((metadata_info->>'from_cache')::boolean::int)::double precision,
        0
    )::double precision AS cache_hit_rate,
    count(*)::bigint AS total_searches
FROM audit_logs
WHERE org_id = $1 AND action = 'rag.search' AND created_at >= $2;
