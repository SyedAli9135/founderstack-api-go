-- name: GetDigestSettings :one
SELECT digest_enabled, digest_send_hour, digest_timezone FROM organizations WHERE id = $1;

-- name: GetOrgNameAndTimezone :one
-- Backs "send test email" -- BuildPayload needs both, and neither
-- GetDigestSettings nor auth.sql's GetActiveOrganizationByID returns the
-- pair together.
SELECT name, digest_timezone FROM organizations WHERE id = $1;

-- name: GetUserEmailByID :one
SELECT email FROM users WHERE id = $1;

-- name: UpdateDigestSettings :exec
UPDATE organizations
SET digest_enabled = $2, digest_send_hour = $3, digest_timezone = $4
WHERE id = $1;

-- name: DisableDigestForOrg :exec
-- Backs the no-login unsubscribe link — deliberately only ever turns the
-- digest off, never on, so a leaked/replayed token can't be used to
-- re-enable something a founder actively disabled some other way.
UPDATE organizations SET digest_enabled = false WHERE id = $1;

-- name: ListOrgsDueForDigest :many
-- Cross-tenant scan (system pool, BYPASSRLS) -- an org is due when its
-- local hour matches digest_send_hour, it hasn't already gotten today's
-- digest in its own timezone yet, and it's had some activity in the last
-- 7 days (skips ghost emails to churned/empty orgs, per spec).
SELECT id, name, digest_timezone
FROM organizations
WHERE digest_enabled = true
  AND is_active = true
  AND EXTRACT(HOUR FROM now() AT TIME ZONE digest_timezone)::int = digest_send_hour
  AND (
    digest_last_sent_at IS NULL
    OR (digest_last_sent_at AT TIME ZONE digest_timezone)::date < (now() AT TIME ZONE digest_timezone)::date
  )
  AND EXISTS (
    SELECT 1 FROM workflow_runs wr
    WHERE wr.org_id = organizations.id AND wr.created_at >= now() - interval '7 days'
  );

-- name: MarkDigestSent :exec
UPDATE organizations SET digest_last_sent_at = now() WHERE id = $1;

-- name: ListDigestRecipients :many
-- Owners/admins only, not every member -- same admin-role check
-- (role IN ('owner','admin')) already used by org.handler.go/clerk
-- webhook, reused here rather than invented fresh.
SELECT email FROM users
WHERE org_id = $1 AND is_active = true AND role IN ('owner', 'admin');

-- name: GetDigestRunStats :one
-- "Yesterday" is the previous calendar day in the org's own
-- digest_timezone, not UTC -- matches what ListOrgsDueForDigest just
-- fired on. parent_run_id IS NULL excludes workflow 18 specialist
-- sub-runs from the count, same reasoning as FinalizeRunHoursSaved: only
-- an orchestrator's own run represents one end-to-end task a founder
-- would otherwise have done by hand.
SELECT
    COUNT(*)::bigint AS total_runs,
    COUNT(*) FILTER (WHERE status = 'completed')::bigint AS successful_runs,
    COUNT(*) FILTER (WHERE status = 'failed')::bigint AS failed_runs,
    COALESCE(SUM(hours_saved), 0)::double precision AS hours_saved
FROM workflow_runs
WHERE org_id = $1
  AND parent_run_id IS NULL
  AND (created_at AT TIME ZONE sqlc.arg(timezone)::text)::date
      = (now() AT TIME ZONE sqlc.arg(timezone)::text)::date - 1;

-- name: GetDigestCostUSD :one
SELECT COALESCE(SUM(estimated_cost_usd), 0)::double precision AS cost_usd
FROM cost_ledger
WHERE org_id = $1
  AND (created_at AT TIME ZONE sqlc.arg(timezone)::text)::date
      = (now() AT TIME ZONE sqlc.arg(timezone)::text)::date - 1;

-- name: GetDigestPendingApprovalsCount :one
-- Current state, not time-boxed to yesterday -- a founder needs to know
-- what's waiting on them right now, not just what queued up yesterday.
SELECT COUNT(*)::bigint FROM approvals WHERE org_id = $1 AND status = 'pending';

-- name: GetDigestTopAgent :one
-- COALESCE(wr.agent_id, w.agent_id): same "derive from workflow unless a
-- team sub-run overrides it directly" convention as GetRunAgentID.
-- Returns pgx.ErrNoRows when nothing ran yesterday -- callers treat that
-- as "no top agent", not an error.
SELECT a.name AS agent_name, COUNT(*)::bigint AS run_count
FROM workflow_runs wr
JOIN workflows w ON w.id = wr.workflow_id
JOIN agents a ON a.id = COALESCE(wr.agent_id, w.agent_id)
WHERE wr.org_id = $1
  AND wr.parent_run_id IS NULL
  AND (wr.created_at AT TIME ZONE sqlc.arg(timezone)::text)::date
      = (now() AT TIME ZONE sqlc.arg(timezone)::text)::date - 1
GROUP BY a.name
ORDER BY run_count DESC
LIMIT 1;
