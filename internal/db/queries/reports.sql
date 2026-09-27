-- Workflow 23 (Client-Facing Reports & Sharing).
--
-- Report *data* is aggregated under tenant.WithTx on the reported workspace
-- (app_user + RLS), so a report can only ever contain that tenant's rows.
-- Windows are half-open [from, to) timestamps the handler computes from the
-- report's date range in the workspace's own timezone.
-- parent_run_id IS NULL counts each end-to-end task once (a team run's
-- specialist sub-runs aren't separate outcomes), same as the digest.

-- name: GetReportRunSummary :one
SELECT
    count(*) FILTER (WHERE status = 'completed')::bigint AS runs_completed,
    count(*) FILTER (WHERE status = 'failed')::bigint AS runs_failed,
    count(*)::bigint AS runs_total,
    COALESCE(SUM(hours_saved), 0)::double precision AS hours_saved,
    count(DISTINCT workflow_id)::bigint AS workflows_active
FROM workflow_runs
WHERE org_id = sqlc.arg(org_id) AND parent_run_id IS NULL AND created_at >= sqlc.arg(window_start) AND created_at < sqlc.arg(window_end);

-- name: ListReportWorkflowOutcomes :many
SELECT w.name,
       count(*) FILTER (WHERE r.status = 'completed')::bigint AS runs_completed,
       count(*)::bigint AS runs_total,
       COALESCE(SUM(r.hours_saved), 0)::double precision AS hours_saved
FROM workflow_runs r
JOIN workflows w ON w.id = r.workflow_id
WHERE r.org_id = sqlc.arg(org_id) AND r.parent_run_id IS NULL AND r.created_at >= sqlc.arg(window_start) AND r.created_at < sqlc.arg(window_end)
GROUP BY w.id, w.name
ORDER BY hours_saved DESC, w.name;

-- name: GetReportCostTotals :one
SELECT
    COALESCE(SUM(estimated_cost_usd), 0)::double precision AS total_cost_usd,
    COALESCE(SUM(input_tokens), 0)::bigint AS input_tokens,
    COALESCE(SUM(output_tokens), 0)::bigint AS output_tokens,
    COALESCE(SUM(cached_tokens), 0)::bigint AS cached_tokens
FROM cost_ledger
WHERE org_id = sqlc.arg(org_id) AND created_at >= sqlc.arg(window_start) AND created_at < sqlc.arg(window_end);

-- name: ListReportCostByAgent :many
SELECT COALESCE(a.name, 'Unattributed') AS agent_name,
       COALESCE(SUM(cl.estimated_cost_usd), 0)::double precision AS cost_usd
FROM cost_ledger cl
LEFT JOIN agents a ON a.id = cl.agent_id
WHERE cl.org_id = sqlc.arg(org_id) AND cl.created_at >= sqlc.arg(window_start) AND cl.created_at < sqlc.arg(window_end)
GROUP BY a.name
ORDER BY cost_usd DESC;

-- name: ListReportRuns :many
-- cost_usd includes a team run's specialist sub-runs, so one row carries the
-- whole task's cost.
SELECT r.id, w.name AS workflow_name, r.status, r.created_at, r.started_at, r.completed_at,
       r.hours_saved,
       (SELECT COALESCE(SUM(cl.estimated_cost_usd), 0) FROM cost_ledger cl
          WHERE cl.run_id = r.id
             OR cl.run_id IN (SELECT c.id FROM workflow_runs c WHERE c.parent_run_id = r.id))::double precision AS cost_usd
FROM workflow_runs r
JOIN workflows w ON w.id = r.workflow_id
WHERE r.org_id = sqlc.arg(org_id) AND r.parent_run_id IS NULL AND r.created_at >= sqlc.arg(window_start) AND r.created_at < sqlc.arg(window_end)
ORDER BY r.created_at DESC
LIMIT sqlc.arg(row_limit);

-- name: InsertClientReport :one
INSERT INTO client_reports (org_id, created_by_user_id, title, date_from, date_to, timezone,
                            visible_sections, snapshot, share_token, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id, created_at;

-- name: RevokeClientReport :execrows
UPDATE client_reports SET is_revoked = true WHERE id = $1 AND org_id = $2 AND is_revoked = false;

-- System pool (app_system) below.

-- name: GetReportOrgInfo :one
-- prepared_by: the practice a client workspace belongs to, else the org itself.
SELECT o.name, o.digest_timezone,
       COALESCE(p.name, o.name)::text AS prepared_by
FROM organizations o
LEFT JOIN organizations p ON p.id = o.parent_practice_id
WHERE o.id = $1 AND o.is_active = true;

-- name: ListClientReportsForUser :many
-- Operator-side list: only reports of workspaces where the caller is an
-- active owner/admin. target_org_id NULL lists across all of them.
SELECT r.id, r.org_id, o.name AS org_name, r.title, r.date_from, r.date_to, r.visible_sections,
       r.share_token, r.expires_at, r.view_count, r.last_viewed_at, r.is_revoked, r.created_at
FROM client_reports r
JOIN organizations o ON o.id = r.org_id AND o.is_active = true
JOIN users u ON u.org_id = r.org_id AND u.clerk_user_id = sqlc.arg(clerk_user_id)
            AND u.is_active = true AND u.role IN ('owner', 'admin')
WHERE sqlc.narg(target_org_id)::uuid IS NULL OR r.org_id = sqlc.narg(target_org_id)::uuid
ORDER BY r.created_at DESC;

-- name: GetClientReportForUser :one
SELECT r.id, r.org_id, o.name AS org_name, r.title, r.date_from, r.date_to, r.timezone,
       r.visible_sections, r.snapshot, r.share_token, r.expires_at, r.view_count,
       r.last_viewed_at, r.is_revoked, r.created_at
FROM client_reports r
JOIN organizations o ON o.id = r.org_id AND o.is_active = true
JOIN users u ON u.org_id = r.org_id AND u.clerk_user_id = $2
            AND u.is_active = true AND u.role IN ('owner', 'admin')
WHERE r.id = $1;

-- name: GetClientReportByToken :one
-- Public, unauthenticated lookup. The handler treats missing, revoked,
-- expired, and deactivated-workspace identically.
SELECT r.id, r.title, r.date_from, r.date_to, r.timezone, r.visible_sections, r.snapshot,
       r.expires_at, r.is_revoked, o.is_active AS org_active
FROM client_reports r
JOIN organizations o ON o.id = r.org_id
WHERE r.share_token = $1;

-- name: RecordClientReportView :exec
UPDATE client_reports SET view_count = view_count + 1, last_viewed_at = now() WHERE id = $1;
