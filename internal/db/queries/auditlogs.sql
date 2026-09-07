-- Workflow 17 (view audit logs). audit_logs itself has existed since
-- migration 000001 and has been written to since workflows 9/10/12 - this
-- is the first query that ever reads it back.

-- name: ListAuditLogsPage :many
-- Cursor-based (created_at, id) pagination, newest first - a plain
-- LIMIT/OFFSET would skip/repeat rows if new entries land between page
-- fetches (a real, ongoing concern here: agents write audit_logs
-- continuously). Every filter is optional (sqlc.narg + "IS NULL OR ..."),
-- matching this codebase's existing pattern (see documents.sql's
-- ListSearchableDocumentIDs).
--
-- actor_name resolves against whichever of users/agents actually owns
-- actor_id for that row's actor_type, falling back to the literal
-- 'System' when neither join matches (no current writer sets
-- actor_type='system', but it's a valid value per the plan). The fallback
-- is baked into SQL, not left to the Go handler: sqlc infers COALESCE's
-- result nullability from its *last* argument's column, which would make
-- actor_name a non-nullable Go string even though it's genuinely
-- NULL-able here - the first real 'system' row would crash pgx's scan.
SELECT
    al.id, al.created_at, al.actor_type, al.actor_id,
    COALESCE(u.full_name, u.email, a.name, 'System') AS actor_name,
    al.action, al.resource_type, al.resource_id, al.status
FROM audit_logs al
LEFT JOIN users u ON al.actor_type = 'user' AND u.id = al.actor_id AND u.org_id = al.org_id
LEFT JOIN agents a ON al.actor_type = 'agent' AND a.id = al.actor_id AND a.org_id = al.org_id
WHERE al.org_id = sqlc.arg(org_id)
  AND (sqlc.narg(actor_type)::varchar IS NULL OR al.actor_type = sqlc.narg(actor_type))
  AND (sqlc.narg(action_prefix)::varchar IS NULL OR al.action LIKE sqlc.narg(action_prefix)::varchar || '%')
  AND (sqlc.narg(status)::varchar IS NULL OR al.status = sqlc.narg(status))
  AND (sqlc.narg(date_from)::timestamptz IS NULL OR al.created_at >= sqlc.narg(date_from))
  AND (sqlc.narg(date_to)::timestamptz IS NULL OR al.created_at <= sqlc.narg(date_to))
  AND (
    sqlc.narg(cursor_created_at)::timestamptz IS NULL
    OR (al.created_at, al.id) < (sqlc.narg(cursor_created_at)::timestamptz, sqlc.narg(cursor_id)::uuid)
  )
ORDER BY al.created_at DESC, al.id DESC
LIMIT sqlc.arg(page_limit);
